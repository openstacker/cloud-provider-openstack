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
	"github.com/gophercloud/gophercloud"

	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/healthcheck"
)

const (
	ProviderName = "openstack"
)

// OpenStack is an implementation of cloud provider Interface for OpenStack.
type OpenStackCloudProvider struct {
	Nova   *gophercloud.ServiceClient
	Heat   *gophercloud.ServiceClient
	Magnum *gophercloud.ServiceClient
	Config config.Config
}

func (provider OpenStackCloudProvider) GetName() string {
	return ProviderName
}

// Repair soft deletes the VMs, marks the heat resource "unhealthy" then trigger Heat stack update in order to rebuild
// the VMs. The information this function needs:
// - Nova VM IDs
// - Heat stack ID and resource ID.
func (provider OpenStackCloudProvider) Repair([]healthcheck.NodeInfo) error {
	// Get Heat stack ID related to the Magnum cluster.

	return nil
}

// Enabled decides if the repair should be triggered.
// There are  two conditions that we disable the repair:
// - The cluster admin disables the auto healing via OpenStack API.
// - The Magnum cluster is not in stable status.
func (provider OpenStackCloudProvider) Enabled() bool {
	return true
}
