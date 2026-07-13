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

package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
)

// applyStartupBoosts checks for recently created pods that need a temporary
// CPU boost. Pods within the boost duration that don't have the boost annotation
// get inflated CPU; pods with an expired boost get reduced to steady-state.
func (r *AttunePolicyReconciler) applyStartupBoosts(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	podsByWorkload map[string][]corev1.Pod,
	recommendations []attunev1alpha1.WorkloadRecommendation,
	resizer *resize.PodResizer,
	checks *resizePreChecks,
) {
	boostConfig := policy.Spec.CPU.StartupBoost
	if boostConfig == nil || r.Clientset == nil {
		return
	}
	logger := log.FromContext(ctx)
	multiplier, err := strconv.ParseFloat(boostConfig.Multiplier, 64)
	if err != nil || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier <= 1 {
		logger.V(1).Info("Startup boost multiplier invalid or <= 1, skipping boost",
			"multiplier", boostConfig.Multiplier, "parseError", err)
		return
	}
	boostDuration := boostConfig.Duration.Duration
	// Defense-in-depth: clamp to 1h even if the webhook was bypassed.
	if boostDuration > time.Hour {
		boostDuration = time.Hour
	}
	now := r.now()

	for _, rec := range recommendations {
		pods := podsByWorkload[rec.Workload]
		// Build per-container recommendation map for this workload.
		recMap := make(map[string]resource.Quantity, len(rec.Containers))
		for _, c := range rec.Containers {
			recMap[c.Name] = c.Recommended.CPURequest
		}

		for i := range pods {
			pod := &pods[i]
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}

			boostAtStr := pod.Annotations[annotationStartupBoostAt]
			podAge := now.Sub(pod.CreationTimestamp.Time)

			if boostAtStr == "" && podAge < boostDuration {
				// New pod within boost window: apply boosted CPU.
				boostedAny := false
				for _, c := range pod.Spec.Containers {
					recCPU, ok := recMap[c.Name]
					if !ok {
						continue
					}
					boostedMillis := int64(float64(recCPU.MilliValue()) * multiplier)
					boostedCPU := *resource.NewMilliQuantity(boostedMillis, resource.DecimalSI)
					// Cap at the policy's maxAllowed to respect admin-configured
					// ceilings even during temporary boost.
					if policy.Spec.CPU.MaxAllowed != nil && boostedCPU.Cmp(*policy.Spec.CPU.MaxAllowed) > 0 {
						boostedCPU = policy.Spec.CPU.MaxAllowed.DeepCopy()
					}
					// Cap at the container's CPU limit to avoid requests > limits
					// rejection from the API server.
					if cpuLim, hasLim := c.Resources.Limits[corev1.ResourceCPU]; hasLim && boostedCPU.Cmp(cpuLim) > 0 {
						boostedCPU = cpuLim.DeepCopy()
					}
					if c.Resources.Requests.Cpu().Cmp(boostedCPU) >= 0 {
						continue // already at or above boosted level
					}
					// Safety check: verify the boosted target does not violate
					// node allocatable, ResourceQuota, LimitRange, or QoS class.
					boostRec := attunev1alpha1.ContainerRecommendation{
						Name: c.Name,
						Current: attunev1alpha1.ResourceValues{
							CPURequest:    c.Resources.Requests.Cpu().DeepCopy(),
							MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
						},
						Recommended: attunev1alpha1.ResourceValues{
							CPURequest:    boostedCPU.DeepCopy(),
							MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
						},
					}
					boostTarget := corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    boostedCPU,
							corev1.ResourceMemory: c.Resources.Requests.Memory().DeepCopy(),
						},
					}
					if skip, reason := r.shouldSkipResize(ctx, policy, pod, boostRec, boostTarget, checks); skip {
						if reason == "" {
							reason = "already at target"
						}
						logger.Info("Skipping startup boost: "+reason,
							"pod", pod.Name, "container", c.Name,
							"boostedCPU", boostedCPU.String())
						continue
					}
					refreshed, err := r.boostResizeAndRefetch(ctx, resizer, pod, c.Name, boostedCPU)
					if err != nil {
						operatormetrics.StartupBoostTotal.WithLabelValues(pod.Namespace, rec.Workload, "failed").Inc()
						logger.Error(err, "Failed to apply startup CPU boost",
							"pod", pod.Name, "container", c.Name,
							"boostedCPU", boostedCPU.String(),
							"currentCPU", c.Resources.Requests.Cpu().String())
						if refreshed == nil {
							break // re-fetch failed, stop processing containers
						}
						continue
					}
					boostedAny = true
					operatormetrics.StartupBoostTotal.WithLabelValues(pod.Namespace, rec.Workload, "applied").Inc()
					logger.Info("Applied startup CPU boost",
						"pod", pod.Name, "container", c.Name,
						"boostedCPU", boostedCPU.String(), "steadyState", recCPU.String())
					*pod = *refreshed
				}
				// Only mark the pod with boost timestamp if at least one
				// resize succeeded. Without this guard, a failed boost
				// would trigger a spurious expiry resize on the next reconcile.
				if boostedAny {
					// Re-fetch from API server and retry on conflict to handle
					// kubelet status churn after resize. Without retry, a 409
					// leaves the annotation unset and the boost never expires.
					const maxBoostAnnotationRetries = 3
					for attempt := range maxBoostAnnotationRetries {
						freshPod, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
						if getErr != nil {
							logger.Error(getErr, "Failed to re-fetch pod for boost annotation", "pod", pod.Name)
							break
						}
						if freshPod.Annotations == nil {
							freshPod.Annotations = make(map[string]string)
						}
						freshPod.Annotations[annotationStartupBoostAt] = now.UTC().Format(time.RFC3339)
						freshPod.Annotations[annotationPolicy] = policy.Name
						if freshPod.Labels == nil {
							freshPod.Labels = make(map[string]string)
						}
						freshPod.Labels[labelTracked] = "true"
						if updateErr := r.Update(ctx, freshPod); updateErr == nil {
							*pod = *freshPod
							break
						} else if !apierrors.IsConflict(updateErr) {
							logger.Error(updateErr, "Failed to persist startup boost annotation", "pod", pod.Name)
							break
						}
						logger.V(1).Info("Boost annotation conflict, retrying",
							"pod", pod.Name, "attempt", attempt+1)
					}
				}
			} else if boostAtStr != "" {
				// Boost was applied: check if it should expire.
				boostAt, parseErr := time.Parse(time.RFC3339, boostAtStr)
				if parseErr != nil {
					logger.Error(parseErr, "Malformed startup boost annotation, skipping expiry check",
						"pod", pod.Name, "value", boostAtStr)
					continue
				}
				if now.Sub(boostAt) >= boostDuration {
					// Boost expired: resize back to steady-state.
					var boostReduceFailed bool
					for _, c := range pod.Spec.Containers {
						recCPU, ok := recMap[c.Name]
						if !ok {
							continue
						}
						// Pre-check: verify the steady-state target doesn't
						// violate LimitRange or quota constraints.
						expireRec := attunev1alpha1.ContainerRecommendation{
							Name: c.Name,
							Current: attunev1alpha1.ResourceValues{
								CPURequest:    c.Resources.Requests.Cpu().DeepCopy(),
								MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
							},
							Recommended: attunev1alpha1.ResourceValues{
								CPURequest:    recCPU.DeepCopy(),
								MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
							},
						}
						expireTarget := corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    recCPU.DeepCopy(),
								corev1.ResourceMemory: c.Resources.Requests.Memory().DeepCopy(),
							},
						}
						if skip, reason := r.shouldSkipResize(ctx, policy, pod, expireRec, expireTarget, checks); skip {
							if reason == "" {
								reason = "already at target"
							}
							logger.Info("Skipping boost expiry reduction: "+reason,
								"pod", pod.Name, "container", c.Name,
								"targetCPU", recCPU.String())
							continue
						}
						refreshed, err := r.boostResizeAndRefetch(ctx, resizer, pod, c.Name, recCPU)
						if err != nil {
							operatormetrics.StartupBoostTotal.WithLabelValues(pod.Namespace, rec.Workload, "failed").Inc()
							logger.Error(err, "Failed to reduce startup boost",
								"pod", pod.Name, "container", c.Name,
								"targetCPU", recCPU.String(),
								"currentCPU", c.Resources.Requests.Cpu().String())
							boostReduceFailed = true
							if refreshed == nil {
								break // re-fetch failed
							}
							continue
						}
						operatormetrics.StartupBoostTotal.WithLabelValues(pod.Namespace, rec.Workload, "expired").Inc()
						logger.Info("Startup boost expired, reduced to steady-state",
							"pod", pod.Name, "container", c.Name, "cpu", recCPU.String())
						*pod = *refreshed
					}
					// Only remove the boost annotation if all containers were
					// successfully reduced. If any failed, keep the annotation
					// so the next reconciliation retries. Without this guard, a
					// transient failure would leave the pod permanently at
					// boosted CPU with no future expiry attempt.
					if !boostReduceFailed {
						delete(pod.Annotations, annotationStartupBoostAt)
					}
					if updateErr := r.Update(ctx, pod); updateErr != nil {
						logger.Error(updateErr, "Failed to update pod after startup boost expiry", "pod", pod.Name)
					}
				}
			}
		}
	}
}

