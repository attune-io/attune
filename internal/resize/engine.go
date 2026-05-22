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
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// MethodInPlace is the resize method for in-place pod resize.
const MethodInPlace = "InPlace"

// Pod resize condition type and reason constants matching kubelet's condition names.
const (
	condPodResizeInProgress = "PodResizeInProgress"
	condPodResizePending    = "PodResizePending"
	reasonInfeasible        = "Infeasible"
)

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
	if idx, _ := findContainer(pod, container); idx == -1 {
		return nil, fmt.Errorf("container %q not found in pod %s/%s", container, pod.Namespace, pod.Name)
	}

	// Retry loop handles 409 Conflict errors that occur when the kubelet
	// or another controller updates the pod (status conditions, container
	// statuses) between our Get and UpdateResize, bumping resourceVersion.
	// This is common during sequential multi-container resizes where the
	// kubelet applies the first container's resize before we submit the second.
	var current corev1.ResourceRequirements
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		fresh, fetchErr := r.client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if fetchErr != nil {
			return fmt.Errorf("re-fetching pod %s/%s before resize: %w", pod.Namespace, pod.Name, fetchErr)
		}

		idx, isInit := findContainer(fresh, container)
		if idx == -1 {
			return fmt.Errorf("container %q not found in pod %s/%s", container, pod.Namespace, pod.Name)
		}

		updated := fresh.DeepCopy()
		// Clamp the target memory limit when the container's resize policy
		// for memory is NotRequired (or absent, which defaults to NotRequired).
		// K8s v1.33 forbids in-place memory limit decreases with NotRequired.
		adjustedTarget := clampMemoryLimitForPolicy(fresh, container, target)
		if isInit {
			current = fresh.Spec.InitContainers[idx].Resources
			updated.Spec.InitContainers[idx].Resources = mergeResources(current, adjustedTarget)
		} else {
			current = fresh.Spec.Containers[idx].Resources
			updated.Spec.Containers[idx].Resources = mergeResources(current, adjustedTarget)
		}

		r.logger.V(1).Info("resizing pod", "pod", pod.Name, "namespace", pod.Namespace,
			"container", container, "method", MethodInPlace)

		_, updateErr := r.client.CoreV1().Pods(pod.Namespace).UpdateResize(ctx, pod.Name, updated, metav1.UpdateOptions{})
		return updateErr
	})
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

// mergeResources builds the final ResourceRequirements by applying target values on top
// of the current resources. Requests are always taken from the target. Limits are taken
// from the target only if the target specifies them; otherwise the pod's existing limits
// are preserved. This prevents adding limits to pods that never had them.
//
// Memory limits are never decreased below the current value because Kubernetes
// forbids in-place memory limit decreases (requires RestartContainer resize policy).
func mergeResources(current, target corev1.ResourceRequirements) corev1.ResourceRequirements {
	merged := corev1.ResourceRequirements{
		Requests: target.Requests.DeepCopy(),
	}
	if len(target.Limits) > 0 || len(current.Limits) > 0 {
		// Start with current limits to preserve uncontrolled resources (e.g.,
		// when CPU uses RequestsAndLimits but memory uses RequestsOnly, the
		// target only has CPU limits; we must carry forward the memory limit).
		merged.Limits = current.Limits.DeepCopy()
		if merged.Limits == nil {
			merged.Limits = corev1.ResourceList{}
		}
		// Apply target limits on top.
		for res, qty := range target.Limits {
			merged.Limits[res] = qty.DeepCopy()
		}
		// Clamp memory limits: K8s forbids in-place memory limit decreases
		// unless the container has RestartContainer resize policy for memory.
		// Skip the clamp when the target explicitly set the memory limit
		// (e.g., ControlledValues=RequestsAndLimits), because the caller
		// intentionally set it and clamping would break Guaranteed QoS
		// (requests != limits).
		_, targetSetMemLimit := target.Limits[corev1.ResourceMemory]
		if !targetSetMemLimit {
			if currentMemLim, ok := current.Limits[corev1.ResourceMemory]; ok {
				if mergedMemLim, ok := merged.Limits[corev1.ResourceMemory]; ok {
					if mergedMemLim.Cmp(currentMemLim) < 0 {
						merged.Limits[corev1.ResourceMemory] = currentMemLim.DeepCopy()
					}
				}
			}
		}
	}
	// CPU limits are not clamped: K8s allows in-place CPU limit decreases.
	return merged
}

