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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/safety"
)

// selectPodsForResize selects pods eligible for resize based on the update mode.
func selectPodsForResize(pods []corev1.Pod, mode attunev1alpha1.UpdateType, canaryPercentage int32) []corev1.Pod {
	var eligible []corev1.Pod
	for _, p := range pods {
		if resize.IsEligibleForResize(&p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	switch mode {
	case attunev1alpha1.UpdateTypeOneShot:
		return eligible[:1]
	case attunev1alpha1.UpdateTypeCanary:
		count := int(canaryPercentage) * len(eligible) / 100
		if count < 1 {
			count = 1
		}
		if count > len(eligible) {
			count = len(eligible)
		}
		return eligible[:count]
	case attunev1alpha1.UpdateTypeAuto:
		return eligible // resize all in Auto mode
	default:
		return nil
	}
}

// budgetIncrease returns the positive live-pod request increase needed to
// reach the clamped resize target. Decreases do not consume per-cycle budget.
func budgetIncrease(pod *corev1.Pod, containerName string, target corev1.ResourceRequirements) (cpuMilli int64, memBytes int64) {
	c := findContainerByName(pod, containerName)
	if c == nil {
		return 0, 0
	}
	cpuMilli = target.Requests.Cpu().MilliValue() - c.Resources.Requests.Cpu().MilliValue()
	memBytes = target.Requests.Memory().Value() - c.Resources.Requests.Memory().Value()
	if cpuMilli < 0 {
		cpuMilli = 0
	}
	if memBytes < 0 {
		memBytes = 0
	}
	return cpuMilli, memBytes
}

// executeResizes performs the actual pod resizes for all workloads with recommendations.
func (r *AttunePolicyReconciler) executeResizes(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workloads []client.Object,
	recommendations []attunev1alpha1.WorkloadRecommendation,
	podsByWorkload map[string][]corev1.Pod,
	collector rsmetrics.MetricsCollector,
	checks *resizePreChecks,
) (int, []attunev1alpha1.ResizeHistoryEntry) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		logger.Info("No clientset configured, skipping resize execution")
		return 0, nil
	}

	mode := policy.Spec.UpdateStrategy.Type
	canaryPct := int32(10)
	canaryAutoPromote := false
	if policy.Spec.UpdateStrategy.Canary != nil {
		canaryPct = policy.Spec.UpdateStrategy.Canary.Percentage
		canaryAutoPromote = policy.Spec.UpdateStrategy.Canary.AutoPromote
	}

	// Pre-build name→Object map for O(1) workload lookups.
	workloadMap := make(map[string]client.Object, len(workloads))
	for _, w := range workloads {
		workloadMap[w.GetName()] = w
	}

	// Canary auto-promotion: if all canary pods passed the observation
	// period without reverts, promote to full rollout.
	if mode == attunev1alpha1.UpdateTypeCanary && canaryAutoPromote {
		if policy.Status.Canary == nil {
			policy.Status.Canary = &attunev1alpha1.CanaryStatus{
				Phase:              attunev1alpha1.CanaryPhaseInProgress,
				ObservedGeneration: policy.Generation,
			}
		}
		for _, rec := range recommendations {
			if rec.Stale {
				continue
			}
			w := workloadMap[rec.Workload]
			if w == nil || isBatchWorkload(w) {
				continue
			}
			policy.Status.Canary.UpsertWorkload(rec.Workload)
		}
		// Drop leftover #571 rows (Job/CronJob, unmatched names) so they
		// cannot keep RollupPhase in InProgress forever. If that empties
		// the table, skip the legacy policy-wide clock: leftover Success
		// history would FullRollout and instantly promote the next app.
		emptied := pruneStaleCanaryWorkloads(policy.Status.Canary, workloadMap)
		if !emptied {
			mode = r.resolveCanaryPhase(ctx, policy, mode)
		}
	}

	resizer := resize.NewPodResizer(r.Clientset, logger)
	resizer.AllowInPlaceMemoryLimitDecrease = r.AllowInPlaceMemoryLimitDecrease
	monitor := r.newSafetyMonitor(logger, collector, policy.Spec.UpdateStrategy.SLOGuardrails)

	var totalResized int
	var history []attunev1alpha1.ResizeHistoryEntry
	now := metav1.NewTime(r.now())

	// Per-cycle budget caps. Protected by budgetMu for concurrent access.
	var budgetMu sync.Mutex
	cpuBudget := int64(-1)
	memBudget := int64(-1)
	if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
		cpuBudget = policy.Spec.UpdateStrategy.MaxTotalCPUIncrease.MilliValue()
	}
	if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
		memBudget = policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease.Value()
	}
	reserveBudget := func(cpuIncrease, memIncrease int64) bool {
		budgetMu.Lock()
		defer budgetMu.Unlock()

		budgetExceeded := (cpuBudget >= 0 && cpuIncrease > cpuBudget) ||
			(memBudget >= 0 && memIncrease > memBudget)
		if budgetExceeded {
			return false
		}
		if cpuBudget >= 0 {
			cpuBudget -= cpuIncrease
		}
		if memBudget >= 0 {
			memBudget -= memIncrease
		}
		return true
	}
	refundBudget := func(cpuRefund, memRefund int64) {
		budgetMu.Lock()
		defer budgetMu.Unlock()

		if cpuBudget >= 0 {
			cpuBudget += cpuRefund
		}
		if memBudget >= 0 {
			memBudget += memRefund
		}
	}
	// Concurrency control: semaphore limits parallel resize calls.
	concurrency := int(policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	var historyMu sync.Mutex
	var wg sync.WaitGroup

	for _, rec := range recommendations {
		if ctx.Err() != nil {
			logger.Info("Context cancelled, aborting remaining resizes")
			break
		}
		matchedWorkload := workloadMap[rec.Workload]
		if matchedWorkload == nil {
			continue
		}

		// Batch workloads (Job/CronJob) are recommend-only; skip resize.
		if isBatchWorkload(matchedWorkload) {
			continue
		}

		if r.isWorkloadCooldownActive(policy, rec.Workload) {
			logger.V(1).Info("Skipping resize, workload cooldown active",
				"workload", rec.Workload)
			continue
		}

		// Skip workloads with stale recommendations to avoid resizing
		// based on outdated data.
		if rec.Stale {
			logger.Info("Skipping resize for workload with stale recommendation", "workload", rec.Workload)
			operatormetrics.StaleRecommendationsTotal.WithLabelValues(policy.Namespace, policy.Name).Inc()
			r.emitEventOnce(policy, corev1.EventTypeWarning, "StaleRecommendation", "resize",
				"Resize deferred for workload %s: recommendation is stale (metrics source may be unavailable)", rec.Workload)
			continue
		}

		pods := podsByWorkload[rec.Workload]
		if pods == nil {
			var err error
			pods, err = r.getPodsForWorkload(ctx, matchedWorkload)
			if err != nil {
				logger.Error(err, "Failed to get pods for workload", "workload", rec.Workload)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("get_pods").Inc()
				continue
			}
		}
		if len(pods) == 0 {
			logger.Info("No pods found for workload", "workload", rec.Workload)
			continue
		}
		wlMode := mode
		if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeCanary {
			if policy.Status.Canary.WorkloadPromoted(rec.Workload) {
				wlMode = attunev1alpha1.UpdateTypeAuto
			} else {
				wlMode = attunev1alpha1.UpdateTypeCanary
			}
		}
		selectedPods := selectPodsForResize(pods, wlMode, canaryPct)
		logger.V(1).Info("Pod selection for resize",
			"workload", rec.Workload, "total", len(pods),
			"selected", len(selectedPods), "type", wlMode)
		if len(selectedPods) == 0 {
			continue
		}

		// Track canary pod names so users can identify the subset.
		if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeCanary &&
			policy.Status.Canary != nil &&
			policy.Status.Canary.Phase == attunev1alpha1.CanaryPhaseInProgress {
			for _, p := range selectedPods {
				policy.Status.Canary.Pods = appendUnique(policy.Status.Canary.Pods, p.Name)
			}
		}

		var workloadResized int32 // atomic for concurrent access
		for _, pod := range selectedPods {
			// Capture loop variables for the goroutine.
			pod, workloadName := pod, rec.Workload

			wg.Add(1)
			sem <- struct{}{} // acquire semaphore
			go func() {
				defer wg.Done()
				defer func() { <-sem }() // release semaphore

				var podHistory []attunev1alpha1.ResizeHistoryEntry
				var podReservedCPU, podReservedMem int64
				podResized := false

				// Containers within the same pod must resize sequentially.
				// Each UpdateResize bumps resourceVersion; using a stale copy
				// for the next container causes a 409 Conflict.
				for _, containerRec := range rec.Containers {
					target, clamped := buildResizeTarget(containerRec)
					if len(clamped) > 0 {
						logger.V(1).Info("Requests clamped to limits",
							"pod", pod.Name, "container", containerRec.Name,
							"clampedResources", clamped)
						for _, res := range clamped {
							operatormetrics.RequestClampedTotal.WithLabelValues(
								policy.Namespace, policy.Name, containerRec.Name, res).Inc()
						}
					}
					cpuIncrease, memIncrease := budgetIncrease(&pod, containerRec.Name, target)

					// Reserve budget before resizing so concurrent goroutines cannot
					// overspend the cap. Refund it below if the resize did not stick.
					if !reserveBudget(cpuIncrease, memIncrease) {
						logger.Info("Budget exhausted, deferring resize to next cycle",
							"pod", pod.Name, "container", containerRec.Name)
						operatormetrics.BudgetExhaustedTotal.WithLabelValues(policy.Namespace, policy.Name).Inc()
						if r.Recorder != nil {
							r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "BudgetExhausted", "resize",
								"Resize deferred for pod %s container %s: per-cycle budget exhausted",
								pod.Name, containerRec.Name)
						}
						continue
					}

					entries, outcome := r.resizeContainer(ctx, resizeParams{
						Policy:       policy,
						Pod:          &pod,
						Workload:     matchedWorkload,
						WorkloadName: workloadName,
						ContainerRec: containerRec,
						Target:       target,
						Resizer:      resizer,
						Monitor:      monitor,
						Now:          now,
						Checks:       checks,
					})
					if outcome == resizeOutcomeNone {
						podHistory = append(podHistory, entries...)
						refundBudget(cpuIncrease, memIncrease)
						continue
					}
					if outcome == resizeOutcomeEvicted {
						refundBudget(cpuIncrease+podReservedCPU, memIncrease+podReservedMem)
						podHistory = removeSuccessfulInPlaceHistory(podHistory)
						podHistory = append(podHistory, entries...)
						podResized = false
						break
					}
					podHistory = append(podHistory, entries...)
					podReservedCPU += cpuIncrease
					podReservedMem += memIncrease
					podResized = true
					// The pod variable is already updated by persistResizeAnnotations
					// with a fresh resourceVersion and annotations, so no additional
					// API Get is needed for the next container's UpdateResize call.
				}
				if len(podHistory) > 0 {
					historyMu.Lock()
					history = append(history, podHistory...)
					if podResized {
						r.startCanaryWatch(policy, pod.Name, workloadName)
					}
					historyMu.Unlock()
				}
				if podResized {
					atomic.AddInt32(&workloadResized, 1)
				}
			}()
		}
		wg.Wait() // wait for all pods in this workload before moving to the next
		if atomic.LoadInt32(&workloadResized) > 0 {
			totalResized++
		}
	}

	return totalResized, history
}

