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

package openstack

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/startstop"
	"github.com/gophercloud/gophercloud/openstack/containerinfra/v1/clusters"
	"github.com/gophercloud/gophercloud/openstack/orchestration/v1/stackresources"
	"github.com/gophercloud/gophercloud/openstack/orchestration/v1/stacks"
	uuid "github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/healthcheck"
)

const (
	ProviderName                  = "openstack"
	clusterStatusUpdateInProgress = "UPDATE_IN_PROGRESS"
	clusterStatusUpdateComplete   = "UPDATE_COMPLETE"
	clusterStatusUpdateFailed     = "UPDATE_FAILED"
	stackStatusUpdateFailed       = "UPDATE_FAILED"
	stackStatusUpdateInProgress   = "UPDATE_IN_PROGRESS"
	stackStatusUpdateComplete     = "UPDATE_COMPLETE"
)

var statusesPreventingRepair = sets.NewString(
	stackStatusUpdateInProgress,
	stackStatusUpdateFailed,
)

// OpenStack is an implementation of cloud provider Interface for OpenStack.
type OpenStackCloudProvider struct {
	Nova                 *gophercloud.ServiceClient
	Heat                 *gophercloud.ServiceClient
	Magnum               *gophercloud.ServiceClient
	Config               config.Config
	ResourceStackMapping map[string]ResourceStackRelationship
}

type ResourceStackRelationship struct {
	ResourceID   string
	ResourceName string
	StackID      string
	StackName    string
}

func (provider OpenStackCloudProvider) GetName() string {
	return ProviderName
}

// getStackName finds the name of a stack matching a given ID.
func (provider *OpenStackCloudProvider) getStackName(stackID string) (string, error) {
	stack, err := stacks.Find(provider.Heat, stackID).Extract()
	if err != nil {
		return "", fmt.Errorf("could not find stack with ID %s: %v", stackID, err)
	}
	log.Debugf("for stack ID %s, stack name is %s", stackID, stack.Name)
	return stack.Name, nil
}

// getAllStackResourceMapping returns all mapping relationships including
// masters and minions(workers). The key in the map is the server/instance ID
// in Nova and the value is the resource ID and name of the server, and the
// parent stack ID and name.
func (provider *OpenStackCloudProvider) getAllStackResourceMapping(stackName, stackID string) (m map[string]ResourceStackRelationship, err error) {
	if provider.ResourceStackMapping != nil {
		return provider.ResourceStackMapping, nil
	}
	mapping := make(map[string]ResourceStackRelationship)
	serverPages, err := stackresources.List(provider.Heat, stackName, stackID, stackresources.ListOpts{Depth: 2}).AllPages()
	if err != nil {
		return m, fmt.Errorf("could not list servers pages of given stack resource: %v", err)
	}
	serverResources, err := stackresources.ExtractResources(serverPages)
	if err != nil {
		return m, fmt.Errorf("could not list servers resource: %v", err)
	}
	for _, sr := range serverResources {
		if sr.Type != "OS::Nova::Server" {
			continue
		}
		for _, link := range sr.Links {
			if link.Rel == "self" {
				continue
			}
			paths := strings.Split(link.Href, "/")
			if len(paths) > 2 {
				m := ResourceStackRelationship{
					ResourceID:   sr.PhysicalID,
					ResourceName: sr.Name,
					StackID:      paths[len(paths)-1],
					StackName:    paths[len(paths)-2],
				}
				log.Infof("resource ID: %s, resource name: %s, parent stack ID: %s, parent stack name: %s", sr.PhysicalID, sr.Name, paths[len(paths)-1], paths[len(paths)-2])
				mapping[sr.PhysicalID] = m
			}
		}
	}
	provider.ResourceStackMapping = mapping
	return provider.ResourceStackMapping, nil
}

// Repair soft deletes the VMs, marks the heat resource "unhealthy" then trigger Heat stack update in order to rebuild
// the VMs. The information this function needs:
// - Nova VM IDs
// - Heat stack ID and resource ID.
func (provider OpenStackCloudProvider) Repair(nodes []healthcheck.NodeInfo) error {
	cluster, err := clusters.Get(provider.Magnum, provider.Config.ClusterName).Extract()
	if err != nil {
		return fmt.Errorf("failed to get the cluster %s", provider.Config.ClusterName)
	}
	clusterStackName, err := provider.getStackName(cluster.StackID)
	if err != nil {
		return fmt.Errorf("failed to get the Heat stack from cluster %s", provider.Config.ClusterName)
	}
	allMapping, err := provider.getAllStackResourceMapping(clusterStackName, cluster.StackID)
	if err != nil {
		return fmt.Errorf("failed to get the resource stack mapping for cluster %s", provider.Config.ClusterName)
	}

	for _, n := range nodes {
		id := uuid.Parse(n.KubeNode.Status.NodeInfo.MachineID)
		if id == nil {
			log.Warningf("failed to get the correct server ID for server %s", n.KubeNode.Name)
			continue
		}
		serverID := id.String()
		err = startstop.Stop(provider.Nova, serverID).ExtractErr()
		if err != nil {
			log.Warningf("failed to shutdown server server %s", serverID)
		}
		log.Infof("marking unhealthy on node %s", serverID)
		opts := stackresources.MarkUnhealthyOpts{
			MarkUnhealthy:        true,
			ResourceStatusReason: "Mark resource unhealthy by autohealing controller",
		}
		err = stackresources.MarkUnhealthy(provider.Heat, allMapping[serverID].StackName, allMapping[serverID].StackID, allMapping[serverID].ResourceID, opts).ExtractErr()
		if err != nil {
			log.Errorf("failed to mark resource %s unhealthy", serverID)
		}
	}
	log.Debugf("start to do Heat stack update to rebuild resources.")
	err = stacks.UpdatePatch(provider.Heat, clusterStackName, cluster.StackID, stacks.UpdateOpts{}).ExtractErr()
	if err != nil {
		log.Errorf(err.Error())
		return fmt.Errorf("could not update stack to rebuild resources: %v", err)
	}
	return nil
}

// Enabled decides if the repair should be triggered.
// There are  two conditions that we disable the repair:
// - The cluster admin disables the auto healing via OpenStack API.
// - The Magnum cluster is not in stable status.
func (provider OpenStackCloudProvider) Enabled() bool {
	cluster, err := clusters.Get(provider.Magnum, provider.Config.ClusterName).Extract()
	if err != nil {
		log.Warningf("failed to get the cluster %s.", provider.Config.ClusterName)
		return false
	}
	autoHealingEnabled, err := strconv.ParseBool(cluster.Labels["auto_healing_enabled"])
	if err != nil {
		log.Warningf("failed to get the auto_healing_enabled label from cluster.")
		return false
	}
	if !autoHealingEnabled {
		log.Infof("auto healing on current cluster is disabled.")
		return false
	}

	clusterStackName, err := provider.getStackName(cluster.StackID)
	stack, err := stacks.Get(provider.Heat, clusterStackName, cluster.StackID).Extract()
	if err != nil {
		log.Warningf("failed to get stack %s", cluster.StackID)
	}
	if statusesPreventingRepair.Has(stack.Status) {
		log.Infof("current stack is in status %s", stack.Status)
		return false
	}

	return true
}
