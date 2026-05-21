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
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
	"github.com/SebTardifLabs/kube-rightsize/internal/safety"
)

const (
	// degradedWindowSize is the number of recent resize history entries
	// inspected when evaluating the Degraded condition.
	degradedWindowSize = 5
	// degradedRevertThreshold is the number of reverts in the window that
	// triggers the Degraded condition.
	degradedRevertThreshold = 3
	// maxBackoffDoublings caps exponential cooldown at 2^N x base.
	maxBackoffDoublings = 4
)

// isResizeMode returns true if the policy mode performs actual pod resizes.
func isResizeMode(mode rightsizev1alpha1.UpdateMode) bool {
	return mode == rightsizev1alpha1.UpdateModeOneShot || mode == rightsizev1alpha1.UpdateModeCanary || mode == rightsizev1alpha1.UpdateModeAuto
}

// newHistoryEntry creates a ResizeHistoryEntry from a resize result.
func newHistoryEntry(now metav1.Time, workload, container string, res resize.ResizeResult, result rightsizev1alpha1.ResizeResult) rightsizev1alpha1.ResizeHistoryEntry {
	return rightsizev1alpha1.ResizeHistoryEntry{
		Timestamp: now,
		Workload:  workload,
		Container: container,
		Resource:  res.Resource,
		From:      res.From.String(),
		To:        res.To.String(),
		Method:    resize.MethodInPlace,
		Result:    result,
	}
}

// removeTrackingAnnotations removes the resize-tracking annotations from a pod.
func removeTrackingAnnotations(pod *corev1.Pod) {
	// Remove per-container annotations for each tracked container.
	if names, ok := pod.Annotations[annotationResizedContainers]; ok {
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			delete(pod.Annotations, annotationOriginalCPUPrefix+name)
			delete(pod.Annotations, annotationOriginalMemoryPrefix+name)
			delete(pod.Annotations, annotationOriginalCPULimitPrefix+name)
			delete(pod.Annotations, annotationOriginalMemoryLimitPrefix+name)
			delete(pod.Annotations, annotationOriginalRestartCountPrefix+name)
		}
	}
	delete(pod.Annotations, annotationResizedAt)
	delete(pod.Annotations, annotationResizedContainers)
	delete(pod.Annotations, annotationResizedWorkload)
	delete(pod.Annotations, annotationPolicy)
	delete(pod.Labels, labelTracked)
}

// appendResizedContainer adds a container name to the comma-separated
// resized-containers annotation, avoiding duplicates.
func appendResizedContainer(pod *corev1.Pod, containerName string) {
	existing := pod.Annotations[annotationResizedContainers]
	if existing == "" {
		pod.Annotations[annotationResizedContainers] = containerName
		return
	}
	for _, name := range strings.Split(existing, ",") {
		if strings.TrimSpace(name) == containerName {
			return
		}
	}
	pod.Annotations[annotationResizedContainers] = existing + "," + containerName
}

// setFailedCondition sets a Ready=False condition on the policy and updates
// the status subresource. Errors from the status update are logged but not
// returned, since the caller typically returns a requeue result regardless.
func (r *RightSizePolicyReconciler) setFailedCondition(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, reason, message string) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}

	for attempt := range 3 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: policy.Generation,
		})
		err := r.Status().Update(ctx, policy)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Error(err, "Failed to update status")
			return
		}
		logger.Info("setFailedCondition conflict, retrying", "attempt", attempt+1)
		if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
			logger.Error(fetchErr, "Failed to re-fetch policy for status retry")
			return
		}
	}
	logger.Error(fmt.Errorf("exhausted retries"), "Failed to set failed condition after retries", "reason", reason)
}

// parseHistoryWindow parses the history window duration from the policy.
// Defense-in-depth: clamps to [1h, 720h] even if webhook validation is bypassed.
func (r *RightSizePolicyReconciler) parseHistoryWindow(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.MetricsSource.HistoryWindow != nil {
		hw := policy.Spec.MetricsSource.HistoryWindow.Duration
		if hw < time.Hour {
			hw = time.Hour
		}
		if hw > 720*time.Hour {
			hw = 720 * time.Hour
		}
		return hw
	}
	return defaultHistoryWindow
}