// resizeParams groups parameters for resizeContainer, reducing the function
// signature from 9 parameters to 2 (ctx + params).
type resizeParams struct {
	Policy       *attunev1alpha1.AttunePolicy
	Pod          *corev1.Pod
	Workload     client.Object
	WorkloadName string
	ContainerRec attunev1alpha1.ContainerRecommendation
	Target       corev1.ResourceRequirements
	Resizer      *resize.PodResizer
	Monitor      *safety.Monitor
	Now          metav1.Time
	Checks       *resizePreChecks
}

// resizeOutcome tells executeResizes whether a container resize succeeded
// in-place, fell back to eviction, or did not stick.
type resizeOutcome int

const (
	resizeOutcomeNone resizeOutcome = iota
	resizeOutcomeInPlace
	resizeOutcomeEvicted
)

// resizeContainer performs a single container resize on a pod, including
// skip checks, the resize call, annotation persistence, and safety checks.
// It returns the history entries produced and the outcome so callers can
// distinguish in-place success, eviction fallback, and no-op/failure.
func (r *AttunePolicyReconciler) resizeContainer(
	ctx context.Context,
	p resizeParams,
) ([]attunev1alpha1.ResizeHistoryEntry, resizeOutcome) {
	logger := log.FromContext(ctx)
	policy, pod, workload, workloadName := p.Policy, p.Pod, p.Workload, p.WorkloadName
	containerRec, resizer, monitor, now := p.ContainerRec, p.Resizer, p.Monitor, p.Now
	target := p.Target

	// Clamp the target memory limit before skip checks (including QoS
	// preservation). K8s v1.33 forbids in-place memory limit decreases
	// when the resize policy is NotRequired. Applying the clamp early
	// ensures shouldSkipResize sees the actual values that will be sent
	// to the API server. Without this, a Guaranteed QoS pod could pass
	// the QoS check with the unclamped target but then have its memory
	// limit preserved by the resize engine, breaking requests == limits.
	preClamped := target.DeepCopy()
	target = resize.ClampMemoryLimitForPolicy(pod, containerRec.Name, target, r.AllowInPlaceMemoryLimitDecrease)
	platformClamped := false
	if memLim, ok := preClamped.Limits[corev1.ResourceMemory]; ok {
		if clampedLim, cok := target.Limits[corev1.ResourceMemory]; cok && !memLim.Equal(clampedLim) {
			platformClamped = true
			logger.Info("Memory limit decrease clamped by resize policy",
				"pod", pod.Name, "container", containerRec.Name,
				"requestedLimit", memLim.String(), "clampedLimit", clampedLim.String())
			r.emitEventOnce(policy, corev1.EventTypeWarning, "MemoryLimitClamped", "resize",
				"Container %s in pod %s: memory limit decrease blocked (NotRequired resize policy); limit preserved at %s",
				containerRec.Name, pod.Name, clampedLim.String())
			operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
				policy.Namespace, policy.Name, "clamped_platform").Inc()
			// For Guaranteed QoS pods, the memory request must also be raised
			// to match the clamped limit. Otherwise requests != limits and
			// PreservesQoS blocks the resize entirely, preventing CPU changes
			// that would otherwise succeed.
			if pod.Status.QOSClass == corev1.PodQOSGuaranteed {
				if memReq, rok := target.Requests[corev1.ResourceMemory]; rok && memReq.Cmp(clampedLim) < 0 {
					target.Requests[corev1.ResourceMemory] = clampedLim.DeepCopy()
					logger.Info("Memory request raised to match clamped limit for Guaranteed QoS",
						"pod", pod.Name, "container", containerRec.Name,
						"request", clampedLim.String())
				}
			}
		}
	}

	// Client-side pre-check: do not apply a memory limit at or below recent
	// usage (plus configurable margin). Uses recommendation RawPercentile as
	// recent usage from the metrics window (#444 / #428).
	if !platformClamped {
		target = r.applyMemoryUsageFloor(ctx, policy, pod, containerRec, target)
	}

	skip, reason := r.shouldSkipResize(ctx, policy, pod, containerRec, target, p.Checks)
	if skip {
		if reason != "" {
			logger.Info("Skipping resize: "+reason,
				"pod", pod.Name, "container", containerRec.Name)
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ResizeSkipped", "resize",
				"Resize blocked for pod %s container %s: %s", pod.Name, containerRec.Name, reason)
			recordCapacitySkip(policy, reason)
		} else {
			// Determine whether the "already at target" came from a change
			// filter suppression (the raw recommendation differed but the
			// delta was below 10%). When a change filter was active, the
			// user needs to know; promote to Info level with an Event.
			cpuFiltered := containerRec.Explanation != nil && containerRec.Explanation.CPU != nil &&
				containerRec.Explanation.CPU.ChangeFilterApplied != ""
			memFiltered := containerRec.Explanation != nil && containerRec.Explanation.Memory != nil &&
				containerRec.Explanation.Memory.ChangeFilterApplied != ""
			memoryClamped := !preClamped.Requests.Memory().Equal(*target.Requests.Memory())
			if cpuFiltered || memFiltered || memoryClamped {
				logger.Info("Resize deferred: resources at target after filtering/clamping",
					"pod", pod.Name, "container", containerRec.Name,
					"cpuTarget", target.Requests.Cpu().String(),
					"memTarget", target.Requests.Memory().String(),
					"cpuChangeFilter", cpuFiltered,
					"memChangeFilter", memFiltered,
					"memoryClamped", memoryClamped)
				r.emitEventOnce(policy, corev1.EventTypeNormal, "ResizeDeferred", "resize",
					"Container %s in pod %s: resources unchanged after change filtering and/or memory clamping (cpu=%s, mem=%s)",
					containerRec.Name, pod.Name, target.Requests.Cpu().String(), target.Requests.Memory().String())
			} else {
				logger.V(1).Info("Skipping resize: already at target",
					"pod", pod.Name, "container", containerRec.Name,
					"cpuTarget", target.Requests.Cpu().String(),
					"memTarget", target.Requests.Memory().String())
			}
		}
		return nil, resizeOutcomeNone
	}

	// Last-moment live pressure re-check. Per-cycle nodeCache can hold a
	// pre-flip snapshot, and k3s/kubelet can clear or set conditions between
	// shouldSkipResize and UpdateResize. Always re-Get for pressure so we do
	// not apply a memory (or disk/PID) increase under active pressure.
	if pod.Spec.NodeName != "" {
		live := r.getNodeForResize(ctx, pod.Spec.NodeName)
		if live != nil {
			if reason := nodePressureBlocksIncrease(live, pod, containerRec.Name, target); reason != "" {
				logger.Info("Skipping resize (live re-check): "+reason,
					"pod", pod.Name, "container", containerRec.Name,
					"node", pod.Spec.NodeName,
					"memoryPressure", nodeConditionStatus(live, corev1.NodeMemoryPressure),
					"diskPressure", nodeConditionStatus(live, corev1.NodeDiskPressure),
					"pidPressure", nodeConditionStatus(live, corev1.NodePIDPressure))
				r.emitEventOnce(policy, corev1.EventTypeWarning, "ResizeSkipped", "resize",
					"Resize blocked for pod %s container %s: %s", pod.Name, containerRec.Name, reason)
				recordCapacitySkip(policy, reason)
				return nil, resizeOutcomeNone
			}
			// Forensic: nightlies that race synthetic MemoryPressure need to
			// see which condition status authorized the apply (#481).
			logger.V(1).Info("live node pressure re-check clear; applying resize",
				"pod", pod.Name, "container", containerRec.Name,
				"node", pod.Spec.NodeName,
				"memoryPressure", nodeConditionStatus(live, corev1.NodeMemoryPressure),
				"diskPressure", nodeConditionStatus(live, corev1.NodeDiskPressure),
				"pidPressure", nodeConditionStatus(live, corev1.NodePIDPressure),
				"memTarget", target.Requests.Memory().String(),
				"cpuTarget", target.Requests.Cpu().String())
		} else if targetIncreasesRequests(pod, containerRec.Name, target) {
			// Fail-closed: cannot verify pressure/allocatable (#483).
			reason := "node status unavailable; skipping request increase"
			logger.Info("Skipping resize (live re-check): "+reason,
				"pod", pod.Name, "container", containerRec.Name, "node", pod.Spec.NodeName)
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ResizeSkipped", "resize",
				"Resize blocked for pod %s container %s: %s", pod.Name, containerRec.Name, reason)
			recordCapacitySkip(policy, reason)
			return nil, resizeOutcomeNone
		} else {
			logger.V(1).Info("node unavailable for live re-check; allowing non-increase resize",
				"pod", pod.Name, "container", containerRec.Name, "node", pod.Spec.NodeName)
		}
	}

	evictionHistory := func() []attunev1alpha1.ResizeHistoryEntry {
		return []attunev1alpha1.ResizeHistoryEntry{
			{
				Timestamp: now, Workload: workloadName, Container: containerRec.Name,
				Resource: "cpu+memory", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted,
			},
		}
	}

	// Pods already marked Infeasible cannot be resized in-place on the current node.
	if resize.IsResizeInfeasible(pod) {
		if policy.Spec.UpdateStrategy.ResizeMethod == attunev1alpha1.ResizeMethodInPlaceOrRecreate {
			logger.Info("Pod resize is Infeasible, attempting eviction fallback",
				"pod", pod.Name, "container", containerRec.Name)
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), resizeOutcomeEvicted
			}
			// Eviction denied or blocked: record failure with actionable reason.
			return []attunev1alpha1.ResizeHistoryEntry{{
				Timestamp: now, Workload: workloadName, Container: containerRec.Name,
				Resource: "cpu+memory", Method: resize.MethodInPlace,
				Result: attunev1alpha1.ResizeResultFailed, Reason: "infeasible",
			}}, resizeOutcomeNone
		}
		logger.Info("Pod resize is Infeasible and resizeMethod is InPlaceOnly, skipping",
			"pod", pod.Name, "container", containerRec.Name)
		operatormetrics.InfeasibleSkippedTotal.WithLabelValues(pod.Namespace, workloadName).Inc()
		r.emitEventOnce(policy, corev1.EventTypeWarning, "InfeasibleBlocked", "resize",
			"Pod %s cannot be resized in-place (Infeasible) and resizeMethod is InPlaceOnly; consider InPlaceOrRecreate or free node capacity",
			pod.Name)
		return []attunev1alpha1.ResizeHistoryEntry{{
			Timestamp: now, Workload: workloadName, Container: containerRec.Name,
			Resource: "cpu+memory", Method: resize.MethodInPlace,
			From: "", To: "",
			Result: attunev1alpha1.ResizeResultFailed, Reason: "infeasible",
		}}, resizeOutcomeNone
	}

	if restartResources := resize.RestartContainerResources(pod, containerRec.Name); len(restartResources) > 0 {
		logger.Info("Container has RestartContainer resize policy; resize will trigger container restart",
			"pod", pod.Name, "container", containerRec.Name, "restartResources", restartResources)
		r.emitEventOnce(policy, corev1.EventTypeWarning, "RestartOnResize", "resize",
			"Container %s in pod %s has RestartContainer resize policy for %v; resize will restart the container",
			containerRec.Name, pod.Name, restartResources)
	}

	resizeStart := r.now()
	results, err := resizer.ResizePod(ctx, pod, containerRec.Name, target)
	if err != nil {
		// Attempt eviction fallback if configured.
		if policy.Spec.UpdateStrategy.ResizeMethod == attunev1alpha1.ResizeMethodInPlaceOrRecreate {
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), resizeOutcomeEvicted
			}
		}

		logger.Error(err, "Failed to resize pod",
			"pod", pod.Name, "container", containerRec.Name)
		var entries []attunev1alpha1.ResizeHistoryEntry
		for _, res := range results {
			entries = append(entries, newHistoryEntry(now, workloadName, containerRec.Name, res, attunev1alpha1.ResizeResultFailed))
			operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "failed").Inc()
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ResizeFailed", "resize",
				"Failed to resize pod %s container %s: %v", pod.Name, containerRec.Name, err)
		}
		return entries, resizeOutcomeNone
	}

	operatormetrics.ResizeDuration.WithLabelValues(pod.Namespace, workloadName).Observe(time.Since(resizeStart).Seconds())

	var history []attunev1alpha1.ResizeHistoryEntry
	for _, res := range results {
		result := attunev1alpha1.ResizeResultSuccess
		if !res.Success {
			result = attunev1alpha1.ResizeResultFailed
		}
		history = append(history, newHistoryEntry(now, workloadName, containerRec.Name, res, result))
		if res.Success {
			operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "success").Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "Resized", "resize",
					"Resized %s %s/%s: %s %s -> %s",
					res.Resource, workloadName, containerRec.Name, res.Resource, res.From.String(), res.To.String())
			}
		}
	}
	// Observability: successful in-place apply of a lower memory limit.
	anySuccess := false
	for _, res := range results {
		if res.Success {
			anySuccess = true
			break
		}
	}
	current := liveContainerCurrent(pod, containerRec)

	if anySuccess {
		if curLim := current.MemoryLimit; !curLim.IsZero() {
			if newLim, ok := target.Limits[corev1.ResourceMemory]; ok && newLim.Cmp(curLim) < 0 {
				operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
					policy.Namespace, policy.Name, "applied").Inc()
			}
		}
	}

	originalResources := resourceRequirementsFromValues(current)

	var restartCount int32
	if cs := findContainerStatusByName(pod, containerRec.Name); cs != nil {
		restartCount = cs.RestartCount
	}

	// revert reverts the resize and marks all history entries as Reverted.
	revert := func(reason string) {
		revertRecord := safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerRec.Name,
			OriginalResources: originalResources,
			WorkloadName:      workloadName,
		}
		revertFailed := false
		revertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), safetyRevertTimeout)
		revertErr := monitor.RevertPod(revertCtx, revertRecord)
		cancel()
		if revertErr != nil {
			logger.Error(revertErr, "Failed to revert pod after "+reason, "pod", pod.Name)
			operatormetrics.RevertFailuresTotal.WithLabelValues(pod.Namespace, workloadName, reason).Inc()
			revertFailed = true
		}
		if !revertFailed {
			operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, workloadName, reason).Inc()
			for _, res := range results {
				if res.Success {
					operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "reverted").Inc()
				}
			}
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(attunev1alpha1.ResizeResultReverted), "revert",
					"Reverted resize on %s/%s: %s", workloadName, containerRec.Name, reason)
			}
		}
		// Always mark history entries regardless of whether the revert succeeded.
		// On revert failure, mark as Failed so the resize is not recorded as Success.
		resultStatus := attunev1alpha1.ResizeResultReverted
		if revertFailed {
			resultStatus = attunev1alpha1.ResizeResultFailed
		}
		for i := range history {
			if history[i].Workload == workloadName && history[i].Container == containerRec.Name {
				history[i].Result = resultStatus
				history[i].Reason = reason
			}
		}
	}

	if reason, err := r.persistResizeAnnotations(ctx, pod, containerRec, policy.Name, workloadName, now, restartCount); err != nil {
		revert(reason)
		return history, resizeOutcomeNone
	}

	record := safety.ResizeRecord{
		PodName:           pod.Name,
		Namespace:         pod.Namespace,
		Container:         containerRec.Name,
		OriginalResources: originalResources,
		NewResources:      target,
		ResizedAt:         now.Time,
		RestartCount:      restartCount,
		WorkloadName:      workloadName,
	}
	if reason, err := r.runImmediateSafetyCheck(ctx, policy, record); err != nil {
		return history, resizeOutcomeInPlace
	} else if reason != "" {
		revert(reason)
		return history, resizeOutcomeNone
	}

	return history, resizeOutcomeInPlace
}

