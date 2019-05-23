/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"os"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	log "k8s.io/klog"

	"k8s.io/cloud-provider-openstack/pkg/autohealing/cloudprovider"
	_ "k8s.io/cloud-provider-openstack/pkg/autohealing/cloudprovider/register"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/healthcheck"
)

// EventType type of event associated with an informer
type EventType string

const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6

	// CreateEvent event associated with new objects in an informer
	CreateEvent EventType = "CREATE"
	// UpdateEvent event associated with an object update in an informer
	UpdateEvent EventType = "UPDATE"
	// DeleteEvent event associated when an object is removed from an informer
	DeleteEvent EventType = "DELETE"

	// LabelNodeRoleMaster specifies that a node is a master
	// Related discussion: https://github.com/kubernetes/kubernetes/pull/39112
	LabelNodeRoleMaster = "node-role.kubernetes.io/master"
)

// Event holds the context of an event
type Event struct {
	Type EventType
	Obj  interface{}
}

// Controller ...
type Controller struct {
	provider             cloudprovider.CloudProvider
	recorder             record.EventRecorder
	kubeClient           kubernetes.Interface
	leaderElectionClient kubernetes.Interface
	config               config.Config
}

func createKubeClients(apiserverHost string, kubeConfig string) (*kubernetes.Clientset, *kubernetes.Clientset, error) {
	cfg, err := clientcmd.BuildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	log.V(4).Info("creating kubernetes API clients")

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	leaderElectionClient, err := kubernetes.NewForConfig(restclient.AddUserAgent(cfg, "leader-election"))
	if err != nil {
		return nil, nil, err
	}
	log.V(4).Info("Kubernetes API client created.")

	return client, leaderElectionClient, nil
}

// NewController creates a new autohealer controller.
func NewController(conf config.Config) *Controller {
	// initialize cloud provider
	provider, err := cloudprovider.GetCloudProvider(conf.CloudProvider, conf)
	if err != nil {
		log.Fatalf("Failed to get the cloud provider %s: %v", conf.CloudProvider, err)
	}

	log.Infof("Using cloud provider: %s", provider.GetName())

	// initialize k8s clients
	kubeClient, leaderElectionClient, err := createKubeClients(conf.Kubernetes.ApiserverHost, conf.Kubernetes.KubeConfig)
	if err != nil {
		log.Fatalf("failed to initialize kubernetes client, error: %v", err)
	}

	// event
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.V(4).Infof)
	eventBroadcaster.StartRecordingToSink(&typev1.EventSinkImpl{
		Interface: kubeClient.CoreV1().Events(""),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: "openstack-ingress-controller"})

	controller := &Controller{
		config:               conf,
		recorder:             recorder,
		provider:             provider,
		kubeClient:           kubeClient,
		leaderElectionClient: leaderElectionClient,
	}

	return controller
}

func (c *Controller) GetLeaderElectionLock() (resourcelock.Interface, error) {
	// Identity used to distinguish between multiple cloud controller manager instances
	id, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	rl, err := resourcelock.New(
		"configmaps",
		"kube-system",
		"k8s-auto-healer",
		c.leaderElectionClient.CoreV1(),
		c.leaderElectionClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: c.recorder,
		})
	if err != nil {
		return nil, err
	}

	return rl, nil
}

func (c *Controller) startMasterMonitor(wg *sync.WaitGroup) {
	log.V(4).Info("Starting to check master nodes.")
	defer wg.Done()

	log.V(4).Info("Finished checking master nodes.")
}

// getUnhealthyWorkerNodes returns the nodes that need to be repaired.
func (c *Controller) getUnhealthyWorkerNodes() ([]healthcheck.NodeInfo, error) {
	var nodes []healthcheck.NodeInfo
	var checkers []healthcheck.HealthCheck

	// Get all the valid checkers.
	for _, item := range c.config.HealthCheck.Worker {
		checker, err := healthcheck.GetHealthChecker(item.Type, item.Params)
		if err != nil {
			log.Errorf("failed to get %s type health check, error: %v", item.Type, err)
			continue
		}
		if !checker.IsWorkerSupported() {
			log.Warningf("Plugin type %s does not support worker node health check.", item.Type)
			continue
		}

		checkers = append(checkers, checker)
	}

	// If no checkers defined, skip.
	if len(checkers) == 0 {
		return nil, nil
	}

	// Get all the worker nodes.
	nodeList, err := c.kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, node := range nodeList.Items {
		if _, hasMasterRoleLabel := node.Labels[LabelNodeRoleMaster]; hasMasterRoleLabel {
			continue
		}
		if len(node.Status.Conditions) == 0 {
			continue
		}
		if time.Now().Before(node.ObjectMeta.GetCreationTimestamp().Add(c.config.RepairDelayAfterAdd)) {
			continue
		}
		nodes = append(nodes, healthcheck.NodeInfo{KubeNode: node})
	}

	// Do health check.
	failedNodes := healthcheck.CheckNodes(checkers, nodes)

	return failedNodes, nil
}

func (c *Controller) startWorkerMonitor(wg *sync.WaitGroup) {
	log.V(4).Info("Starting to check worker nodes.")
	defer wg.Done()

	// Get all the unhealthy worker nodes.
	unhealthyNodes, err := c.getUnhealthyWorkerNodes()
	if err != nil {
		log.Errorf("Failed to get unhealthy worker nodes, error: %v", err)
		return
	}

	unhealthyNodeNames := sets.NewString()
	for _, n := range unhealthyNodes {
		unhealthyNodeNames.Insert(n.KubeNode.Name)
	}

	// Trigger unhealthy nodes repair.
	if len(unhealthyNodes) > 0 {
		if !c.provider.Enabled() {
			// The cloud provider doesn't allow to trigger node repair.
			log.Infof("Auto healing is ignored for nodes %s", unhealthyNodeNames.List())
		} else {
			log.Infof("Starting to repair worker nodes %s", unhealthyNodeNames.List())

			if !c.config.DryRun {
				// Cordon the nodes before repair.
				for _, node := range unhealthyNodes {
					newNode := node.KubeNode.DeepCopy()
					newNode.Spec.Unschedulable = true
					if _, err = c.kubeClient.CoreV1().Nodes().Update(newNode); err != nil {
						log.Errorf("Failed to cordon worker node %s", node.KubeNode.Name)
					} else {
						log.Infof("Worker node %s is cordoned", node.KubeNode.Name)
					}
				}

				c.provider.Repair(unhealthyNodes)
			}
		}
	}

	log.V(4).Info("Finished checking worker nodes.")
}

// Start starts the autohealing controller.
func (c *Controller) Start(ctx context.Context) {
	log.Info("Starting autohealing controller")

	ticker := time.NewTicker(time.Duration(c.config.MonitorInterval) * time.Second)
	defer ticker.Stop()

	var wg sync.WaitGroup
	for {
		select {
		case <-ticker.C:
			if c.config.MasterMonitorEnabled {
				wg.Add(1)
				go c.startMasterMonitor(&wg)
			}
			if c.config.WorkerMonitorEnabled {
				wg.Add(1)
				go c.startWorkerMonitor(&wg)
			}

			wg.Wait()
		}
	}
}
