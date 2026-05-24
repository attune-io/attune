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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
	"github.com/SebTardifLabs/kube-rightsize/internal/safety"
)

// runImmediateSafetyCheck performs an immediate safety check on a freshly
// resized pod. If auto-revert is not enabled it returns ("", nil). A non-nil
// error means the check itself failed (the caller should defer to the
// observation cycle). A non-empty revertReason means the pod is unsafe and
// should be reverted.
func (r *RightSizePolicyReconciler) runImmediateSafetyCheck(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	monitor *safety.Monitor,
	record safety.ResizeRecord,
) (revertReason string, err error) {
	if !autoRevertEnabled(policy.Spec.UpdateStrategy) {
		return "", nil
	}
	record.ObservationEnd = record.ResizedAt.Add(getObservationPeriod(policy))

	logger := log.FromContext(ctx)
	verdict, checkErr := monitor.CheckPod(ctx, record, record.ResizedAt)
	if checkErr != nil {
		logger.Error(checkErr, "Safety check failed, deferring to observation cycle",
			"pod", record.PodName)
		return "", checkErr
	}
	if !verdict.Safe {
		logger.Info("Safety violation detected, reverting",
			"pod", record.PodName, "reason", verdict.Reason)
		return verdict.Reason, nil
	}
	return "", nil
}

// tryEvictionFallback attempts to evict a pod as a fallback when in-place
// resize fails. It checks safety guards before evicting:
//   - Never evict the last replica of a workload
//   - The Eviction API itself enforces PodDisruptionBudgets
//
// Returns true if the eviction was submitted successfully.
func (r *RightSizePolicyReconciler) tryEvictionFallback(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	pod *corev1.Pod,
	workload client.Object,
	workloadName, containerName string,
	resizer *resize.PodResizer,
) bool {
	logger := log.FromContext(ctx)

	// Safety: never evict the last replica. Count running pods for this workload.
	selectorLabels := r.getPodSelectorLabels(workload)
	if len(selectorLabels) == 0 {
		logger.Info("Skipping eviction fallback: workload has no pod selector labels",
			"pod", pod.Name, "workload", workloadName)
		return false
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(pod.Namespace),
		client.MatchingLabels(selectorLabels),
	); err != nil {
		logger.Error(err, "Cannot list pods for eviction safety check, skipping eviction")
		return false
	}
	running := 0
	for _, p := range podList.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running++
		}
	}
	if running <= 1 {
		logger.Info("Skipping eviction fallback: would evict the last running replica",
			"pod", pod.Name, "workload", workloadName)
		return false
	}

	// The Eviction API respects PDBs. If the eviction is denied, the error
	// will be a 429 TooManyRequests or 500. We just log and skip.
	if err := resizer.EvictPod(ctx, pod); err != nil {
		logger.Error(err, "Eviction fallback denied (PDB or other constraint)",
			"pod", pod.Name, "workload", workloadName)
		operatormetrics.EvictionTotal.WithLabelValues(pod.Namespace, workloadName, "denied").Inc()
		return false
	}

	operatormetrics.EvictionTotal.WithLabelValues(pod.Namespace, workloadName, "success").Inc()
	if r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "Evicted", "resize",
			"Evicted pod %s for workload %s container %s: in-place resize failed, falling back to eviction",
			pod.Name, workloadName, containerName)
	}
	logger.Info("Eviction fallback successful",
		"pod", pod.Name, "workload", workloadName, "container", containerName)
	return true
}