// persistResizeAnnotations re-fetches the pod from the API server (to get a
// fresh resourceVersion after the in-place resize) and writes the tracking
// annotations that mark the pod as resized. On failure it returns a non-empty
// revert reason so the caller can revert the resize.
//
// The update is retried on conflict because the kubelet concurrently updates
// pod status (conditions, containerStatuses) after a resize, bumping
// resourceVersion. In multi-container pods the second container's annotation
// persist races with the kubelet's status write from the first resize.
//
// A non-conflict write error (timeout after the apiserver committed) is not
// treated as failure when a follow-up Get shows this persist's tracking
// annotations already landed. Reverting in that case would roll the spec
// back and leave stale tracking annotations.
func (r *AttunePolicyReconciler) persistResizeAnnotations(
	ctx context.Context,
	pod *corev1.Pod,
	containerRec attunev1alpha1.ContainerRecommendation,
	policyName string,
	workloadName string,
	now metav1.Time,
	restartCount int32,
) (revertReason string, err error) {
	logger := log.FromContext(ctx)

	const maxRetries = 3
	for attempt := range maxRetries {
		// Re-fetch directly from API server (not informer cache) to get
		// fresh resourceVersion after UpdateResize. See #37.
		freshPod, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if getErr != nil {
			logger.Error(getErr, "Failed to re-fetch pod after resize, reverting to avoid untracked resize", "pod", pod.Name)
			return "re-fetch-failed", getErr
		}

		freshPod.Annotations = ensureAnnotations(freshPod.Annotations)
		freshPod.Annotations[annotationResizedAt] = now.UTC().Format(time.RFC3339)
		freshPod.Annotations[annotationResizedWorkload] = workloadName
		if freshPod.Labels == nil {
			freshPod.Labels = make(map[string]string)
		}
		freshPod.Labels[labelTracked] = "true"
		freshPod.Annotations[annotationPolicy] = policyName
		appendResizedContainer(freshPod, containerRec.Name)
		// Snapshot the pre-resize live container. rec.Current is the
		// pod-template value and is stale after an earlier in-place resize.
		current := liveContainerCurrent(pod, containerRec)
		freshPod.Annotations[annotationOriginalCPUPrefix+containerRec.Name] = current.CPURequest.String()
		freshPod.Annotations[annotationOriginalMemoryPrefix+containerRec.Name] = current.MemoryRequest.String()
		if !current.CPULimit.IsZero() {
			freshPod.Annotations[annotationOriginalCPULimitPrefix+containerRec.Name] = current.CPULimit.String()
		}
		if !current.MemoryLimit.IsZero() {
			freshPod.Annotations[annotationOriginalMemoryLimitPrefix+containerRec.Name] = current.MemoryLimit.String()
		}
		freshPod.Annotations[annotationOriginalRestartCountPrefix+containerRec.Name] = strconv.FormatInt(int64(restartCount), 10)

		updateErr := r.Update(ctx, freshPod)
		if updateErr == nil {
			// Propagate the fresh pod (with updated resourceVersion and annotations)
			// back to the caller so subsequent container resizes on the same pod
			// do not need an additional API Get.
			*pod = *freshPod
			return "", nil
		}
		if apierrors.IsConflict(updateErr) {
			logger.Info("Annotation update conflict, retrying", "pod", pod.Name, "attempt", attempt+1, "maxRetries", maxRetries)
			continue
		}
		// Timeout and other non-conflict errors can race a successful write.
		// Confirm via Clientset Get (not the informer cache) before revert.
		confirmed, confirmErr := r.confirmTrackingAnnotations(ctx, pod.Namespace, pod.Name, freshPod, containerRec.Name)
		if confirmErr != nil {
			logger.Error(confirmErr, "Failed to confirm annotation persist after write error",
				"pod", pod.Name, "writeError", updateErr.Error())
			return "annotation-persist-failed", updateErr
		}
		if confirmed != nil {
			logger.Info("Annotation persist write error after committed tracking annotations; treating as success",
				"pod", pod.Name, "writeError", updateErr.Error())
			*pod = *confirmed
			return "", nil
		}
		logger.Error(updateErr, "Failed to persist resize tracking annotations, reverting resize", "pod", pod.Name)
		return "annotation-persist-failed", updateErr
	}
	logger.Error(nil, "Exhausted annotation persist retries, reverting resize", "pod", pod.Name, "maxRetries", maxRetries)
	return "annotation-persist-conflict", fmt.Errorf("exhausted %d annotation persist retries", maxRetries)
}

