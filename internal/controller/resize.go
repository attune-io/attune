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
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
	"github.com/SebTardifLabs/kube-rightsize/internal/safety"
)

// selectPodsForResize selects pods eligible for resize based on the update mode.
func selectPodsForResize(pods []corev1.Pod, mode rightsizev1alpha1.UpdateMode, canaryPercentage int32) []corev1.Pod {
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
	case rightsizev1alpha1.UpdateModeOneShot:
		return eligible[:1]
	case rightsizev1alpha1.UpdateModeCanary:
		count := int(canaryPercentage) * len(eligible) / 100
		if count < 1 {
			count = 1
		}
		if count > len(eligible) {
			count = len(eligible)
		}
		return eligible[:count]
	case rightsizev1alpha1.UpdateModeAuto:
		return eligible // resize all in Auto mode
	default:
		return nil
	}
}

// budgetIncrease returns the positive live-pod request increase needed to
// reach the clamped resize target. Decreases do not consume per-cycle budget.
func budgetIncrease(pod *corev1.Pod, containerName string, target corev1.ResourceRequirements) (cpuMilli int64, memBytes int64) {
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name != containerName {
			continue
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
	return 0, 0
}

// executeResizes performs the actual pod resizes for all workloads with recommendations.
func (r *RightSizePolicyReconciler) executeResizes(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workloads []client.Object,
	recommendations []rightsizev1alpha1.WorkloadRecommendation,
	podsByWorkload map[string][]corev1.Pod,
	collector rsmetrics.MetricsCollector,
	checks *resizePreChecks,
) (int, []rightsizev1alpha1.ResizeHistoryEntry) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		logger.Info("No clientset configured, skipping resize execution")
		return 0, nil
	}

	mode := policy.Spec.UpdateStrategy.Mode
	canaryPct := int32(10)
	canaryAutoPromote := false
	if policy.Spec.UpdateStrategy.Canary != nil {
		canaryPct = policy.Spec.UpdateStrategy.Canary.Percentage
		canaryAutoPromote = policy.Spec.UpdateStrategy.Canary.AutoPromote
	}

	// Canary auto-promotion: if all canary pods passed the observation
	// period without reverts, promote to full rollout.
	if mode == rightsizev1alpha1.UpdateModeCanary && canaryAutoPromote {
		mode = r.resolveCanaryPhase(ctx, policy, mode)
	}

	resizer := resize.NewPodResizer(r.Clientset, logger)
	monitor := r.newSafetyMonitor(logger, collector)

	var totalResized int
	var history []rightsizev1alpha1.ResizeHistoryEntry
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
	removeInPlaceSuccessHistory := func(entries []rightsizev1alpha1.ResizeHistoryEntry) []rightsizev1alpha1.ResizeHistoryEntry {
		return slices.DeleteFunc(entries, func(entry rightsizev1alpha1.ResizeHistoryEntry) bool {
			return entry.Method == "InPlace" && entry.Result == rightsizev1alpha1.ResizeResultSuccess
		})
	}

	// Concurrency control: semaphore limits parallel resize calls.
	concurrency := int(policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	var historyMu sync.Mutex
	var wg sync.WaitGroup

	// Pre-build name→Object map for O(1) workload lookups.
	workloadMap := make(map[string]client.Object, len(workloads))
	for _, w := range workloads {
		workloadMap[w.GetName()] = w
	}

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
		selectedPods := selectPodsForResize(pods, mode, canaryPct)
		logger.V(1).Info("Pod selection for resize",
			"workload", rec.Workload, "total", len(pods),
			"selected", len(selectedPods), "mode", mode)
		if len(selectedPods) == 0 {
			continue
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

				var podHistory []rightsizev1alpha1.ResizeHistoryEntry
				var podReservedCPU, podReservedMem int64
				podResized := false

				// Containers within the same pod must resize sequentially.
				// Each UpdateResize bumps resourceVersion; using a stale copy
				// for the next container causes a 409 Conflict.
				for _, containerRec := range rec.Containers {
					target := buildResizeTarget(containerRec)
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
						podHistory = removeInPlaceSuccessHistory(podHistory)
						podHistory = append(podHistory, entries...)
						podResized = false
						break
					}
					podHistory = append(podHistory, entries...)
					podReservedCPU += cpuIncrease
					podReservedMem += memIncrease
					podResized = true
					// Re-fetch pod from API server to get fresh resourceVersion
					// for the next container's UpdateResize call.
					freshPod, err := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if err == nil {
						pod = *freshPod
					} else {
						logger.Error(err, "Failed to re-fetch pod after container resize, remaining containers will be deferred",
							"pod", pod.Name)
						break
					}
				}
				if len(podHistory) > 0 {
					historyMu.Lock()
					history = append(history, podHistory...)
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
	Policy       *rightsizev1alpha1.RightSizePolicy
	Pod          *corev1.Pod
	Workload     client.Object
	WorkloadName string
	ContainerRec rightsizev1alpha1.ContainerRecommendation
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
func (r *RightSizePolicyReconciler) resizeContainer(
	ctx context.Context,
	p resizeParams,
) ([]rightsizev1alpha1.ResizeHistoryEntry, resizeOutcome) {
	logger := log.FromContext(ctx)
	policy, pod, workload, workloadName := p.Policy, p.Pod, p.Workload, p.WorkloadName
	containerRec, resizer, monitor, now := p.ContainerRec, p.Resizer, p.Monitor, p.Now

	target := buildResizeTarget(containerRec)

	skip, reason := r.shouldSkipResize(ctx, policy, pod, containerRec, target, p.Checks)
	if skip {
		if reason != "" {
			logger.Info("Skipping resize: "+reason,
				"pod", pod.Name, "container", containerRec.Name)
		}
		return nil, resizeOutcomeNone
	}

	evictionHistory := func() []rightsizev1alpha1.ResizeHistoryEntry {
		return []rightsizev1alpha1.ResizeHistoryEntry{
			{
				Timestamp: now, Workload: workloadName, Container: containerRec.Name,
				Resource: "cpu+memory", Method: "Eviction", Result: rightsizev1alpha1.ResizeResultEvicted,
			},
		}
	}

	// Pods already marked Infeasible cannot be resized in-place on the current node.
	if resize.IsResizeInfeasible(pod) {
		if policy.Spec.UpdateStrategy.ResizeMethod == rightsizev1alpha1.ResizeMethodInPlaceOrEvict {
			logger.Info("Pod resize is Infeasible, attempting eviction fallback",
				"pod", pod.Name, "container", containerRec.Name)
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), resizeOutcomeEvicted
			}
		} else {
			logger.Info("Pod resize is Infeasible and resizeMethod is InPlaceOnly, skipping",
				"pod", pod.Name, "container", containerRec.Name)
			operatormetrics.InfeasibleSkippedTotal.WithLabelValues(pod.Namespace, workloadName).Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "InfeasibleBlocked", "resize",
					"Pod %s cannot be resized in-place (Infeasible) and resizeMethod is InPlaceOnly; consider InPlaceOrEvict",
					pod.Name)
			}
		}
		return nil, resizeOutcomeNone
	}

	if resize.WouldRestartContainer(pod, containerRec.Name) {
		logger.Info("Container has RestartContainer resize policy; resize will trigger restart",
			"pod", pod.Name, "container", containerRec.Name)
	}

	resizeStart := r.now()
	results, err := resizer.ResizePod(ctx, pod, containerRec.Name, target)
	if err != nil {
		// Attempt eviction fallback if configured.
		if policy.Spec.UpdateStrategy.ResizeMethod == rightsizev1alpha1.ResizeMethodInPlaceOrEvict {
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), resizeOutcomeEvicted
			}
		}

		logger.Error(err, "Failed to resize pod",
			"pod", pod.Name, "container", containerRec.Name)
		var entries []rightsizev1alpha1.ResizeHistoryEntry
		for _, res := range results {
			entries = append(entries, newHistoryEntry(now, workloadName, containerRec.Name, res, rightsizev1alpha1.ResizeResultFailed))
			operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "failed").Inc()
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ResizeFailed", "resize",
				"Failed to resize pod %s container %s: %v", pod.Name, containerRec.Name, err)
		}
		return entries, resizeOutcomeNone
	}

	operatormetrics.ResizeDuration.WithLabelValues(pod.Namespace, workloadName).Observe(time.Since(resizeStart).Seconds())

	var history []rightsizev1alpha1.ResizeHistoryEntry
	for _, res := range results {
		result := rightsizev1alpha1.ResizeResultSuccess
		if !res.Success {
			result = rightsizev1alpha1.ResizeResultFailed
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

	originalResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    containerRec.Current.CPURequest.DeepCopy(),
			corev1.ResourceMemory: containerRec.Current.MemoryRequest.DeepCopy(),
		},
	}
	if !containerRec.Current.CPULimit.IsZero() || !containerRec.Current.MemoryLimit.IsZero() {
		originalResources.Limits = make(corev1.ResourceList)
		if !containerRec.Current.CPULimit.IsZero() {
			originalResources.Limits[corev1.ResourceCPU] = containerRec.Current.CPULimit.DeepCopy()
		}
		if !containerRec.Current.MemoryLimit.IsZero() {
			originalResources.Limits[corev1.ResourceMemory] = containerRec.Current.MemoryLimit.DeepCopy()
		}
	}

	var restartCount int32
	for _, cs := range slices.Concat(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses) {
		if cs.Name == containerRec.Name {
			restartCount = cs.RestartCount
			break
		}
	}

	// revert reverts the resize and marks all history entries as Reverted.
	revert := func(reason string) {
		revertRecord := safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerRec.Name,
			OriginalResources: originalResources,
		}
		if revertErr := monitor.RevertPod(ctx, revertRecord); revertErr != nil {
			logger.Error(revertErr, "Failed to revert pod after "+reason, "pod", pod.Name)
			return
		}
		operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, workloadName, reason).Inc()
		for _, res := range results {
			if res.Success {
				operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "reverted").Inc()
			}
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(rightsizev1alpha1.ResizeResultReverted), "revert",
				"Reverted resize on %s/%s: %s", workloadName, containerRec.Name, reason)
		}
		for i := range history {
			if history[i].Workload == workloadName && history[i].Container == containerRec.Name {
				history[i].Result = rightsizev1alpha1.ResizeResultReverted
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
	}
	if reason, err := r.runImmediateSafetyCheck(ctx, policy, monitor, record); err != nil {
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
func (r *RightSizePolicyReconciler) persistResizeAnnotations(
	ctx context.Context,
	pod *corev1.Pod,
	containerRec rightsizev1alpha1.ContainerRecommendation,
	policyName string,
	workloadName string,
	now metav1.Time,
	restartCount int32,
) (revertReason string, err error) {
	logger := log.FromContext(ctx)

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
	freshPod.Annotations[annotationOriginalCPUPrefix+containerRec.Name] = containerRec.Current.CPURequest.String()
	freshPod.Annotations[annotationOriginalMemoryPrefix+containerRec.Name] = containerRec.Current.MemoryRequest.String()
	if !containerRec.Current.CPULimit.IsZero() {
		freshPod.Annotations[annotationOriginalCPULimitPrefix+containerRec.Name] = containerRec.Current.CPULimit.String()
	}
	if !containerRec.Current.MemoryLimit.IsZero() {
		freshPod.Annotations[annotationOriginalMemoryLimitPrefix+containerRec.Name] = containerRec.Current.MemoryLimit.String()
	}
	freshPod.Annotations[annotationOriginalRestartCountPrefix+containerRec.Name] = strconv.FormatInt(int64(restartCount), 10)

	if updateErr := r.Update(ctx, freshPod); updateErr != nil {
		logger.Error(updateErr, "Failed to persist resize tracking annotations, reverting resize", "pod", pod.Name)
		return "annotation-persist-failed", updateErr
	}
	return "", nil
}

// buildResizeTarget constructs the target ResourceRequirements from a container recommendation.
// Limits are included when non-zero: for RequestsOnly they equal the current limits (no-op),
// for RequestsAndLimits they are scaled proportionally. Pods that never had limits produce
// zero-valued limit fields, which are omitted to avoid Kubernetes rejecting the resize.
func buildResizeTarget(rec rightsizev1alpha1.ContainerRecommendation) corev1.ResourceRequirements {
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
	clampRequestsToLimits(&target)
	return target
}

// clampRequestsToLimits ensures requests do not exceed limits for each resource.
// When a limit is present and the request exceeds it, the request is capped
// at the limit value to prevent API server rejection.
func clampRequestsToLimits(target *corev1.ResourceRequirements) {
	if target.Limits == nil {
		return
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		lim, hasLim := target.Limits[res]
		req, hasReq := target.Requests[res]
		if hasLim && hasReq && req.Cmp(lim) > 0 {
			target.Requests[res] = lim.DeepCopy()
		}
	}
}

// resolveCanaryPhase checks whether canary pods have passed the observation
// period without reverts. If so, it promotes to FullRollout and returns
// ModeAuto so selectPodsForResize resizes all pods.
func (r *RightSizePolicyReconciler) resolveCanaryPhase(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, currentMode rightsizev1alpha1.UpdateMode) rightsizev1alpha1.UpdateMode {
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
	if cs != nil && cs.Phase == rightsizev1alpha1.CanaryPhaseFullRollout {
		return rightsizev1alpha1.UpdateModeAuto
	}

	// Phase: CanaryInProgress -- check if observation period has elapsed.
	if cs != nil && cs.Phase == rightsizev1alpha1.CanaryPhaseInProgress && cs.StartTime != nil {
		elapsed := r.now().Sub(cs.StartTime.Time)
		if elapsed >= observationPeriod {
			// Check for reverts during the observation window and require at least
			// one successful in-place resize before promoting.
			hasRevert := false
			hasSuccessfulInPlaceResize := false
			for _, h := range policy.Status.ResizeHistory {
				if !h.Timestamp.After(cs.StartTime.Time) {
					continue
				}
				if h.Result == rightsizev1alpha1.ResizeResultReverted {
					hasRevert = true
					break
				}
				if isSuccessfulInPlaceHistory(h) {
					hasSuccessfulInPlaceResize = true
				}
			}
			if hasRevert {
				logger.Info("Canary observation found reverts, staying in canary mode",
					"policy", policy.Name, "observationPeriod", observationPeriod)
				return currentMode
			}
			if !hasSuccessfulInPlaceResize {
				logger.Info("Canary observation has no successful in-place resize yet, staying in canary mode",
					"policy", policy.Name, "observationPeriod", observationPeriod)
				return currentMode
			}
			logger.Info("Canary observation passed, promoting to full rollout",
				"policy", policy.Name, "observationPeriod", observationPeriod)
			policy.Status.Canary.Phase = rightsizev1alpha1.CanaryPhaseFullRollout
			return rightsizev1alpha1.UpdateModeAuto
		}
		return currentMode
	}

	// Phase: not started yet. Initialize canary tracking on the next resize.
	if cs == nil {
		now := metav1.NewTime(r.now())
		policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
			Phase:              rightsizev1alpha1.CanaryPhaseInProgress,
			StartTime:          &now,
			ObservedGeneration: policy.Generation,
		}
	}
	return currentMode
}

// resizePreChecks holds per-cycle cached data for shouldSkipResize,
// avoiding redundant API calls when checking many pods in the same namespace.
// nodeCache uses sync.Map for safe concurrent access when MaxConcurrentResizes > 1.
type resizePreChecks struct {
	nodeCache   sync.Map // string -> *corev1.Node
	limitRanges []corev1.LimitRange
	quotas      []corev1.ResourceQuota
}

// buildResizePreChecks pre-fetches namespace-scoped LimitRanges and
// ResourceQuotas so that both executeResizes and applyStartupBoosts can
// share the data without duplicate API calls.
func (r *RightSizePolicyReconciler) buildResizePreChecks(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) *resizePreChecks {
	logger := log.FromContext(ctx)
	var limitRanges corev1.LimitRangeList
	if err := r.List(ctx, &limitRanges, client.InNamespace(policy.Namespace)); err != nil {
		logger.Info("Could not pre-fetch LimitRanges, quota pre-checks skipped", "error", err)
	}
	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(policy.Namespace)); err != nil {
		logger.Info("Could not pre-fetch ResourceQuotas, quota pre-checks skipped", "error", err)
	}
	return &resizePreChecks{
		limitRanges: limitRanges.Items,
		quotas:      quotas.Items,
	}
}