// checkPendingSafetyObservations checks pods that were previously resized and
// annotated with tracking annotations. For each pod whose observation period
// has elapsed, it runs a safety check. Unsafe pods are reverted to their
// original resource values and the annotations are removed.
func (r *RightSizePolicyReconciler) checkPendingSafetyObservations(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, collector rsmetrics.MetricsCollector, workloads []client.Object) (observationsPending bool) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		return false
	}

	// List only pods with the tracking label (set during resize).
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(policy.Namespace), client.MatchingLabels{labelTracked: "true"}); err != nil {
		logger.Error(err, "Failed to list pods for safety observation")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
		return
	}

	monitor := r.newSafetyMonitor(logger, collector, policy.Spec.UpdateStrategy.SLOGuardrails)
	observationPeriod := getObservationPeriod(policy)

	// Build a set of workload names this policy targets for provenance checks.
	workloadNames := make(map[string]bool, len(workloads))
	for _, w := range workloads {
		workloadNames[w.GetName()] = true
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Provenance check: only process pods owned by this policy AND whose
		// tracked workload matches this policy's targets. Prevents cross-policy
		// interference and spoofed annotations from triggering reverts.
		trackedWorkload, ok := trackedWorkloadForPolicy(pod, policy.Name, workloadNames)
		if !ok {
			continue
		}

		records, err := parseResizeRecords(pod, observationPeriod, r.now())
		if err != nil {
			if errors.Is(err, errNotReady) {
				// Observation period hasn't elapsed yet. Check for critical
				// events (OOMKill, crash loops) immediately without waiting.
				if earlyRecords, buildErr := buildResizeRecords(pod, observationPeriod); buildErr == nil {
					for _, record := range earlyRecords {
						if v := safety.CheckCriticalStatuses(pod, record); v != nil {
							logger.Info("Critical safety event detected during observation period, reverting early",
								"pod", pod.Name, "container", record.Container, "reason", v.Reason)
							if revertErr := monitor.RevertPod(ctx, record); revertErr != nil {
								logger.Error(revertErr, "Failed to revert pod during early critical check", "pod", pod.Name)
								continue
							}
							operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, trackedWorkload, v.Reason).Inc()
							if r.Recorder != nil {
								r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(rightsizev1alpha1.ResizeResultReverted), "revert",
									"Early safety detection reverted resize on pod %s/%s: %s", pod.Name, record.Container, v.Message)
							}
							for j := range policy.Status.ResizeHistory {
								h := &policy.Status.ResizeHistory[j]
								if h.Workload == trackedWorkload && h.Container == record.Container && h.Result == rightsizev1alpha1.ResizeResultSuccess {
									h.Result = rightsizev1alpha1.ResizeResultReverted
								}
							}
						}
					}
				}
				logger.V(1).Info("Safety observation pending, period not yet elapsed",
					"pod", pod.Name, "observationPeriod", observationPeriod)
				observationsPending = true
			} else {
				logger.Error(err, "Failed to parse resize records", "pod", pod.Name)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
			}
			continue
		}

		var revertFailed, throttlePending bool
		for _, record := range records {
			verdict, err := monitor.CheckPod(ctx, record, r.now())
			if err != nil {
				logger.Error(err, "Safety observation check failed", "pod", pod.Name, "container", record.Container)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
				revertFailed = true
				continue
			}

			if verdict.ThrottleDeferred {
				logger.V(1).Info("Throttle check deferred (within grace period), keeping observation",
					"pod", pod.Name, "container", record.Container)
				throttlePending = true
				operatormetrics.ThrottleDeferredTotal.WithLabelValues(pod.Namespace, trackedWorkload).Inc()
			}

			if !verdict.Safe {
				logger.Info("Deferred safety violation detected, reverting",
					"pod", pod.Name, "container", record.Container, "reason", verdict.Reason)
				if revertErr := monitor.RevertPod(ctx, record); revertErr != nil {
					logger.Error(revertErr, "Failed to revert pod during safety observation", "pod", pod.Name)
					revertFailed = true
					continue
				}
				operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, trackedWorkload, verdict.Reason).Inc()
				if r.Recorder != nil {
					r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(rightsizev1alpha1.ResizeResultReverted), "revert",
						"Safety observation reverted resize on pod %s/%s: %s", pod.Name, record.Container, verdict.Message)
				}
				// Mark matching history entries as reverted so status reflects the revert.
				for i := range policy.Status.ResizeHistory {
					h := &policy.Status.ResizeHistory[i]
					if h.Workload == trackedWorkload && h.Container == record.Container && h.Result == rightsizev1alpha1.ResizeResultSuccess {
						h.Result = rightsizev1alpha1.ResizeResultReverted
					}
				}
			}
		}

		// Only remove tracking annotations if all reverts succeeded and no
		// throttle checks are still pending. If either condition holds, keep
		// annotations so the next reconciliation retries or completes the
		// deferred throttle check.
		if revertFailed || throttlePending {
			observationsPending = true
			continue
		}
		// Re-fetch directly from API server (not informer cache) to get
		// fresh resourceVersion after UpdateResize. The cache may not have
		// the watch event yet, causing a 409 Conflict on annotation update.
		// Retry on conflict to handle kubelet status churn.
		const maxCleanupRetries = 3
		var cleanupErr error
		for attempt := range maxCleanupRetries {
			freshPod, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if getErr != nil {
				logger.Error(getErr, "Failed to re-fetch pod for annotation cleanup", "pod", pod.Name)
				cleanupErr = getErr
				break
			}
			removeTrackingAnnotations(freshPod)
			updateErr := r.Update(ctx, freshPod)
			if updateErr == nil {
				cleanupErr = nil
				break
			}
			if !apierrors.IsConflict(updateErr) {
				logger.Error(updateErr, "Failed to remove resize tracking annotations", "pod", pod.Name)
				cleanupErr = updateErr
				break
			}
			logger.V(1).Info("Annotation cleanup conflict, retrying",
				"pod", pod.Name, "attempt", attempt+1)
			cleanupErr = updateErr
		}
		if cleanupErr != nil && apierrors.IsConflict(cleanupErr) {
			logger.Error(cleanupErr, "Exhausted annotation cleanup retries", "pod", pod.Name, "retries", maxCleanupRetries)
		}
	}
	return observationsPending
}