// Confirm uses a detached budget so a cancelled or timed-out persist ctx
// cannot force a revert after the write may already have committed.
const (
	confirmTrackingAttempts = 3
	confirmTrackingTimeout  = 5 * time.Second
	confirmTrackingBackoff  = 50 * time.Millisecond
	// safetyRevertTimeout is used with context.WithoutCancel so a spent
	// PrometheusTimeout cannot cancel rollback.
	safetyRevertTimeout = 5 * time.Second
)

// confirmTrackingAnnotations returns the live pod when a Clientset Get shows
// that intended tracking annotations from this persist attempt already landed.
// A nil pod and nil error means the write did not land after retries.
// Get errors are retried; the parent ctx is not used so a dead deadline
// cannot skip confirmation.
func (r *AttunePolicyReconciler) confirmTrackingAnnotations(
	ctx context.Context, namespace, name string, intended *corev1.Pod, container string,
) (*corev1.Pod, error) {
	logger := log.FromContext(ctx)
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), confirmTrackingTimeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < confirmTrackingAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-confirmCtx.Done():
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, confirmCtx.Err()
			case <-time.After(confirmTrackingBackoff):
			}
		}
		got, err := r.Clientset.CoreV1().Pods(namespace).Get(confirmCtx, name, metav1.GetOptions{})
		if err != nil {
			lastErr = err
			logger.V(1).Info("Annotation persist confirm Get failed, retrying",
				"pod", name, "attempt", attempt+1, "maxAttempts", confirmTrackingAttempts, "error", err.Error())
			continue
		}
		if trackingAnnotationsApplied(got, intended, container) {
			return got, nil
		}
		// Object is visible but this persist's keys are not. Retry in case
		// the write is still in flight; lastErr stays nil so a clean miss
		// after retries means revert, not a Get error.
		lastErr = nil
		logger.V(1).Info("Annotation persist confirm Get missed this persist, retrying",
			"pod", name, "attempt", attempt+1, "maxAttempts", confirmTrackingAttempts)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func trackingAnnotationsApplied(got, intended *corev1.Pod, container string) bool {
	if got == nil || intended == nil || got.Annotations == nil || intended.Annotations == nil {
		return false
	}
	keys := []string{
		annotationResizedAt,
		annotationResizedWorkload,
		annotationPolicy,
		annotationOriginalCPUPrefix + container,
		annotationOriginalMemoryPrefix + container,
		annotationOriginalRestartCountPrefix + container,
	}
	for _, k := range keys {
		if intended.Annotations[k] == "" || got.Annotations[k] != intended.Annotations[k] {
			return false
		}
	}
	if lim := intended.Annotations[annotationOriginalCPULimitPrefix+container]; lim != "" &&
		got.Annotations[annotationOriginalCPULimitPrefix+container] != lim {
		return false
	}
	if lim := intended.Annotations[annotationOriginalMemoryLimitPrefix+container]; lim != "" &&
		got.Annotations[annotationOriginalMemoryLimitPrefix+container] != lim {
		return false
	}
	if !resizedContainersContains(got.Annotations[annotationResizedContainers], container) {
		return false
	}
	if got.Labels[labelTracked] != "true" {
		return false
	}
	return true
}