// getMinimumDataPoints returns the minimum data points threshold from the policy.
func (r *RightSizePolicyReconciler) getMinimumDataPoints(policy *rightsizev1alpha1.RightSizePolicy) int32 {
	if policy.Spec.MetricsSource.MinimumDataPoints != nil && *policy.Spec.MetricsSource.MinimumDataPoints > 0 {
		return *policy.Spec.MetricsSource.MinimumDataPoints
	}
	return defaultMinimumDataPoints
}

// getQueryStep returns the query step interval from the policy or the default (5m).
func (r *RightSizePolicyReconciler) getQueryStep(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.MetricsSource.QueryStep != nil {
		qs := policy.Spec.MetricsSource.QueryStep.Duration
		if qs < 10*time.Second {
			qs = 10 * time.Second
		}
		if qs > time.Hour {
			qs = time.Hour
		}
		return qs
	}
	return defaultPrometheusStep
}

// getRateWindow returns the rate window from the policy or falls back to queryStep.
func (r *RightSizePolicyReconciler) getRateWindow(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.MetricsSource.RateWindow != nil {
		rw := policy.Spec.MetricsSource.RateWindow.Duration
		if rw < 30*time.Second {
			rw = 30 * time.Second
		}
		hw := r.parseHistoryWindow(policy)
		if rw > hw {
			rw = hw
		}
		return rw
	}
	return r.getQueryStep(policy)
}

// parseCooldown returns the cooldown duration from the policy's update strategy.
func (r *RightSizePolicyReconciler) parseCooldown(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.Cooldown != nil {
		cd := policy.Spec.UpdateStrategy.Cooldown.Duration
		// Defense-in-depth: enforce minimum floor even if webhook validation is bypassed.
		minCooldown := r.MinCooldown
		if minCooldown == 0 {
			minCooldown = time.Minute
		}
		if cd > 0 && cd < minCooldown {
			cd = minCooldown
		}
		return cd
	}
	return defaultCooldown
}

// isCooldownActive checks if the policy is within the cooldown window since last resize.
// The cooldown is multiplied by 2^N where N is the number of consecutive reverts
// (exponential backoff), capped at 16x the base cooldown.
func (r *RightSizePolicyReconciler) isCooldownActive(policy *rightsizev1alpha1.RightSizePolicy) bool {
	ann := policy.Annotations
	if ann == nil {
		return false
	}
	lastStr, ok := ann[lastResizeAnnotation]
	if !ok {
		return false
	}
	last, err := time.Parse(time.RFC3339, lastStr)
	if err != nil {
		return false
	}
	cooldown := r.getEffectiveCooldown(policy)
	return r.now().Sub(last) < cooldown
}

// getEffectiveCooldown returns the cooldown with exponential backoff applied
// based on the number of consecutive reverts in the resize history.
func (r *RightSizePolicyReconciler) getEffectiveCooldown(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	base := r.parseCooldown(policy)
	reverts := consecutiveReverts(policy.Status.ResizeHistory)
	if reverts == 0 {
		return base
	}
	if reverts > maxBackoffDoublings {
		reverts = maxBackoffDoublings
	}
	multiplier := 1 << reverts // 2^N
	return base * time.Duration(multiplier)
}

// setCooldownStatus populates the CooldownStatus on the policy with the
// effective cooldown, backoff multiplier, and consecutive revert count.
func (r *RightSizePolicyReconciler) setCooldownStatus(policy *rightsizev1alpha1.RightSizePolicy) {
	base := r.parseCooldown(policy)
	reverts := consecutiveReverts(policy.Status.ResizeHistory)
	capped := reverts
	if capped > maxBackoffDoublings {
		capped = maxBackoffDoublings
	}
	multiplier := int32(1 << capped) // 2^N
	effective := base * time.Duration(multiplier)
	policy.Status.Cooldown = &rightsizev1alpha1.CooldownStatus{
		EffectiveCooldown:  &metav1.Duration{Duration: effective},
		BackoffMultiplier:  multiplier,
		ConsecutiveReverts: safeInt32(reverts),
	}
}

// markResizeTime sets the last-resize-time annotation on the policy using a
// merge patch to avoid 409 Conflict with concurrent spec changes.
func (r *RightSizePolicyReconciler) markResizeTime(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) error {
	patch := client.MergeFrom(policy.DeepCopy())
	if policy.Annotations == nil {
		policy.Annotations = make(map[string]string)
	}
	policy.Annotations[lastResizeAnnotation] = r.now().UTC().Format(time.RFC3339)
	return r.Patch(ctx, policy, patch)
}

