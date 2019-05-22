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
	"regexp"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/startstop"
	"github.com/gophercloud/gophercloud/openstack/containerinfra/v1/clusters"
	"github.com/gophercloud/gophercloud/openstack/orchestration/v1/stackresources"
	"github.com/gophercloud/gophercloud/openstack/orchestration/v1/stacks"
	uuid "github.com/satori/go.uuid"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/healthcheck"
	"k8s.io/klog"
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
		return "", fmt.Errorf("Could not find stack with ID %s: %v", stackID, err)
	}
	klog.V(0).Infof("For stack ID %s, stack name is %s", stackID, stack.Name)
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
	stackPages, err := stacks.List(provider.Heat, stacks.ListOpts{ShowNested: true}).AllPages()
	if err != nil {
		return m, fmt.Errorf("Could not list stacks: %v", err)
	}
	allStacks, err := stacks.ExtractStacks(stackPages)

	serverPages, err := stackresources.List(provider.Heat, stackName, stackID, stackresources.ListOpts{Depth: 2}).AllPages()
	if err != nil {
		return m, fmt.Errorf("Could not list servers pages of given stack resource: %v", err)
	}
	serverResources, err := stackresources.ExtractResources(serverPages)
	if err != nil {
		return m, fmt.Errorf("Could not list servers resource: %v", err)
	}
	for _, sr := range serverResources {
		if sr.Type != "OS::Nova::Server" {
			continue
		}
		for _, sp := range allStacks {
			matched, err := regexp.MatchString(stackName+`-(kube_masters|kube_minions)-\S+-\d+-\S+$`, sp.Name)
			if err != nil {
				klog.Warningln("Failed to find matched stack for %s", sp.Name)
			}
			if matched && strings.Contains(sr.Links[0].Href, sp.Name) {
				m := ResourceStackRelationship{
					ResourceID:   sr.PhysicalID,
					ResourceName: sr.Name,
					StackID:      sp.ID,
					StackName:    sp.Name,
				}
				klog.V(8).Infof("Resource ID: %s, Resource name: %s, Parent ID: %s, Parent name: %s", sr.PhysicalID, sr.Name, sp.ID, sp.Name)
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
	// Get Heat stack ID related to the Magnum cluster.
	cluster, err := clusters.Get(provider.Magnum, provider.Config.ClusterName).Extract()
	clusterStackName, err := provider.getStackName(cluster.StackID)
	allMapping, err := provider.getAllStackResourceMapping(clusterStackName, cluster.StackID)

	for _, n := range nodes {
		id, err := uuid.FromString(n.KubeNode.Status.NodeInfo.MachineID)
		if err != nil {
			klog.Warningln("Failed to get the correct server ID for server %s.", n.KubeNode.Name)
			continue
		}
		serverID := id.String()
		err = startstop.Stop(provider.Nova, serverID).ExtractErr()
		if err != nil {
			klog.Warningln("Failed to shutdown server server %s.", serverID)
		}
		klog.V(0).Infof("Marking unhealthy on node %s", serverID)
		opts := stackresources.MarkUnhealthyOpts{
			MarkUnhealthy:        true,
			ResourceStatusReason: "Mark resource unhealthy by autohealing controller",
		}
		err = stackresources.MarkUnhealthy(provider.Heat, allMapping[serverID].StackName, allMapping[serverID].StackID, allMapping[serverID].ResourceID, opts).ExtractErr()
		if err != nil {
			klog.Errorf("Failed to mark resource %s unhealthy", serverID)
		}
	}

	err = stacks.Update(provider.Heat, clusterStackName, cluster.StackID, nil).ExtractErr()
	if err != nil {
		return fmt.Errorf("Could not update stack to rebuild resources: %v", err)
	}
	return nil
}

// Enabled decides if the repair should be triggered.
// There are  two conditions that we disable the repair:
// - The cluster admin disables the auto healing via OpenStack API.
// - The Magnum cluster is not in stable status.
func (provider OpenStackCloudProvider) Enabled() bool {
	cluster, err := clusters.Get(provider.Magnum, provider.Config.ClusterName).Extract()
	autoHealingEnabled, err := strconv.ParseBool(cluster.Labels["auto_healing_enabled"])
	if err != nil {
		klog.Warningln("Failed to get the auto_healing_enabled label from cluster.")
		return false
	}
	if !autoHealingEnabled {
		klog.V(0).Infoln("Auto healing on current cluster is disabled.")
		return false
	}

	clusterStackName, err := provider.getStackName(cluster.StackID)
	stack, err := stacks.Get(provider.Heat, clusterStackName, cluster.StackID).Extract()
	if err != nil {
		klog.Warningln("Failed to get stack %s", cluster.StackID)
	}
	if statusesPreventingRepair.Has(stack.Status) {
		klog.V(0).Infoln("Current stack is in status %s", stack.Status)
		return false
	}

	return true
}