func resizedContainersContains(list, container string) bool {
	for _, name := range strings.Split(list, ",") {
		if strings.TrimSpace(name) == container {
			return true
		}
	}
	return false
}

// liveContainerCurrent returns the live container's requests/limits when
// the named container exists on the pod. rec.Current is the workload
// template and is stale after an in-place resize. Undo annotations and
// quota deltas must use live values. Falls back to rec.Current when the
// pod is nil or the container is missing. Keys absent on the live
// container keep rec.Current (do not invent zero).
func liveContainerCurrent(pod *corev1.Pod, rec attunev1alpha1.ContainerRecommendation) attunev1alpha1.ResourceValues {
	if pod == nil {
		return rec.Current
	}
	c := findContainerByName(pod, rec.Name)
	if c == nil {
		return rec.Current
	}
	// Start from rec.Current so missing live keys (no limit) do not invent
	// zero and wipe the snapshot used for quota deltas and undo.
	cur := rec.Current
	if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		cur.CPURequest = req.DeepCopy()
	}
	if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		cur.MemoryRequest = req.DeepCopy()
	}
	if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		cur.CPULimit = lim.DeepCopy()
	}
	if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		cur.MemoryLimit = lim.DeepCopy()
	}
	return cur
}

// resourceRequirementsFromValues copies request/limit quantities into a
// ResourceRequirements. Zero limits are omitted so pods without limits
// stay limit-free.
func resourceRequirementsFromValues(v attunev1alpha1.ResourceValues) corev1.ResourceRequirements {
	req := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    v.CPURequest.DeepCopy(),
			corev1.ResourceMemory: v.MemoryRequest.DeepCopy(),
		},
	}
	if !v.CPULimit.IsZero() || !v.MemoryLimit.IsZero() {
		req.Limits = corev1.ResourceList{}
		if !v.CPULimit.IsZero() {
			req.Limits[corev1.ResourceCPU] = v.CPULimit.DeepCopy()
		}
		if !v.MemoryLimit.IsZero() {
			req.Limits[corev1.ResourceMemory] = v.MemoryLimit.DeepCopy()
		}
	}
	return req
}

// buildResizeTarget constructs the target ResourceRequirements from a container recommendation.
// Limits are included when non-zero: for RequestsOnly they equal the current limits (no-op),
// for RequestsAndLimits they are scaled proportionally. Pods that never had limits produce
// zero-valued limit fields, which are omitted to avoid Kubernetes rejecting the resize.
// Returns the target resources and the list of resource names that were clamped.
func buildResizeTarget(rec attunev1alpha1.ContainerRecommendation) (corev1.ResourceRequirements, []string) {
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    rec.Recommended.CPURequest.DeepCopy(),
			corev1.ResourceMemory: rec.Recommended.MemoryRequest.DeepCopy(),
		},
	}
	if !rec.Recommended.CPULimit.IsZero() || !rec.Recommended.MemoryLimit.IsZero() {
		target.Limits = corev1.ResourceList{}
		if !rec.Recommended.CPULimit.IsZero() {
			target.Limits[corev1.ResourceCPU] = rec.Recommended.CPULimit.DeepCopy()
		}
		if !rec.Recommended.MemoryLimit.IsZero() {
			target.Limits[corev1.ResourceMemory] = rec.Recommended.MemoryLimit.DeepCopy()
		}
	}
	// Clamp requests to not exceed limits. When ControlledValues is
	// RequestsOnly, limits stay at current values and a growing request
	// can exceed them, causing the API server to reject the resize.
	clamped := clampRequestsToLimits(&target)
	return target, clamped
}

// clampRequestsToLimits ensures requests do not exceed limits for each resource.
// When a limit is present and the request exceeds it, the request is capped
// at the limit value to prevent API server rejection.
func clampRequestsToLimits(target *corev1.ResourceRequirements) []string {
	if target.Limits == nil {
		return nil
	}
	var clamped []string
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		lim, hasLim := target.Limits[res]
		req, hasReq := target.Requests[res]
		if hasLim && hasReq && req.Cmp(lim) > 0 {
			target.Requests[res] = lim.DeepCopy()
			clamped = append(clamped, string(res))
		}
	}
	return clamped
}

