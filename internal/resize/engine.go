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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/attune-io/attune/internal/cluster"
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
	// AllowInPlaceMemoryLimitDecrease skips clamping memory limit decreases
	// when the cluster permits live decreases (Kubernetes 1.34+).
	AllowInPlaceMemoryLimitDecrease bool
	// InPlacePodLevelResources is true when /resize may write spec.resources
	// in the same UpdateResize as container resources.
	InPlacePodLevelResources bool
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
	//
	// A non-conflict write error can arrive after the apiserver stored
	// the new spec (timeout while reading the response, empty body).
	// confirm is set only when this attempt changed the container and
	// the error is not a conflict. The caller treats that as success
	// when a follow-up Get already shows the target, so safety still
	// runs and the budget stays spent. A rejected write leaves confirm
	// unset. A canceled context fails the follow-up Get and stays an error.
	var current, applied corev1.ResourceRequirements
	var confirm *corev1.ResourceRequirements
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
		// K8s v1.33 forbids in-place memory limit decreases with NotRequired;
		// v1.34+ allows them (best-effort kubelet check).
		adjustedTarget := ClampMemoryLimitForPolicy(fresh, container, target, r.AllowInPlaceMemoryLimitDecrease)
		if isInit {
			current = fresh.Spec.InitContainers[idx].Resources
			applied = MergeResources(current, adjustedTarget)
			updated.Spec.InitContainers[idx].Resources = applied
		} else {
			current = fresh.Spec.Containers[idx].Resources
			applied = MergeResources(current, adjustedTarget)
			updated.Spec.Containers[idx].Resources = applied
		}
		if r.InPlacePodLevelResources && updated.Spec.Resources != nil {
			if raised := RaiseToCover(updated); raised != nil {
				updated.Spec.Resources = raised
			}
		}

		r.logger.V(1).Info("resizing pod", "pod", pod.Name, "namespace", pod.Namespace,
			"container", container, "method", MethodInPlace)

		_, updateErr := r.client.CoreV1().Pods(pod.Namespace).UpdateResize(ctx, pod.Name, updated, metav1.UpdateOptions{})
		if updateErr != nil && !apierrors.IsConflict(updateErr) && !ResourceRequirementsEqual(current, applied) {
			confirm = applied.DeepCopy()
		} else {
			confirm = nil
		}
		return updateErr
	})
	if err != nil {
		if confirm != nil && r.resizeTargetLanded(ctx, pod.Namespace, pod.Name, container, *confirm) {
			applied = *confirm
			r.logger.Info("resize write error after the apiserver committed the target; treating as success",
				"pod", pod.Name, "namespace", pod.Namespace, "container", container, "writeError", err.Error())
		} else {
			return []ResizeResult{
				{PodName: pod.Name, Container: container, Resource: "cpu", Method: MethodInPlace, Success: false, Error: err},
				{PodName: pod.Name, Container: container, Resource: "memory", Method: MethodInPlace, Success: false, Error: err},
			}, fmt.Errorf("calling UpdateResize for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	fromCPU := current.Requests[corev1.ResourceCPU]
	toCPU := applied.Requests[corev1.ResourceCPU]
	fromMem := current.Requests[corev1.ResourceMemory]
	toMem := applied.Requests[corev1.ResourceMemory]

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

// MergeResources overlays target onto current. Requests start as a copy of
// current so extended keys (GPU, hugepages, ephemeral-storage) survive, then
// every key in target.Requests overwrites. Limits start from current and
// overlay target the same way; existing limits stay when the target omits
// them. This prevents adding limits to pods that never had them.
//
// Memory limits are never decreased below the current value unless the
// target explicitly set memory, because Kubernetes forbids in-place memory
// limit decreases (requires RestartContainer resize policy).
func MergeResources(current, target corev1.ResourceRequirements) corev1.ResourceRequirements {
	merged := corev1.ResourceRequirements{
		Requests: current.Requests.DeepCopy(),
	}
	if merged.Requests == nil {
		merged.Requests = corev1.ResourceList{}
	}
	for res, qty := range target.Requests {
		merged.Requests[res] = qty.DeepCopy()
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
	ClampRequestsToLimits(&merged)
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

// ResourceRequirementsEqual reports semantic equality of requests and limits.
// Nil and empty lists are equal. Quantity format (100m versus 0.1) is not a difference.
func ResourceRequirementsEqual(a, b corev1.ResourceRequirements) bool {
	return resourceListEqual(a.Requests, b.Requests) && resourceListEqual(a.Limits, b.Limits)
}

// ContainerResourcesMatch reports whether the named container's requests and
// limits already equal want. Missing containers do not match.
func ContainerResourcesMatch(pod *corev1.Pod, container string, want corev1.ResourceRequirements) bool {
	if pod == nil {
		return false
	}
	idx, isInit := findContainer(pod, container)
	if idx < 0 {
		return false
	}
	if isInit {
		return ResourceRequirementsEqual(pod.Spec.InitContainers[idx].Resources, want)
	}
	return ResourceRequirementsEqual(pod.Spec.Containers[idx].Resources, want)
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !av.Equal(bv) {
			return false
		}
	}
	return true
}

// resizeTargetLanded is a live Get, not the informer cache. A canceled
// context or a spec that is still at the old size returns false.
func (r *PodResizer) resizeTargetLanded(ctx context.Context, namespace, name, container string, want corev1.ResourceRequirements) bool {
	if r.client == nil {
		return false
	}
	fresh, err := r.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return ContainerResourcesMatch(fresh, container, want)
}

// ResizeApplyOutstanding reports that an accepted resize is still not
// applied. Infeasible is not outstanding: those pods stay eligible so
// InPlaceOrRecreate can evict, and holding them would never clear.
// PodResizeInProgress older than resizeInProgressTimeout is stale and
// allows a retry (#697). On Kubernetes 1.33+ the conditions are the
// signal. On 1.32 the deprecated Status.Resize field is the only one.
// A nil pod is not outstanding.
func ResizeApplyOutstanding(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch string(cond.Type) {
		case condPodResizeInProgress:
			if !resizeInProgressTimedOut(cond) {
				return true
			}
		case condPodResizePending:
			if cond.Reason != reasonInfeasible {
				return true
			}
		}
	}
	return pod.Status.Resize == corev1.PodResizeStatusInProgress ||
		pod.Status.Resize == corev1.PodResizeStatusDeferred
}

// InProgressStale reports a PodResizeInProgress condition that has been
// true long enough for eligibility to allow a retry. Safety observation
// must not start a new window for that stuck condition.
func InProgressStale(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) != condPodResizeInProgress || cond.Status != corev1.ConditionTrue {
			continue
		}
		return resizeInProgressTimedOut(cond)
	}
	return false
}

// IsEligibleForResize returns true if the pod can be considered for a resize
// cycle. A pod is eligible if it is Running, not marked for deletion, and does
// not have an in-progress or deferred resize. Pods marked Infeasible ARE
// eligible: they cannot be resized in-place but may be evicted when the policy
// uses InPlaceOrRecreate.
func IsEligibleForResize(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return false
	}
	return !ResizeApplyOutstanding(pod)
}

