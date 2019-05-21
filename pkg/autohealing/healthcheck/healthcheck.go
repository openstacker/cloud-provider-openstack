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
	"time"

	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
)

var (
	checkPlugins = make(map[string]registerPlugin)
)

type registerPlugin func(config interface{}) (HealthCheck, error)

type NodeInfo struct {
	KubeNode          apiv1.Node
	LastUnhealthyTime time.Time
}

type HealthCheck interface {
	// Check returns true if the node is healthy, otherwide false. The plugin should deal with any error happened.
	Check(node NodeInfo) bool
	IsMasterSupported() bool
	IsWorkerSupported() bool
}

func registerHealthCheck(name string, register registerPlugin) {
	if _, found := checkPlugins[name]; found {
		log.Fatalf("Health check plugin %s is already registered.", name)
	}

	log.WithFields(log.Fields{"plugin": name}).Info("Registered health check plugin.")
	checkPlugins[name] = register
}

func GetHealthChecker(name string, config interface{}) (HealthCheck, error) {
	c, found := checkPlugins[name]
	if !found {
		return nil, nil
	}
	return c(config)
}

// CheckNodes goes through the health checkers, returns the nodes need to repair.
func CheckNodes(checkers []HealthCheck, nodes []NodeInfo) []NodeInfo {
	var failedNodes []NodeInfo

	// Check the health for each node. If any checker returns false, the node needs to repair.
	for _, node := range nodes {
		for _, checker := range checkers {
			if ok := checker.Check(node); !ok {
				failedNodes = append(failedNodes, node)
				break
			}
		}
	}

	return failedNodes
}
