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

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardif/kube-rightsize/internal/metrics"
	"github.com/SebTardif/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardif/kube-rightsize/internal/resize"
	"github.com/SebTardif/kube-rightsize/internal/safety"
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
func isResizeMode(mode string) bool {
	return mode == rightsizev1alpha1.ModeOneShot || mode == rightsizev1alpha1.ModeCanary || mode == rightsizev1alpha1.ModeAuto
}

// newHistoryEntry creates a ResizeHistoryEntry from a resize result.
func newHistoryEntry(now metav1.Time, workload, container string, res resize.ResizeResult, result string) rightsizev1alpha1.ResizeHistoryEntry {
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
			delete(pod.Annotations, annotationOriginalRestartCountPrefix+name)
		}
	}
	delete(pod.Annotations, annotationResizedAt)
	delete(pod.Annotations, annotationResizedContainers)
	delete(pod.Annotations, annotationResizedWorkload)
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
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               rightsizev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if err := r.Status().Update(ctx, policy); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update status")
	}
}

// parseHistoryWindow parses the history window duration from the policy.
func (r *RightSizePolicyReconciler) parseHistoryWindow(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.MetricsSource.HistoryWindow != nil {
		return policy.Spec.MetricsSource.HistoryWindow.Duration
	}
	return defaultHistoryWindow
}

// getMinimumDataPoints returns the minimum data points threshold from the policy.
func (r *RightSizePolicyReconciler) getMinimumDataPoints(policy *rightsizev1alpha1.RightSizePolicy) int32 {
	if policy.Spec.MetricsSource.MinimumDataPoints > 0 {
		return policy.Spec.MetricsSource.MinimumDataPoints
	}
	return defaultMinimumDataPoints
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

const (
	// Default on-demand Linux pricing (approximate).
	defaultCPUPerCoreHour = 0.031
	defaultMemPerGiBHour  = 0.004
	hoursPerMonth         = 730
)

// computeSavings calculates the aggregate resource savings across all recommendations.
func (r *RightSizePolicyReconciler) computeSavings(namespace string, recommendations []rightsizev1alpha1.WorkloadRecommendation, defaults *rightsizev1alpha1.RightSizeDefaults) rightsizev1alpha1.SavingsStatus {
	var totalCPUSaved, totalMemSaved int64
	var totalCPU, totalMem int64

	for _, rec := range recommendations {
		for _, c := range rec.Containers {
			totalCPU += c.Current.CPURequest.MilliValue()
			totalMem += c.Current.MemoryRequest.Value()

			cpuDiff := c.Current.CPURequest.MilliValue() - c.Recommended.CPURequest.MilliValue()
			if cpuDiff > 0 {
				totalCPUSaved += cpuDiff
			}

			memDiff := c.Current.MemoryRequest.Value() - c.Recommended.MemoryRequest.Value()
			if memDiff > 0 {
				totalMemSaved += memDiff
			}
		}
	}

	savings := rightsizev1alpha1.SavingsStatus{}
	if totalCPU > 0 {
		savings.CPURequestTotal = resource.NewMilliQuantity(totalCPU, resource.DecimalSI).String()
	}
	if totalMem > 0 {
		savings.MemoryRequestTotal = resource.NewQuantity(totalMem, resource.BinarySI).String()
	}
	if totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(totalCPUSaved, resource.DecimalSI).String()
		operatormetrics.SavingsCPU.WithLabelValues(namespace).Set(float64(totalCPUSaved) / 1000.0)
	}
	if totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(totalMemSaved, resource.BinarySI).String()
		operatormetrics.SavingsMemory.WithLabelValues(namespace).Set(float64(totalMemSaved))
	}

	// Compute estimated monthly cost savings.
	if totalCPUSaved > 0 || totalMemSaved > 0 {
		cpuPrice, memPrice := getCostPricing(defaults)
		cpuCoresSaved := float64(totalCPUSaved) / 1000.0
		memGiBSaved := float64(totalMemSaved) / (1024 * 1024 * 1024)
		monthlySavings := (cpuCoresSaved*cpuPrice + memGiBSaved*memPrice) * hoursPerMonth
		savings.EstimatedMonthlySavings = fmt.Sprintf("$%.2f", monthlySavings)
		operatormetrics.SavingsEstimatedMonthly.WithLabelValues(namespace).Set(monthlySavings)
	}

	return savings
}

// getCostPricing reads pricing from RightSizeDefaults, falling back to defaults.
func getCostPricing(defaults *rightsizev1alpha1.RightSizeDefaults) (cpuPerCoreHour, memPerGiBHour float64) {
	cpuPerCoreHour = defaultCPUPerCoreHour
	memPerGiBHour = defaultMemPerGiBHour

	if defaults == nil {
		return
	}

	pricing := defaults.Spec.CostPricing
	if pricing == nil {
		return
	}

	if v := parseFloat64(pricing.CPUPerCoreHour, 0); v > 0 {
		cpuPerCoreHour = v
	}
	if v := parseFloat64(pricing.MemoryPerGiBHour, 0); v > 0 {
		memPerGiBHour = v
	}
	return
}