// adjustHPATargets checks for HPAs with the auto-tune annotation and adjusts
// boostResizeAndRefetch resizes a single container's CPU to targetCPU
// (preserving the current memory request) and re-fetches the pod from the API
// server. On resize failure it returns (original pod, err) so the caller can
// continue to the next container. On re-fetch failure it returns (nil, err),
// signaling the caller should break the container loop. On success it returns
// the refreshed pod.
func (r *AttunePolicyReconciler) boostResizeAndRefetch(
	ctx context.Context,
	resizer *resize.PodResizer,
	pod *corev1.Pod,
	containerName string,
	targetCPU resource.Quantity,
) (*corev1.Pod, error) {
	reqs := corev1.ResourceList{corev1.ResourceCPU: targetCPU}
	if c := findContainerByName(pod, containerName); c != nil {
		if memReq, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			reqs[corev1.ResourceMemory] = memReq
		}
	}
	target := corev1.ResourceRequirements{Requests: reqs}
	if _, err := resizer.ResizePod(ctx, pod, containerName, target); err != nil {
		// Return the original pod (non-nil) so the caller knows this is a
		// resize failure (continue to next container), not a re-fetch failure
		// (break the loop). A nil pod signals an unrecoverable re-fetch error.
		return pod, err
	}
	freshPod, err := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("re-fetch after boost resize: %w", err)
	}
	return freshPod, nil
}

// findContainerByName searches both regular and init containers for the named
// container and returns a pointer to it, or nil if not found.
func findContainerByName(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// findContainerStatusByName searches both regular and init container statuses
// for the named container. Returns nil if not found.
func findContainerStatusByName(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == name {
			return &pod.Status.InitContainerStatuses[i]
		}
	}
	return nil
}