// findContainer searches both regular and init containers for the named container.
// Returns the index and whether it was found in InitContainers.
func findContainer(pod *corev1.Pod, name string) (idx int, isInit bool) {
	for i, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return i, true
		}
	}
	for i, c := range pod.Spec.Containers {
		if c.Name == name {
			return i, false
		}
	}
	return -1, false
}

// IsEligibleForResize returns true if the pod can be considered for a resize
// cycle. A pod is eligible if it is Running, not marked for deletion, and does
// not have an in-progress or deferred resize. Pods marked Infeasible ARE
// eligible: they cannot be resized in-place but may be evicted when the policy
// uses InPlaceOrEvict.
func IsEligibleForResize(pod *corev1.Pod) bool {
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
		if condType == condPodResizeInProgress {
			return false
		}
		if condType == condPodResizePending && cond.Reason != reasonInfeasible {
			return false
		}
	}
	return true
}

// IsResizeInfeasible returns true if the kubelet has marked the pod's resize
// as Infeasible, meaning it cannot be completed in-place on the current node.
func IsResizeInfeasible(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) == condPodResizePending &&
			cond.Status == corev1.ConditionTrue &&
			cond.Reason == reasonInfeasible {
			return true
		}
	}
	return false
}

// EvictPod evicts a pod using the Eviction API, which respects
// PodDisruptionBudgets. Returns an error if the eviction is denied.
func (r *PodResizer) EvictPod(ctx context.Context, pod *corev1.Pod) error {
	r.logger.Info("evicting pod for resize fallback",
		"pod", pod.Name, "namespace", pod.Namespace)
	return r.client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	})
}

// clampMemoryLimitForPolicy prevents memory limit decreases when the
// container's resize policy for memory is NotRequired (or absent, which
// defaults to NotRequired). Kubernetes v1.33 rejects in-place memory limit
// decreases unless the resize policy is RestartContainer.
func clampMemoryLimitForPolicy(pod *corev1.Pod, container string, target corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(target.Limits) == 0 {
		return target
	}
	targetMemLim, hasTargetMem := target.Limits[corev1.ResourceMemory]
	if !hasTargetMem {
		return target
	}

	// Find the container and check if memory resize policy allows in-place decrease.
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name != container {
			continue
		}
		memPolicyAllowsDecrease := false
		for _, rp := range c.ResizePolicy {
			if rp.ResourceName == corev1.ResourceMemory && rp.RestartPolicy == corev1.RestartContainer {
				memPolicyAllowsDecrease = true
				break
			}
		}
		if memPolicyAllowsDecrease {
			return target
		}
		// Policy is NotRequired (or absent): clamp memory limit to not decrease.
		currentMemLim, ok := c.Resources.Limits[corev1.ResourceMemory]
		if !ok {
			return target
		}
		if targetMemLim.Cmp(currentMemLim) < 0 {
			adjusted := target.DeepCopy()
			adjusted.Limits[corev1.ResourceMemory] = currentMemLim.DeepCopy()
			return *adjusted
		}
		return target
	}
	return target
}

// WouldRestartContainer returns true if resizing the named container would
// trigger a kubelet restart based on the container's resizePolicy. If the
// container has no resizePolicy, the default is NotRequired (no restart).
func WouldRestartContainer(pod *corev1.Pod, containerName string) bool {
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
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
