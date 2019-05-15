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
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"k8s.io/cloud-provider-openstack/pkg/autohealing/cloudprovider"
	_ "k8s.io/cloud-provider-openstack/pkg/autohealing/cloudprovider/register"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
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

type NodeInfo struct {
	kubeNode           apiv1.Node
	lastTransitionTime time.Time
}

// Controller ...
type Controller struct {
	provider   cloudprovider.CloudProvider
	recorder   record.EventRecorder
	kubeClient kubernetes.Interface
	config     config.Config
}

func createApiserverClient(apiserverHost string, kubeConfig string) (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.BuildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	log.Debug("creating kubernetes API client")

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	v, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}
	log.WithFields(log.Fields{
		"version": fmt.Sprintf("v%v.%v", v.Major, v.Minor),
	}).Debug("kubernetes API client created")

	return client, nil
}

// NewController creates a new autohealer controller.
func NewController(conf config.Config) *Controller {
	// initialize cloud provider
	provider, err := cloudprovider.GetCloudProvider(conf.CloudProvider, conf)
	if err != nil {
		log.Fatalf("Failed to get the cloud provider %s: %v", conf.CloudProvider, err)
	}

	log.Infof("Using cloud provider: %s", provider.GetName())

	// initialize k8s client
	kubeClient, err := createApiserverClient(conf.Kubernetes.ApiserverHost, conf.Kubernetes.KubeConfig)
	if err != nil {
		log.WithFields(log.Fields{
			"api_server":  conf.Kubernetes.ApiserverHost,
			"kuberconfig": conf.Kubernetes.KubeConfig,
			"error":       err,
		}).Fatal("failed to initialize kubernetes client")
	}

	// event
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typev1.EventSinkImpl{
		Interface: kubeClient.CoreV1().Events(""),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: "openstack-ingress-controller"})

	controller := &Controller{
		config:     conf,
		recorder:   recorder,
		provider:   provider,
		kubeClient: kubeClient,
	}

	return controller
}

func (c *Controller) startMasterMonitor(wg *sync.WaitGroup) {
	log.Debug("Starting to check master nodes.")
	defer wg.Done()

	log.Debug("Finished checking master nodes.")
}

func (c *Controller) getUnhealthyWorkerNodes() ([]NodeInfo, error) {
	var nodes []NodeInfo

	nodeList, err := c.kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, node := range nodeList.Items {
		// Ignore master nodes
		if _, hasMasterRoleLabel := node.Labels[LabelNodeRoleMaster]; hasMasterRoleLabel {
			continue
		}

		// If we have no info, don't accept
		if len(node.Status.Conditions) == 0 {
			continue
		}

		for _, cond := range node.Status.Conditions {
			if cond.Type == apiv1.NodeReady && cond.Status != apiv1.ConditionTrue {
				nodes = append(nodes, NodeInfo{kubeNode: node, lastTransitionTime: cond.LastTransitionTime.Time})
			}
		}
	}

	return nodes, nil
}

func (c *Controller) startWorkerMonitor(wg *sync.WaitGroup) {
	log.Debug("Starting to check worker nodes.")
	defer wg.Done()

	// Get all the unhealthy worker nodes.
	unhealthyNodes, err := c.getUnhealthyWorkerNodes()
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Warn("Failed to get unhealthy worker nodes.")
		return
	}

	namesNeedRepair := sets.NewString()
	var nodesNeedRepair []apiv1.Node

	for _, node := range unhealthyNodes {
		unhealthyDuration := time.Now().Sub(node.lastTransitionTime)

		if int(unhealthyDuration.Seconds()) >= c.config.WorkerUnhealthyDuration {
			nodesNeedRepair = append(nodesNeedRepair, node.kubeNode)
			namesNeedRepair.Insert(node.kubeNode.Name)
		}
	}

	// Trigger unhealthy nodes repair.
	if len(nodesNeedRepair) > 0 {
		if !c.provider.Enabled() {
			// The cloud provider doesn't allow to trigger node repair.
			log.WithFields(log.Fields{"nodes": namesNeedRepair}).Info("Auto healing is ignored.")
		} else {
			log.WithFields(log.Fields{"nodes": namesNeedRepair}).Info("Starting to repair worker nodes.")

			if !c.config.DryRun {
				c.provider.Repair(nodesNeedRepair)
			}
		}
	}

	log.Debug("Finished checking worker nodes.")
}

// Start starts the autohealing controller.
func (c *Controller) Start() {
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
