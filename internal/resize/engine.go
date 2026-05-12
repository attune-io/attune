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

// Package resize implements in-place pod resizing via the Kubernetes /resize subresource.
package resize

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MethodInPlace is the resize method for in-place pod resize.
const MethodInPlace = "InPlace"

// ResizeResult represents the outcome of a resize operation.
type ResizeResult struct {
	PodName   string
	Container string
	Resource  string // "cpu" or "memory"
	From      resource.Quantity
	To        resource.Quantity
	Method    string
	Success   bool
	Error     error
}

// PodResizer performs in-place pod resizes via the Kubernetes /resize subresource.
type PodResizer struct {
	client kubernetes.Interface
	logger logr.Logger
}

// NewPodResizer creates a new PodResizer backed by the given Kubernetes client.
func NewPodResizer(client kubernetes.Interface, logger logr.Logger) *PodResizer {
	return &PodResizer{
		client: client,
		logger: logger,
	}
}

// ResizePod performs an in-place resize of the specified container in a pod.
// It deep-copies the pod, updates the target container's resources, and calls
// the /resize subresource. It returns a ResizeResult for each resource type
// (cpu and memory) describing the change.
func (r *PodResizer) ResizePod(ctx context.Context, pod *corev1.Pod, container string,
	target corev1.ResourceRequirements,
) ([]ResizeResult, error) {
	idx := -1
	for i, c := range pod.Spec.Containers {
		if c.Name == container {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, fmt.Errorf("container %q not found in pod %s/%s", container, pod.Namespace, pod.Name)
	}

	current := pod.Spec.Containers[idx].Resources

	updated := pod.DeepCopy()
	updated.Spec.Containers[idx].Resources = target

	r.logger.V(1).Info("resizing pod", "pod", pod.Name, "namespace", pod.Namespace,
		"container", container, "method", MethodInPlace)

	_, err := r.client.CoreV1().Pods(pod.Namespace).UpdateResize(ctx, pod.Name, updated, metav1.UpdateOptions{})
	if err != nil {
		return []ResizeResult{
			{PodName: pod.Name, Container: container, Resource: "cpu", Method: MethodInPlace, Success: false, Error: err},
			{PodName: pod.Name, Container: container, Resource: "memory", Method: MethodInPlace, Success: false, Error: err},
		}, fmt.Errorf("calling UpdateResize for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	fromCPU := current.Requests[corev1.ResourceCPU]
	toCPU := target.Requests[corev1.ResourceCPU]
	fromMem := current.Requests[corev1.ResourceMemory]
	toMem := target.Requests[corev1.ResourceMemory]

	results := []ResizeResult{
		{
			PodName:   pod.Name,
			Container: container,
			Resource:  "cpu",
			From:      fromCPU,
			To:        toCPU,
			Method:    MethodInPlace,
			Success:   true,
		},
		{
			PodName:   pod.Name,
			Container: container,
			Resource:  "memory",
			From:      fromMem,
			To:        toMem,
			Method:    MethodInPlace,
			Success:   true,
		},
	}

	r.logger.Info("resize submitted", "pod", pod.Name, "namespace", pod.Namespace,
		"container", container, "cpuFrom", fromCPU.String(), "cpuTo", toCPU.String(),
		"memFrom", fromMem.String(), "memTo", toMem.String())

	return results, nil
}

// CanResizeInPlace returns true if the pod is eligible for an in-place resize.
// The pod must be Running, must not be marked for deletion, and must not have
// an active resize already in progress.
func CanResizeInPlace(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return false
	}
	// Check pod conditions for active resize (preferred over deprecated Status.Resize)
	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		condType := string(cond.Type)
		if condType == "PodResizeInProgress" {
			return false
		}
		if condType == "PodResizePending" && cond.Reason != "Infeasible" {
			return false
		}
	}
	return true
}

// WouldRestartContainer returns true if resizing the named container would
// trigger a kubelet restart based on the container's resizePolicy. If the
// container has no resizePolicy, the default is NotRequired (no restart).
func WouldRestartContainer(pod *corev1.Pod, containerName string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		for _, rp := range c.ResizePolicy {
			if rp.RestartPolicy == corev1.RestartContainer {
				return true
			}
		}
		return false
	}
	return false
}

// PreservesQoS returns true if applying the target resources to the named
// container would preserve the pod's current QoS class. For Guaranteed pods
// this means requests must equal limits for both CPU and memory. Burstable
// and BestEffort pods always return true because changing resource values
// within those classes does not alter the QoS category.
func PreservesQoS(pod *corev1.Pod, container string, target corev1.ResourceRequirements) bool {
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		return true
	}

	cpuReq, hasCPUReq := target.Requests[corev1.ResourceCPU]
	cpuLim, hasCPULim := target.Limits[corev1.ResourceCPU]
	memReq, hasMemReq := target.Requests[corev1.ResourceMemory]
	memLim, hasMemLim := target.Limits[corev1.ResourceMemory]

	if !hasCPUReq || !hasCPULim || !hasMemReq || !hasMemLim {
		return false
	}

	return cpuReq.Equal(cpuLim) && memReq.Equal(memLim)
}