// resolveCanaryPhase checks whether canary pods have passed the observation
// period without reverts. If so, it promotes to FullRollout and returns
// ModeAuto so selectPodsForResize resizes all pods.
//
// Observation starts after a successful in-place canary resize (see
// startCanaryWatch), not on the first executeResizes attempt. A premature
// StartTime from an earlier cycle is re-anchored to the first real success.
// A revert clears the clock so the next in-place success starts a new watch
// instead of freezing or promoting immediately.
func (r *AttunePolicyReconciler) resolveCanaryPhase(ctx context.Context, policy *attunev1alpha1.AttunePolicy, currentMode attunev1alpha1.UpdateType) attunev1alpha1.UpdateType {
	logger := log.FromContext(ctx)
	observationPeriod := getObservationPeriod(policy)

	cs := policy.Status.Canary

	// Spec changed since this canary cycle started: reset so the new
	// configuration is re-validated from scratch.
	if cs != nil && cs.ObservedGeneration != 0 && cs.ObservedGeneration != policy.Generation {
		logger.Info("Policy spec changed, resetting canary observation",
			"policy", policy.Name,
			"oldGeneration", cs.ObservedGeneration,
			"newGeneration", policy.Generation)
		policy.Status.Canary = nil
		cs = nil
	}

	// Phase: FullRollout already active from a prior reconcile.
	// If per-app rows exist, still resolve so a newly matched app can
	// start its own watch instead of inheriting fleet promote.
	if cs != nil && cs.Phase == attunev1alpha1.CanaryPhaseFullRollout && len(canaryWorkloadNames(policy)) == 0 {
		return attunev1alpha1.UpdateTypeAuto
	}

	if cs == nil || cs.Phase != attunev1alpha1.CanaryPhaseInProgress {
		return currentMode
	}

	named := canaryWorkloadNames(policy)
	if len(named) > 0 {
		for _, name := range named {
			r.resolveOneCanaryWorkload(policy, cs, name, observationPeriod)
		}
		cs.SyncRollupClock()
		cs.RollupPhase()
		if cs.Phase == attunev1alpha1.CanaryPhaseFullRollout {
			logger.Info("Canary observation passed for all apps, promoting to full rollout",
				"policy", policy.Name, "observationPeriod", observationPeriod)
			return attunev1alpha1.UpdateTypeAuto
		}
		return currentMode
	}

	lastRevert := latestMatchingHistoryTime(policy.Status.ResizeHistory, func(h attunev1alpha1.ResizeHistoryEntry) bool {
		return h.Result == attunev1alpha1.ResizeResultReverted
	})
	firstSuccess := earliestSuccessfulInPlaceAfter(policy.Status.ResizeHistory, "", lastRevert)
	if firstSuccess == nil {
		// Production reverts flip the Success row in place and keep its
		// original timestamp, which is often not after StartTime. Reset
		// whenever there is no live in-place success after the last revert.
		if cs.StartTime != nil || len(cs.Pods) > 0 {
			logger.Info("Canary observation has no successful in-place resize yet, resetting watch",
				"policy", policy.Name, "observationPeriod", observationPeriod)
			cs.StartTime = nil
			cs.Pods = nil
		}
		return currentMode
	}

	if cs.StartTime == nil || cs.StartTime.Time.Before(firstSuccess.Time) {
		t := *firstSuccess
		cs.StartTime = &t
		logger.V(1).Info("Canary observation clock anchored to in-place resize",
			"policy", policy.Name, "startTime", cs.StartTime.Time)
	}

	if r.now().Sub(cs.StartTime.Time) < observationPeriod {
		return currentMode
	}
	logger.Info("Canary observation passed, promoting to full rollout",
		"policy", policy.Name, "observationPeriod", observationPeriod)
	cs.Phase = attunev1alpha1.CanaryPhaseFullRollout
	return attunev1alpha1.UpdateTypeAuto
}

// pruneStaleCanaryWorkloads removes per-app rows that can never promote:
// batch (Job/CronJob) and names that are not in the current workload list.
// Returns true when the table went from nonempty to empty.
func pruneStaleCanaryWorkloads(cs *attunev1alpha1.CanaryStatus, workloadMap map[string]client.Object) bool {
	if cs == nil || len(cs.Workloads) == 0 {
		return false
	}
	before := len(cs.Workloads)
	kept := make([]attunev1alpha1.CanaryWorkloadStatus, 0, len(cs.Workloads))
	for _, row := range cs.Workloads {
		obj := workloadMap[row.Workload]
		if obj == nil || isBatchWorkload(obj) {
			continue
		}
		kept = append(kept, row)
	}
	cs.Workloads = kept
	return before > 0 && len(kept) == 0
}

func canaryWorkloadNames(policy *attunev1alpha1.AttunePolicy) []string {
	seen := map[string]struct{}{}
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if policy.Status.Canary != nil {
		for _, w := range policy.Status.Canary.Workloads {
			add(w.Workload)
		}
	}
	return names
}

func (r *AttunePolicyReconciler) resolveOneCanaryWorkload(
	policy *attunev1alpha1.AttunePolicy,
	cs *attunev1alpha1.CanaryStatus,
	workload string,
	observationPeriod time.Duration,
) {
	logger := log.FromContext(context.Background())
	ws := cs.UpsertWorkload(workload)
	if ws.Phase == attunev1alpha1.CanaryPhaseFullRollout {
		return
	}
	lastRevert := latestMatchingHistoryTime(policy.Status.ResizeHistory, func(h attunev1alpha1.ResizeHistoryEntry) bool {
		return h.Result == attunev1alpha1.ResizeResultReverted && h.Workload == workload
	})
	firstSuccess := earliestSuccessfulInPlaceAfter(policy.Status.ResizeHistory, workload, lastRevert)
	if firstSuccess == nil {
		if ws.StartTime != nil || len(ws.Pods) > 0 {
			logger.V(1).Info("Canary observation has no successful in-place resize yet, resetting watch",
				"policy", policy.Name, "workload", workload)
			ws.StartTime = nil
			ws.Pods = nil
		}
		return
	}
	// Do not adopt leftover Success rows from before this app's canary
	// watch started (prior Auto/OneShot history).
	if ws.StartTime == nil {
		return
	}
	if ws.StartTime.Time.Before(firstSuccess.Time) {
		t := *firstSuccess
		ws.StartTime = &t
	}
	if r.now().Sub(ws.StartTime.Time) < observationPeriod {
		return
	}
	logger.Info("Canary observation passed for workload, promoting",
		"policy", policy.Name, "workload", workload, "observationPeriod", observationPeriod)
	ws.Phase = attunev1alpha1.CanaryPhaseFullRollout
}

// startCanaryWatch records the first successful in-place canary resize as the
// observation start. It is a no-op when autoPromote is off or the cycle is
// already in FullRollout.
func (r *AttunePolicyReconciler) startCanaryWatch(policy *attunev1alpha1.AttunePolicy, podName, workload string) {
	if policy.Spec.UpdateStrategy.Type != attunev1alpha1.UpdateTypeCanary {
		return
	}
	if policy.Spec.UpdateStrategy.Canary == nil || !policy.Spec.UpdateStrategy.Canary.AutoPromote {
		return
	}
	if policy.Status.Canary.WorkloadPromoted(workload) {
		return
	}
	if policy.Status.Canary == nil {
		policy.Status.Canary = &attunev1alpha1.CanaryStatus{
			Phase:              attunev1alpha1.CanaryPhaseInProgress,
			ObservedGeneration: policy.Generation,
		}
	}
	now := metav1.NewTime(r.now())
	if policy.Status.Canary.StartTime == nil {
		policy.Status.Canary.StartTime = &now
		policy.Status.Canary.ObservedGeneration = policy.Generation
	}
	if podName != "" {
		policy.Status.Canary.Pods = appendUnique(policy.Status.Canary.Pods, podName)
	}
	if workload != "" {
		ws := policy.Status.Canary.UpsertWorkload(workload)
		if ws.StartTime == nil {
			ws.StartTime = &now
			ws.Phase = attunev1alpha1.CanaryPhaseInProgress
		}
		if podName != "" {
			ws.Pods = appendUnique(ws.Pods, podName)
		}
	}
}

func latestMatchingHistoryTime(
	history []attunev1alpha1.ResizeHistoryEntry,
	match func(attunev1alpha1.ResizeHistoryEntry) bool,
) *metav1.Time {
	var latest *metav1.Time
	for i := range history {
		h := history[i]
		if !match(h) {
			continue
		}
		if latest == nil || h.Timestamp.After(latest.Time) {
			t := h.Timestamp
			latest = &t
		}
	}
	return latest
}

func earliestSuccessfulInPlaceAfter(history []attunev1alpha1.ResizeHistoryEntry, workload string, after *metav1.Time) *metav1.Time {
	var earliest *metav1.Time
	for i := range history {
		h := history[i]
		if !isSuccessfulInPlaceHistory(h) {
			continue
		}
		if workload != "" && h.Workload != workload {
			continue
		}
		if after != nil && !h.Timestamp.After(after.Time) {
			continue
		}
		if earliest == nil || h.Timestamp.Before(earliest) {
			t := h.Timestamp
			earliest = &t
		}
	}
	return earliest
}

