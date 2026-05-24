/*
Copyright 2026.

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

package metrics

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VPAGVR is the GroupVersionResource for VerticalPodAutoscaler objects.
var VPAGVR = schema.GroupVersionResource{
	Group:    "autoscaling.k8s.io",
	Version:  "v1",
	Resource: "verticalpodautoscalers",
}

// VPAContainerRecommendation holds the parsed target recommendation from
// a VPA's status.recommendation.containerRecommendations entry.
type VPAContainerRecommendation struct {
	ContainerName string
	CPUTarget     resource.Quantity
	MemoryTarget  resource.Quantity
}

// ReadVPARecommendations fetches a VPA object by name/namespace using the
// controller-runtime client and extracts the target CPU and memory
// recommendations for each container. It uses unstructured access to avoid
// importing the VPA API types as a Go dependency.
func ReadVPARecommendations(ctx context.Context, c client.Client, name, namespace string) ([]VPAContainerRecommendation, error) {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   VPAGVR.Group,
		Version: VPAGVR.Version,
		Kind:    "VerticalPodAutoscaler",
	})

	key := client.ObjectKey{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, vpa); err != nil {
		return nil, fmt.Errorf("fetching VPA %s/%s: %w", namespace, name, err)
	}

	// Navigate: .status.recommendation.containerRecommendations[]
	recs, found, err := unstructured.NestedSlice(vpa.Object, "status", "recommendation", "containerRecommendations")
	if err != nil {
		return nil, fmt.Errorf("reading VPA recommendation: %w", err)
	}
	if !found || len(recs) == 0 {
		return nil, nil
	}

	var result []VPAContainerRecommendation
	for _, item := range recs {
		rec, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		containerName, _, _ := unstructured.NestedString(rec, "containerName")
		if containerName == "" {
			continue
		}

		target, targetFound, _ := unstructured.NestedMap(rec, "target")
		if !targetFound || target == nil {
			continue
		}

		cpuStr, _ := target["cpu"].(string)
		memStr, _ := target["memory"].(string)

		cpuQty, err := resource.ParseQuantity(cpuStr)
		if err != nil {
			return nil, fmt.Errorf("parsing VPA CPU target %q for container %s: %w", cpuStr, containerName, err)
		}
		memQty, err := resource.ParseQuantity(memStr)
		if err != nil {
			return nil, fmt.Errorf("parsing VPA memory target %q for container %s: %w", memStr, containerName, err)
		}

		result = append(result, VPAContainerRecommendation{
			ContainerName: containerName,
			CPUTarget:     cpuQty,
			MemoryTarget:  memQty,
		})
	}

	return result, nil
}