// appendHistory appends new entries to existing history, capping at maxEntries.
//
//nolint:unparam // maxEntries is a parameter for configurability
func appendHistory(existing []rightsizev1alpha1.ResizeHistoryEntry,
	newEntries []rightsizev1alpha1.ResizeHistoryEntry, maxEntries int,
) []rightsizev1alpha1.ResizeHistoryEntry {
	result := append(existing, newEntries...)
	if len(result) > maxEntries {
		result = result[len(result)-maxEntries:]
	}
	return result
}

func resizeHistoryMethod(entry rightsizev1alpha1.ResizeHistoryEntry) string {
	if entry.Method != "" {
		return entry.Method
	}
	if entry.Result == rightsizev1alpha1.ResizeResultEvicted {
		return "Eviction"
	}
	return resize.MethodInPlace
}

func normalizeResizeHistoryMethods(history []rightsizev1alpha1.ResizeHistoryEntry) bool {
	changed := false
	for i := range history {
		method := resizeHistoryMethod(history[i])
		if method == history[i].Method {
			continue
		}
		history[i].Method = method
		changed = true
	}
	return changed
}

func isSuccessfulInPlaceHistory(entry rightsizev1alpha1.ResizeHistoryEntry) bool {
	return resizeHistoryMethod(entry) == resize.MethodInPlace && entry.Result == rightsizev1alpha1.ResizeResultSuccess
}

func removeSuccessfulInPlaceHistory(entries []rightsizev1alpha1.ResizeHistoryEntry) []rightsizev1alpha1.ResizeHistoryEntry {
	return slices.DeleteFunc(entries, isSuccessfulInPlaceHistory)
}

// setResizingCondition sets the Resizing condition based on current state.
func (r *RightSizePolicyReconciler) setResizingCondition(policy *rightsizev1alpha1.RightSizePolicy, cooldownActive bool) {
	if !isResizeMode(policy.Spec.UpdateStrategy.Mode) {
		// Non-resize modes: clear the condition.
		meta.RemoveStatusCondition(&policy.Status.Conditions, rightsizev1alpha1.ConditionResizing)
		return
	}

	if policy.Status.Workloads.Resized > 0 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionResizing,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonInProgress,
			Message:            fmt.Sprintf("%d workload(s) resized this cycle", policy.Status.Workloads.Resized),
			ObservedGeneration: policy.Generation,
		})
	} else if cooldownActive {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionResizing,
			Status:             metav1.ConditionFalse,
			Reason:             rightsizev1alpha1.ReasonCooldownActive,
			Message:            "Waiting for cooldown period to expire",
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionResizing,
			Status:             metav1.ConditionFalse,
			Reason:             rightsizev1alpha1.ReasonIdle,
			Message:            "No resizes needed",
			ObservedGeneration: policy.Generation,
		})
	}
}

// setScheduleBlockedCondition sets or removes the ScheduleBlocked condition
// based on whether a resize schedule is configured and whether the current
// time falls within the allowed window.
func (r *RightSizePolicyReconciler) setScheduleBlockedCondition(policy *rightsizev1alpha1.RightSizePolicy, withinWindow bool) {
	if policy.Spec.UpdateStrategy.Schedule == nil || len(policy.Spec.UpdateStrategy.Schedule.Windows) == 0 {
		meta.RemoveStatusCondition(&policy.Status.Conditions, rightsizev1alpha1.ConditionScheduleBlocked)
		return
	}

	if !withinWindow {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionScheduleBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonOutsideWindow,
			Message:            "Resizes deferred: current time is outside the configured schedule window",
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionScheduleBlocked,
			Status:             metav1.ConditionFalse,
			Reason:             rightsizev1alpha1.ReasonInsideWindow,
			Message:            "Current time is within the resize schedule window",
			ObservedGeneration: policy.Generation,
		})
	}
}