// resizePreChecks holds per-cycle cached data for shouldSkipResize,
// avoiding redundant API calls when checking many pods in the same namespace.
// nodeCache uses sync.Map for safe concurrent access when MaxConcurrentResizes > 1.
type nodePodCache struct {
	pods []corev1.Pod
	err  error
}

type resizePreChecks struct {
	nodeCache         sync.Map // string -> *corev1.Node
	nodePods          sync.Map // string -> nodePodCache
	limitRanges       []corev1.LimitRange
	quotas            []corev1.ResourceQuota
	limitRangeListErr error
	quotaListErr      error
}

// buildResizePreChecks pre-fetches namespace-scoped LimitRanges and
// ResourceQuotas so that both executeResizes and applyStartupBoosts can
// share the data without duplicate API calls.
func (r *AttunePolicyReconciler) buildResizePreChecks(ctx context.Context, policy *attunev1alpha1.AttunePolicy) *resizePreChecks {
	logger := log.FromContext(ctx)
	checks := &resizePreChecks{}
	var limitRanges corev1.LimitRangeList
	if err := r.List(ctx, &limitRanges, client.InNamespace(policy.Namespace)); err != nil {
		logger.Info("Could not pre-fetch LimitRanges, quota pre-checks skipped", "error", err)
		checks.limitRangeListErr = err
	} else {
		checks.limitRanges = limitRanges.Items
	}
	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(policy.Namespace)); err != nil {
		logger.Info("Could not pre-fetch ResourceQuotas, quota pre-checks skipped", "error", err)
		checks.quotaListErr = err
	} else {
		checks.quotas = quotas.Items
	}
	return checks
}

// shouldSkipResize runs pre-checks and returns whether to skip the resize
// and an optional reason string. An empty reason with skip=true means the
// pod already matches the recommendation (no log needed).
func (r *AttunePolicyReconciler) shouldSkipResize(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	containerRec attunev1alpha1.ContainerRecommendation,
	target corev1.ResourceRequirements,
	checks *resizePreChecks,
) (skip bool, reason string) {
	// Already at target (compare against clamped target, not raw recommendation,
	// so requests clamped to limits are correctly detected as no-ops).
	if c := findContainerByName(pod, containerRec.Name); c != nil {
		if c.Resources.Requests.Cpu().MilliValue() == target.Requests.Cpu().MilliValue() &&
			c.Resources.Requests.Memory().Value() == target.Requests.Memory().Value() {
			return true, ""
		}
	}

	// Node allocatable / pressure (use per-cycle cached node data when available).
	// Prefer the typed Clientset so we read live API status rather than a
	// potentially stale informer snapshot. Node conditions (MemoryPressure)
	// and allocatable can change between reconciles; acting on stale False
	// allows request increases under real pressure.
	if pod.Spec.NodeName != "" {
		var node *corev1.Node
		if checks != nil {
			if cached, ok := checks.nodeCache.Load(pod.Spec.NodeName); ok {
				node, _ = cached.(*corev1.Node)
			} else {
				node = r.getNodeForResize(ctx, pod.Spec.NodeName)
				if node != nil {
					checks.nodeCache.Store(pod.Spec.NodeName, node)
				}
			}
		} else {
			node = r.getNodeForResize(ctx, pod.Spec.NodeName)
		}
		if node != nil {
			// Skip request *increases* when the node is under pressure so we
			// do not worsen packing on a stressed node (#372).
			if reason := nodePressureBlocksIncrease(node, pod, containerRec.Name, target); reason != "" {
				return true, reason
			}
			if len(node.Status.Allocatable) > 0 {
				totalCPU := int64(0)
				totalMem := int64(0)
				// Only count containers that consume resources at runtime:
				// native sidecars (restartPolicy=Always) + regular containers.
				// Completed traditional init containers are not running.
				running := append(nativeSidecars(pod.Spec.InitContainers), pod.Spec.Containers...)
				for _, c := range running {
					if c.Name == containerRec.Name {
						totalCPU += target.Requests.Cpu().MilliValue()
						totalMem += target.Requests.Memory().Value()
					} else {
						totalCPU += c.Resources.Requests.Cpu().MilliValue()
						totalMem += c.Resources.Requests.Memory().Value()
					}
				}
				if totalCPU > node.Status.Allocatable.Cpu().MilliValue() ||
					totalMem > node.Status.Allocatable.Memory().Value() {
					return true, "total pod requests would exceed node allocatable"
				}
				// Neighbor request budget: skip *increases* that would not
				// fit after other pods already reserved allocatable.
				// Decreases stay allowed. Missing nodeName or a failed
				// neighbor list leaves only the this-pod check above.
				if targetIncreasesRequests(pod, containerRec.Name, target) {
					nCPU, nMem, nErr := r.neighborRequestTotals(ctx, pod, checks)
					if nErr != nil {
						return true, "node neighbor list unavailable; skipping request increase"
					}
					allocCPU := node.Status.Allocatable.Cpu().MilliValue()
					allocMem := node.Status.Allocatable.Memory().Value()
					if totalCPU+nCPU > allocCPU || totalMem+nMem > allocMem {
						return true, "node free request budget exceeded by neighbors"
					}
				}
			}
		} else if targetIncreasesRequests(pod, containerRec.Name, target) {
			// Fail-closed for increases when node status is unavailable (#483).
			// Decreases still proceed (same philosophy as pressure gates).
			return true, "node status unavailable; skipping request increase"
		}
	}

	// Quota/LimitRange violation. Headroom is target minus live requests;
	// the template (rec.Current) is stale after an in-place resize.
	currentRes := resourceRequirementsFromValues(liveContainerCurrent(pod, containerRec))
	if checks != nil {
		if targetIncreasesRequests(pod, containerRec.Name, target) &&
			(checks.quotaListErr != nil || checks.limitRangeListErr != nil) {
			return true, "quota list unavailable; skipping request increase; check RBAC list/watch on resourcequotas and limitranges"
		}
		if err := checkQuotaCompatibilityFromLists(checks.limitRanges, checks.quotas, currentRes, target); err != nil {
			return true, "quota/limitrange violation: " + err.Error()
		}
	} else {
		if err := r.checkQuotaCompatibility(ctx, pod.Namespace, currentRes, target); err != nil {
			return true, "quota/limitrange violation: " + err.Error()
		}
	}

	// QoS class change.
	if !resize.PreservesQoS(pod, containerRec.Name, target) {
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ResizeSkipped", "resize",
				"Skipping resize for pod %s container %s: would change QoS class from Guaranteed. "+
					"Use controlledValues: RequestsAndLimits, or on K8s v1.33 set resizePolicy to RestartContainer for memory",
				pod.Name, containerRec.Name)
		}
		return true, "would change QoS class"
	}

	return false, ""
}

// recordCapacitySkip increments capacity/pressure skip metrics when the skip
// reason matches node headroom or pressure gates (#445).
func recordCapacitySkip(policy *attunev1alpha1.AttunePolicy, reason string) {
	if policy == nil || reason == "" {
		return
	}
	switch {
	case strings.Contains(reason, "exceed node allocatable"):
		operatormetrics.CapacitySkipTotal.WithLabelValues(policy.Namespace, policy.Name, "allocatable").Inc()
	case strings.Contains(reason, "neighbors"), strings.Contains(reason, "free request budget"),
		strings.Contains(reason, "neighbor list unavailable"):
		operatormetrics.CapacitySkipTotal.WithLabelValues(policy.Namespace, policy.Name, "neighbors").Inc()
	case strings.Contains(reason, "MemoryPressure"),
		strings.Contains(reason, "DiskPressure"),
		strings.Contains(reason, "PIDPressure"),
		strings.Contains(reason, "node pressure"):
		operatormetrics.CapacitySkipTotal.WithLabelValues(policy.Namespace, policy.Name, "pressure").Inc()
	case strings.Contains(reason, "node status unavailable"):
		operatormetrics.CapacitySkipTotal.WithLabelValues(policy.Namespace, policy.Name, "unavailable").Inc()
	}
}