func trackedWorkloadForPolicy(pod *corev1.Pod, policyName string, workloadNames map[string]bool) (string, bool) {
	if pod.Annotations[annotationPolicy] != policyName {
		return "", false
	}

	trackedWorkload := pod.Annotations[annotationResizedWorkload]
	if trackedWorkload == "" || !workloadNames[trackedWorkload] {
		return "", false
	}

	return trackedWorkload, true
}

// errNotReady is a sentinel error indicating the pod's observation period hasn't elapsed yet.
var errNotReady = errors.New("observation period not elapsed")

// buildResizeRecords extracts safety.ResizeRecords from a pod's tracking
// annotations, one per resized container. Unlike parseResizeRecords, it does
// NOT check whether the observation period has elapsed. Returns errNotReady
// only if the pod has no tracking annotations.
func buildResizeRecords(pod *corev1.Pod, observationPeriod time.Duration) ([]safety.ResizeRecord, error) {
	resizedAtStr, ok := pod.Annotations[annotationResizedAt]
	if !ok {
		return nil, errNotReady
	}

	resizedAt, err := time.Parse(time.RFC3339, resizedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing resized-at annotation: %w", err)
	}

	containerNames := strings.Split(pod.Annotations[annotationResizedContainers], ",")
	var records []safety.ResizeRecord
	for _, containerName := range containerNames {
		containerName = strings.TrimSpace(containerName)
		if containerName == "" {
			continue
		}

		originalCPU, cpuErr := resource.ParseQuantity(pod.Annotations[annotationOriginalCPUPrefix+containerName])
		if cpuErr != nil {
			return nil, fmt.Errorf("parsing original CPU for %s: %w", containerName, cpuErr)
		}
		originalMem, memErr := resource.ParseQuantity(pod.Annotations[annotationOriginalMemoryPrefix+containerName])
		if memErr != nil {
			return nil, fmt.Errorf("parsing original memory for %s: %w", containerName, memErr)
		}

		var currentResources corev1.ResourceRequirements
		if c := findContainerByName(pod, containerName); c != nil {
			currentResources = c.Resources
		}

		var origRestartCount int32
		if rcStr := pod.Annotations[annotationOriginalRestartCountPrefix+containerName]; rcStr != "" {
			rc, parseErr := strconv.ParseInt(rcStr, 10, 32)
			if parseErr != nil {
				return nil, fmt.Errorf("parsing original restart count for %s: %w", containerName, parseErr)
			}
			origRestartCount = int32(rc)
		}

		origResources := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    originalCPU,
				corev1.ResourceMemory: originalMem,
			},
		}
		// Restore original limits if they were saved (pods that had limits set before resize).
		if cpuLimStr := pod.Annotations[annotationOriginalCPULimitPrefix+containerName]; cpuLimStr != "" {
			cpuLim, err := resource.ParseQuantity(cpuLimStr)
			if err != nil {
				return nil, fmt.Errorf("parsing original CPU limit for %s: %w", containerName, err)
			}
			if origResources.Limits == nil {
				origResources.Limits = make(corev1.ResourceList)
			}
			origResources.Limits[corev1.ResourceCPU] = cpuLim
		}
		if memLimStr := pod.Annotations[annotationOriginalMemoryLimitPrefix+containerName]; memLimStr != "" {
			memLim, err := resource.ParseQuantity(memLimStr)
			if err != nil {
				return nil, fmt.Errorf("parsing original memory limit for %s: %w", containerName, err)
			}
			if origResources.Limits == nil {
				origResources.Limits = make(corev1.ResourceList)
			}
			origResources.Limits[corev1.ResourceMemory] = memLim
		}

		records = append(records, safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerName,
			OriginalResources: origResources,
			NewResources:      currentResources,
			ResizedAt:         resizedAt,
			ObservationEnd:    resizedAt.Add(observationPeriod),
			RestartCount:      origRestartCount,
			WorkloadName:      pod.Annotations[annotationResizedWorkload],
		})
	}

	if len(records) == 0 {
		return nil, errNotReady
	}
	return records, nil
}

// parseResizeRecords extracts safety.ResizeRecords from a pod's tracking
// annotations, one per resized container. Returns errNotReady if the
// observation period hasn't elapsed or the pod has no tracking annotations.
func parseResizeRecords(pod *corev1.Pod, observationPeriod time.Duration, now time.Time) ([]safety.ResizeRecord, error) {
	records, err := buildResizeRecords(pod, observationPeriod)
	if err != nil {
		return nil, err
	}
	// All records share the same resizedAt; check the first.
	if now.Sub(records[0].ResizedAt) < observationPeriod {
		return nil, errNotReady
	}
	return records, nil
}