// setDegradedCondition checks recent resize history for high revert rates.
// If 3+ of the last 5 history entries are reverted, the condition is set.
func (r *RightSizePolicyReconciler) setDegradedCondition(policy *rightsizev1alpha1.RightSizePolicy) {
	history := policy.Status.ResizeHistory
	if len(history) == 0 {
		meta.RemoveStatusCondition(&policy.Status.Conditions, rightsizev1alpha1.ConditionDegraded)
		return
	}

	window := degradedWindowSize
	if len(history) < window {
		window = len(history)
	}
	recent := history[len(history)-window:]
	reverts := 0
	for _, entry := range recent {
		if entry.Result == rightsizev1alpha1.ResizeResultReverted {
			reverts++
		}
	}

	if reverts >= degradedRevertThreshold {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonHighRevertRate,
			Message:            fmt.Sprintf("%d of last %d resizes were reverted; consider adjusting safety margins", reverts, window),
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&policy.Status.Conditions, rightsizev1alpha1.ConditionDegraded)
	}
}

// checkQuotaCompatibility verifies that the target resources don't violate
// LimitRange or ResourceQuota constraints in the namespace.
func (r *RightSizePolicyReconciler) checkQuotaCompatibility(ctx context.Context, namespace string, currentResources, target corev1.ResourceRequirements) error {
	logger := log.FromContext(ctx)

	// Check LimitRange per-container min/max.
	var limitRangeList corev1.LimitRangeList
	if err := r.List(ctx, &limitRangeList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list LimitRanges", "error", err)
	}

	var quotaList corev1.ResourceQuotaList
	if err := r.List(ctx, &quotaList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list ResourceQuotas", "error", err)
	}

	return checkQuotaCompatibilityFromLists(limitRangeList.Items, quotaList.Items, currentResources, target)
}

// checkQuotaCompatibilityFromLists validates that the target resources respect
// pre-fetched LimitRange and ResourceQuota constraints. This avoids redundant
// API calls when multiple pods are checked in the same namespace.
func checkQuotaCompatibilityFromLists(limitRanges []corev1.LimitRange, quotas []corev1.ResourceQuota, currentResources, target corev1.ResourceRequirements) error {
	for _, lr := range limitRanges {
		for _, item := range lr.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer {
				continue
			}
			if minCPU, ok := item.Min[corev1.ResourceCPU]; ok {
				if target.Requests.Cpu().Cmp(minCPU) < 0 {
					return fmt.Errorf("CPU request %s below LimitRange minimum %s", target.Requests.Cpu().String(), minCPU.String())
				}
			}
			if minMem, ok := item.Min[corev1.ResourceMemory]; ok {
				if target.Requests.Memory().Cmp(minMem) < 0 {
					return fmt.Errorf("memory request %s below LimitRange minimum %s", target.Requests.Memory().String(), minMem.String())
				}
			}
			if maxCPU, ok := item.Max[corev1.ResourceCPU]; ok {
				if target.Requests.Cpu().Cmp(maxCPU) > 0 {
					return fmt.Errorf("CPU request %s exceeds LimitRange maximum %s", target.Requests.Cpu().String(), maxCPU.String())
				}
			}
			if maxMem, ok := item.Max[corev1.ResourceMemory]; ok {
				if target.Requests.Memory().Cmp(maxMem) > 0 {
					return fmt.Errorf("memory request %s exceeds LimitRange maximum %s", target.Requests.Memory().String(), maxMem.String())
				}
			}
			// Also validate limits against LimitRange maximums. When
			// ControlledValues=RequestsAndLimits the controller scales
			// limits proportionally, which can exceed LimitRange bounds.
			if target.Limits != nil {
				if maxCPU, ok := item.Max[corev1.ResourceCPU]; ok {
					if limCPU := target.Limits.Cpu(); limCPU != nil && limCPU.Cmp(maxCPU) > 0 {
						return fmt.Errorf("CPU limit %s exceeds LimitRange maximum %s", limCPU.String(), maxCPU.String())
					}
				}
				if maxMem, ok := item.Max[corev1.ResourceMemory]; ok {
					if limMem := target.Limits.Memory(); limMem != nil && limMem.Cmp(maxMem) > 0 {
						return fmt.Errorf("memory limit %s exceeds LimitRange maximum %s", limMem.String(), maxMem.String())
					}
				}
			}
		}
	}

	for _, quota := range quotas {
		if err := checkQuotaHeadroom(quota, currentResources, target); err != nil {
			return err
		}
	}

	return nil
}