// shouldSkipResize runs pre-checks and returns whether to skip the resize
// and an optional reason string. An empty reason with skip=true means the
// pod already matches the recommendation (no log needed).
func (r *RightSizePolicyReconciler) shouldSkipResize(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	pod *corev1.Pod,
	containerRec rightsizev1alpha1.ContainerRecommendation,
	target corev1.ResourceRequirements,
	checks *resizePreChecks,
) (skip bool, reason string) {
	// Already at target (compare against clamped target, not raw recommendation,
	// so requests clamped to limits are correctly detected as no-ops).
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name == containerRec.Name {
			if c.Resources.Requests.Cpu().MilliValue() == target.Requests.Cpu().MilliValue() &&
				c.Resources.Requests.Memory().Value() == target.Requests.Memory().Value() {
				return true, ""
			}
			break
		}
	}

	// Node allocatable exceeded (use cached node data when available).
	if pod.Spec.NodeName != "" {
		var node *corev1.Node
		if checks != nil {
			if cached, ok := checks.nodeCache.Load(pod.Spec.NodeName); ok {
				node, _ = cached.(*corev1.Node)
			} else {
				var n corev1.Node
				if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &n); err == nil {
					node = &n
				}
				checks.nodeCache.Store(pod.Spec.NodeName, node)
			}
		} else {
			var n corev1.Node
			if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &n); err == nil {
				node = &n
			}
		}
		if node != nil && len(node.Status.Allocatable) > 0 {
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
		}
	}

	// Quota/LimitRange violation.
	currentRes := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    containerRec.Current.CPURequest.DeepCopy(),
			corev1.ResourceMemory: containerRec.Current.MemoryRequest.DeepCopy(),
		},
	}
	if checks != nil {
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
					"Set controlledValues: RequestsAndLimits to resize Guaranteed pods",
				pod.Name, containerRec.Name)
		}
		return true, "would change QoS class"
	}

	return false, ""
}