// podsOnNode lists pods scheduled on nodeName (cached per cycle).
// The result is filtered by spec.nodeName so fake clientsets that ignore
// field selectors still behave.
func (r *AttunePolicyReconciler) podsOnNode(ctx context.Context, nodeName string, checks *resizePreChecks) ([]corev1.Pod, error) {
	if nodeName == "" {
		return nil, nil
	}
	if checks != nil {
		if v, ok := checks.nodePods.Load(nodeName); ok {
			if cached, ok := v.(nodePodCache); ok {
				return cached.pods, cached.err
			}
		}
	}
	if r.Clientset == nil {
		// Unit paths without a Clientset keep the this-pod check only.
		// Production executeResizes always has a Clientset; a live List
		// error is fail-closed above the caller.
		return nil, nil
	}
	list, err := r.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		if checks != nil {
			checks.nodePods.Store(nodeName, nodePodCache{err: err})
		}
		return nil, err
	}
	filtered := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.NodeName == nodeName {
			filtered = append(filtered, list.Items[i])
		}
	}
	if checks != nil {
		checks.nodePods.Store(nodeName, nodePodCache{pods: filtered})
	}
	return filtered, nil
}

func samePod(a, b *corev1.Pod) bool {
	if a.UID != "" && b.UID != "" {
		return a.UID == b.UID
	}
	return a.Name == b.Name && a.Namespace == b.Namespace
}

// neighborRequestTotals sums running-container requests of other pods on
// the same node. The target pod is excluded. Empty nodeName returns zeros.
// A list error is returned so increases can fail closed.
func (r *AttunePolicyReconciler) neighborRequestTotals(ctx context.Context, pod *corev1.Pod, checks *resizePreChecks) (cpu, mem int64, err error) {
	if pod.Spec.NodeName == "" {
		return 0, 0, nil
	}
	neighbors, err := r.podsOnNode(ctx, pod.Spec.NodeName, checks)
	if err != nil {
		return 0, 0, err
	}
	for _, n := range neighbors {
		if samePod(pod, &n) {
			continue
		}
		running := append(nativeSidecars(n.Spec.InitContainers), n.Spec.Containers...)
		for _, c := range running {
			cpu += c.Resources.Requests.Cpu().MilliValue()
			mem += c.Resources.Requests.Memory().Value()
		}
	}
	return cpu, mem, nil
}

// targetIncreasesRequests reports whether target raises CPU and/or memory
// requests for the named container relative to the live pod spec.
func targetIncreasesRequests(pod *corev1.Pod, containerName string, target corev1.ResourceRequirements) bool {
	c := findContainerByName(pod, containerName)
	if c == nil {
		return false
	}
	cpuInc := target.Requests.Cpu().MilliValue() > c.Resources.Requests.Cpu().MilliValue()
	memInc := target.Requests.Memory().Value() > c.Resources.Requests.Memory().Value()
	return cpuInc || memInc
}

// applyMemoryUsageFloor raises a decreasing memory limit so it stays above
// recent usage * (1 + margin/100). Recent usage is the memory recommendation
// RawPercentile (historical usage percentile before overhead).
func (r *AttunePolicyReconciler) applyMemoryUsageFloor(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	containerRec attunev1alpha1.ContainerRecommendation,
	target corev1.ResourceRequirements,
) corev1.ResourceRequirements {
	logger := log.FromContext(ctx)
	targetLim, ok := target.Limits[corev1.ResourceMemory]
	if !ok {
		return target
	}
	currentLim := containerRec.Current.MemoryLimit
	if currentLim.IsZero() {
		if c := findContainerByName(pod, containerRec.Name); c != nil {
			if lim, lok := c.Resources.Limits[corev1.ResourceMemory]; lok {
				currentLim = lim
			}
		}
	}
	if currentLim.IsZero() || targetLim.Cmp(currentLim) >= 0 {
		return target
	}

	usage, hasUsage := recentMemoryUsage(containerRec)
	if !hasUsage {
		return target
	}

	margin := float64(attunev1alpha1.DefaultDecreaseUsageMarginPercent)
	if policy.Spec.Memory.DecreaseUsageMarginPercent != nil {
		margin = float64(*policy.Spec.Memory.DecreaseUsageMarginPercent)
	}

	floored, applied := resize.FloorMemoryLimitForUsage(target, currentLim, usage, margin)
	if !applied {
		return target
	}

	newLim := floored.Limits[corev1.ResourceMemory]
	logger.Info("Memory limit decrease floored above recent usage",
		"pod", pod.Name, "container", containerRec.Name,
		"requestedLimit", targetLim.String(),
		"usage", usage.String(),
		"marginPercent", margin,
		"flooredLimit", newLim.String())
	r.emitEventOnce(policy, corev1.EventTypeWarning, "MemoryLimitUsageFloor", "resize",
		"Container %s in pod %s: memory limit decrease raised from %s to %s (usage %s + %.0f%% margin)",
		containerRec.Name, pod.Name, targetLim.String(), newLim.String(), usage.String(), margin)

	if newLim.Equal(currentLim) {
		operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
			policy.Namespace, policy.Name, "skipped_unsafe").Inc()
	} else {
		operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
			policy.Namespace, policy.Name, "clamped_usage").Inc()
	}
	return floored
}

// recentMemoryUsage returns the raw usage percentile from the recommendation
// explanation when available.
func recentMemoryUsage(containerRec attunev1alpha1.ContainerRecommendation) (resource.Quantity, bool) {
	if containerRec.Explanation == nil || containerRec.Explanation.Memory == nil {
		return resource.Quantity{}, false
	}
	u := containerRec.Explanation.Memory.RawPercentile
	if u.IsZero() || u.Sign() <= 0 {
		return resource.Quantity{}, false
	}
	return u, true
}

// getNodeForResize returns the named node for capacity/pressure checks.
// Prefer Clientset (live apiserver) when configured; fall back to the
// controller-runtime client (informer cache). Returns nil on error.
func (r *AttunePolicyReconciler) getNodeForResize(ctx context.Context, nodeName string) *corev1.Node {
	if r.Clientset != nil {
		n, err := r.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err == nil {
			return n
		}
	}
	if r.Client != nil {
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &n); err == nil {
			return &n
		}
	}
	return nil
}

// nodePressureBlocksIncrease returns a skip reason when the node is under
// MemoryPressure, DiskPressure, or PIDPressure and the target would increase
// requests for the named container. Decreases remain allowed.
func nodePressureBlocksIncrease(node *corev1.Node, pod *corev1.Pod, containerName string, target corev1.ResourceRequirements) string {
	if node == nil {
		return ""
	}
	c := findContainerByName(pod, containerName)
	if c == nil {
		return ""
	}
	cpuInc := target.Requests.Cpu().MilliValue() > c.Resources.Requests.Cpu().MilliValue()
	memInc := target.Requests.Memory().Value() > c.Resources.Requests.Memory().Value()
	if !cpuInc && !memInc {
		return ""
	}
	for _, cond := range node.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case corev1.NodeMemoryPressure:
			if memInc {
				return "node has MemoryPressure; skipping memory request increase"
			}
		case corev1.NodeDiskPressure:
			return "node has DiskPressure; skipping resource request increase"
		case corev1.NodePIDPressure:
			return "node has PIDPressure; skipping resource request increase"
		}
	}
	return ""
}

// nodeConditionStatus returns the Status string for condType on node, or
// "absent" / "unknown" when the condition or node is missing.
func nodeConditionStatus(node *corev1.Node, condType corev1.NodeConditionType) string {
	if node == nil {
		return "unknown"
	}
	for _, c := range node.Status.Conditions {
		if c.Type == condType {
			if c.Status == "" {
				return "empty"
			}
			return string(c.Status)
		}
	}
	return "absent"
}