const resizeInProgressTimeout = time.Hour

func resizeInProgressTimedOut(cond corev1.PodCondition) bool {
	if cond.LastTransitionTime.IsZero() {
		return false
	}
	return time.Since(cond.LastTransitionTime.Time) >= resizeInProgressTimeout
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
	// Fallback: check deprecated Status.Resize field (K8s 1.32 alpha).
	return pod.Status.Resize == corev1.PodResizeStatusInfeasible
}

// reasonDeferred is the PodResizePending reason when the kubelet has deferred
// the resize (node cannot accept it yet; retry when capacity frees up).
const reasonDeferred = "Deferred"

// IsResizeDeferred returns true if the kubelet has deferred an in-place resize
// (PodResizePending with reason Deferred, or legacy Status.Resize=Deferred).
// Deferred pods are not eligible for a new resize until the condition clears.
func IsResizeDeferred(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) == condPodResizePending &&
			cond.Status == corev1.ConditionTrue &&
			cond.Reason == reasonDeferred {
			return true
		}
	}
	return pod.Status.Resize == corev1.PodResizeStatusDeferred
}

// ResizeDeferredSince returns when the deferred condition was last transitioned,
// if known. Zero time means unknown (legacy Status.Resize path).
func ResizeDeferredSince(pod *corev1.Pod) time.Time {
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) == condPodResizePending &&
			cond.Status == corev1.ConditionTrue &&
			cond.Reason == reasonDeferred &&
			!cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime.Time
		}
	}
	return time.Time{}
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

// ClampMemoryLimitForPolicy prevents memory limit decreases when the
// container's resize policy for memory is NotRequired (or absent, which
// defaults to NotRequired) and the cluster still rejects those decreases.
//
// Kubernetes v1.33 rejects in-place memory limit decreases unless the resize
// policy is RestartContainer. Kubernetes v1.34+ allows live decreases with a
// best-effort usage check. Pass allowInPlaceMemoryLimitDecrease=true on 1.34+
// clusters so Attune does not over-clamp.
func ClampMemoryLimitForPolicy(pod *corev1.Pod, container string, target corev1.ResourceRequirements, allowInPlaceMemoryLimitDecrease bool) corev1.ResourceRequirements {
	if allowInPlaceMemoryLimitDecrease {
		return target
	}
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

// AllowsInPlaceMemoryLimitDecrease reports whether GitVersion (e.g. "v1.34.0")
// is at least Kubernetes 1.34, where live memory limit decreases are allowed.
// Wrapper around cluster.AllowsInPlaceMemoryLimitDecrease so existing
// call sites compile until they move to cluster.Capabilities.
func AllowsInPlaceMemoryLimitDecrease(gitVersion string) bool {
	return cluster.AllowsInPlaceMemoryLimitDecrease(gitVersion)
}

// WouldRestartContainer returns true if resizing the named container would
// trigger a kubelet restart based on the container's resizePolicy. If the
// container has no resizePolicy, the default is NotRequired (no restart).
func WouldRestartContainer(pod *corev1.Pod, containerName string) bool {
	return len(RestartContainerResources(pod, containerName)) > 0
}

// RestartContainerResources returns the resource names (e.g. "cpu", "memory")
// that have RestartContainer resize policy on the named container. Returns nil
// if no resources require restart.
func RestartContainerResources(pod *corev1.Pod, containerName string) []string {
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name != containerName {
			continue
		}
		var resources []string
		for _, rp := range c.ResizePolicy {
			if rp.RestartPolicy == corev1.RestartContainer {
				resources = append(resources, string(rp.ResourceName))
			}
		}
		return resources
	}
	return nil
}