// checkQuotaHeadroom verifies that the increase from current to target
// resources fits within the remaining headroom of a ResourceQuota.
func checkQuotaHeadroom(quota corev1.ResourceQuota, current, target corev1.ResourceRequirements) error {
	cpuDelta := target.Requests.Cpu().MilliValue() - current.Requests.Cpu().MilliValue()
	memDelta := target.Requests.Memory().Value() - current.Requests.Memory().Value()

	if cpuDelta > 0 {
		hardCPU, hasHard := quota.Status.Hard[corev1.ResourceRequestsCPU]
		usedCPU, hasUsed := quota.Status.Used[corev1.ResourceRequestsCPU]
		if hasHard && hasUsed {
			headroom := hardCPU.MilliValue() - usedCPU.MilliValue()
			if cpuDelta > headroom {
				return fmt.Errorf("CPU increase of %dm would exceed ResourceQuota %s (headroom: %dm)",
					cpuDelta, quota.Name, headroom)
			}
		}
	}

	if memDelta > 0 {
		hardMem, hasHard := quota.Status.Hard[corev1.ResourceRequestsMemory]
		usedMem, hasUsed := quota.Status.Used[corev1.ResourceRequestsMemory]
		if hasHard && hasUsed {
			headroom := hardMem.Value() - usedMem.Value()
			if memDelta > headroom {
				return fmt.Errorf("memory increase of %s would exceed ResourceQuota %s (headroom: %s)",
					resource.NewQuantity(memDelta, resource.BinarySI).String(),
					quota.Name,
					resource.NewQuantity(headroom, resource.BinarySI).String())
			}
		}
	}

	return nil
}

// consecutiveReverts returns the number of consecutive reverted entries at the
// end of the resize history.
func consecutiveReverts(history []rightsizev1alpha1.ResizeHistoryEntry) int {
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Result == rightsizev1alpha1.ResizeResultReverted {
			count++
		} else {
			break
		}
	}
	return count
}

// updateStatusWithRetry performs a status update with up to 4 attempts
// (3 retries + 1 final) on conflict. On each conflict it re-fetches the
// policy and re-applies the saved status fields, preserving the higher
// Resized count from concurrent reconciles.
func (r *RightSizePolicyReconciler) updateStatusWithRetry(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, key types.NamespacedName) error {
	const maxRetries = 3
	logger := log.FromContext(ctx)

	for attempt := range maxRetries {
		err := r.Status().Update(ctx, policy)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}

		// Conflict: re-fetch and retry, preserving the higher Resized count.
		// A concurrent reconcile may have already set Resized > 0; we must not
		// overwrite it with 0 from our stale snapshot.
		logger.Info("Status update conflict, retrying", "attempt", attempt+1, "maxRetries", maxRetries)
		savedStatus := policy.Status.DeepCopy()
		if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
			return fetchErr
		}
		fetchedResized := policy.Status.Workloads.Resized
		policy.Status = *savedStatus
		if fetchedResized > policy.Status.Workloads.Resized {
			policy.Status.Workloads.Resized = fetchedResized
		}
	}
	return r.Status().Update(ctx, policy)
}

// newSafetyMonitor creates a safety.Monitor with optional throttle checking
// if the metrics collector supports it.
func (r *RightSizePolicyReconciler) newSafetyMonitor(logger logr.Logger, collector rsmetrics.MetricsCollector) *safety.Monitor {
	monitor := safety.NewMonitor(r.Clientset, logger)
	if tc, ok := collector.(safety.ThrottleChecker); ok {
		monitor.WithThrottleChecker(tc, safety.DefaultThrottleThreshold)
	}
	return monitor
}

// autoRevertEnabled returns true when the policy's AutoRevert setting is nil
// (defaulting to true) or explicitly set to true.
func autoRevertEnabled(s rightsizev1alpha1.UpdateStrategy) bool {
	return s.AutoRevert == nil || *s.AutoRevert
}

// getObservationPeriod returns the safety observation period using the
// precedence: safetyObservationPeriod > canary.observationPeriod > default (5m).
func getObservationPeriod(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.SafetyObservationPeriod != nil && policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration
	}
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
	}
	return defaultObservationPeriod
}