// scaleLimits scales a resource limit proportionally to maintain the same
// request:limit ratio when the request changes.
func scaleLimits(currentReq, currentLim, newReq resource.Quantity) resource.Quantity {
	if currentReq.IsZero() || currentLim.IsZero() {
		return newReq.DeepCopy()
	}
	ratio := float64(currentLim.MilliValue()) / float64(currentReq.MilliValue())
	newLimMilli := int64(float64(newReq.MilliValue()) * ratio)
	return *resource.NewMilliQuantity(newLimMilli, currentLim.Format)
}

// parseFloat64 parses a string as a float64, returning the fallback on error.
func parseFloat64(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}

// safeInt32 converts an int to int32, clamping to math.MaxInt32 on overflow.
func safeInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v) // #nosec G115 -- overflow guarded by check above
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
	return time.Since(last) < cooldown
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

// markResizeTime sets the last-resize-time annotation on the policy using a
// merge patch to avoid 409 Conflict with concurrent spec changes.
func (r *RightSizePolicyReconciler) markResizeTime(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) error {
	patch := client.MergeFrom(policy.DeepCopy())
	if policy.Annotations == nil {
		policy.Annotations = make(map[string]string)
	}
	policy.Annotations[lastResizeAnnotation] = time.Now().UTC().Format(time.RFC3339)
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
		if entry.Result == rightsizev1alpha1.ResultReverted {
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
	} else {
		for _, lr := range limitRangeList.Items {
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
			}
		}
	}

	// Check ResourceQuota: verify the delta won't exceed quota headroom.
	var quotaList corev1.ResourceQuotaList
	if err := r.List(ctx, &quotaList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list ResourceQuotas", "error", err)
		return nil
	}
	for _, quota := range quotaList.Items {
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
				return fmt.Errorf("memory increase of %d bytes would exceed ResourceQuota %s (headroom: %d bytes)",
					memDelta, quota.Name, headroom)
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
		if history[i].Result == rightsizev1alpha1.ResultReverted {
			count++
		} else {
			break
		}
	}
	return count
}

// fetchDefaults fetches the cluster-scoped RightSizeDefaults once per reconciliation.
// Returns nil if no defaults exist or on error.
func (r *RightSizePolicyReconciler) fetchDefaults(ctx context.Context) *rightsizev1alpha1.RightSizeDefaults {
	var defaultsList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &defaultsList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list RightSizeDefaults")
		return nil
	}
	if len(defaultsList.Items) == 0 {
		return nil
	}
	return &defaultsList.Items[0]
}

// mergeDefaults merges values from RightSizeDefaults into the policy where
// the policy has not specified its own values.
func (r *RightSizePolicyReconciler) mergeDefaults(policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) {
	if defaults == nil {
		return
	}
	spec := defaults.Spec

	// Merge CPU config
	if policy.Spec.CPU.Percentile == 0 && spec.CPU != nil {
		policy.Spec.CPU.Percentile = spec.CPU.Percentile
	}
	if policy.Spec.CPU.SafetyMargin == "" && spec.CPU != nil {
		policy.Spec.CPU.SafetyMargin = spec.CPU.SafetyMargin
	}

	// Merge Memory config
	if policy.Spec.Memory.Percentile == 0 && spec.Memory != nil {
		policy.Spec.Memory.Percentile = spec.Memory.Percentile
	}
	if policy.Spec.Memory.SafetyMargin == "" && spec.Memory != nil {
		policy.Spec.Memory.SafetyMargin = spec.Memory.SafetyMargin
	}

	// Merge UpdateStrategy mode
	if policy.Spec.UpdateStrategy.Mode == "" && spec.UpdateStrategy != nil {
		policy.Spec.UpdateStrategy.Mode = spec.UpdateStrategy.Mode
	}
}

// updateStatusWithRetry performs a status update with a retry on conflict.
// If the first attempt fails with a conflict, it re-fetches the policy,
// re-applies the status fields, and retries once.
func (r *RightSizePolicyReconciler) updateStatusWithRetry(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, key types.NamespacedName) error {
	err := r.Status().Update(ctx, policy)
	if err == nil {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return err
	}

	// Conflict: re-fetch and retry.
	logger := log.FromContext(ctx)
	logger.Info("Status update conflict, retrying")
	savedStatus := policy.Status.DeepCopy()
	if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
		return fetchErr
	}
	policy.Status = *savedStatus
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

// getObservationPeriod returns the safety observation period from the policy's
// canary config, falling back to defaultObservationPeriod.
func getObservationPeriod(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
	}
	return defaultObservationPeriod
}
