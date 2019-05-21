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

package healthcheck

import (
	"fmt"
	"time"

	"github.com/mitchellh/mapstructure"

	"k8s.io/cloud-provider-openstack/pkg/autohealing/utils"
)

const (
	NodeConditionType = "NodeCondition"
)

type NodeConditionCheck struct {
	// (Optional) The condition type(case sensitive) specified in Node.spec.status.conditions items. Default: "Ready".
	Type string `mapstructure:"type"`

	// (Optional) How long to wait before a unhealthy worker node should be repaired. Default: 300s
	UnhealthyDuration int `mapstructure:"unhealthyDuration"`

	// (Optional) The accepted healthy values(case sensitive) for the type. Default: ["True"]
	OKValues []string `mapstructure:"okValues"`
}

// Check returns true if the node is healthy, otherwide false.
func (check *NodeConditionCheck) Check(node NodeInfo) (bool) {
	for _, cond := range node.KubeNode.Status.Conditions {
		if string(cond.Type) == check.Type && !utils.Contains(check.OKValues, string(cond.Status)) {
			unhealthyDuration := time.Now().Sub(cond.LastTransitionTime.Time)
			if int(unhealthyDuration.Seconds()) >= check.UnhealthyDuration {
				return false
			}
		}
	}

	return true
}

func (check *NodeConditionCheck) IsMasterSupported() bool {
	return true
}

func (check *NodeConditionCheck) IsWorkerSupported() bool {
	return true
}

func newNodeConditionCheck(config interface{}) (HealthCheck, error) {
	check := NodeConditionCheck{
		UnhealthyDuration: 300,
		Type:              "Ready",
		OKValues:          []string{"True"},
	}

	err := mapstructure.Decode(config, &check)
	if err != nil {
		return nil, fmt.Errorf("failed to get configuration for health check plugin %s, error: %v", NodeConditionType, err)
	}

	return &check, nil
}

func init() {
	registerHealthCheck(NodeConditionType, newNodeConditionCheck)
}
