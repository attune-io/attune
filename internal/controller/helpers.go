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
	"slices"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
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

// savingsAccumulator holds accumulated resource diffs across all recommendations.
type savingsAccumulator struct {
	totalCPU         int64
	totalMem         int64
	totalCPUSaved    int64
	totalMemSaved    int64
	totalCPUIncrease int64
	totalMemIncrease int64
}

// accumulateSavings iterates over recommendations and accumulates resource diffs.
func accumulateSavings(recommendations []rightsizev1alpha1.WorkloadRecommendation) savingsAccumulator {
	var acc savingsAccumulator
	for _, rec := range recommendations {
		for _, c := range rec.Containers {
			acc.totalCPU += c.Current.CPURequest.MilliValue()
			acc.totalMem += c.Current.MemoryRequest.Value()

			cpuDiff := c.Current.CPURequest.MilliValue() - c.Recommended.CPURequest.MilliValue()
			if cpuDiff > 0 {
				acc.totalCPUSaved += cpuDiff
			} else if cpuDiff < 0 {
				acc.totalCPUIncrease += -cpuDiff
			}

			memDiff := c.Current.MemoryRequest.Value() - c.Recommended.MemoryRequest.Value()
			if memDiff > 0 {
				acc.totalMemSaved += memDiff
			} else if memDiff < 0 {
				acc.totalMemIncrease += -memDiff
			}
		}
	}
	return acc
}

// computeSavings calculates the aggregate resource savings across all recommendations.
func (r *RightSizePolicyReconciler) computeSavings(recommendations []rightsizev1alpha1.WorkloadRecommendation, defaults *rightsizev1alpha1.RightSizeDefaults) rightsizev1alpha1.SavingsStatus {
	acc := accumulateSavings(recommendations)

	savings := rightsizev1alpha1.SavingsStatus{}
	if acc.totalCPU > 0 {
		savings.CPURequestTotal = resource.NewMilliQuantity(acc.totalCPU, resource.DecimalSI).String()
	}
	if acc.totalMem > 0 {
		savings.MemoryRequestTotal = resource.NewQuantity(acc.totalMem, resource.BinarySI).String()
	}
	if acc.totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(acc.totalCPUSaved, resource.DecimalSI).String()
	}
	if acc.totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(acc.totalMemSaved, resource.BinarySI).String()
	}
	if acc.totalCPUIncrease > 0 {
		savings.CPURequestIncrease = resource.NewMilliQuantity(acc.totalCPUIncrease, resource.DecimalSI).String()
	}
	if acc.totalMemIncrease > 0 {
		savings.MemoryRequestIncrease = resource.NewQuantity(acc.totalMemIncrease, resource.BinarySI).String()
	}

	cpuPrice, memPrice := getCostPricing(defaults)

	cpuCoresSaved := float64(acc.totalCPUSaved) / 1000.0
	memGiBSaved := float64(acc.totalMemSaved) / (1024 * 1024 * 1024)
	monthlySavings := (cpuCoresSaved*cpuPrice + memGiBSaved*memPrice) * hoursPerMonth
	if monthlySavings > 0 {
		savings.EstimatedMonthlySavings = fmt.Sprintf("$%.2f", monthlySavings)
	}

	cpuCoresIncrease := float64(acc.totalCPUIncrease) / 1000.0
	memGiBIncrease := float64(acc.totalMemIncrease) / (1024 * 1024 * 1024)
	monthlyCostIncrease := (cpuCoresIncrease*cpuPrice + memGiBIncrease*memPrice) * hoursPerMonth
	if monthlyCostIncrease > 0 {
		savings.EstimatedMonthlyCostIncrease = fmt.Sprintf("$%.2f", monthlyCostIncrease)
	}

	return savings
}

// updateSavingsGauges publishes savings metrics to Prometheus gauges.
// Called from Reconcile after computeSavings. Separated so computeSavings
// remains a pure function that tests can call without registering collectors.
func updateSavingsGauges(namespace string, recommendations []rightsizev1alpha1.WorkloadRecommendation, defaults *rightsizev1alpha1.RightSizeDefaults) {
	acc := accumulateSavings(recommendations)

	cpuCoresSaved := float64(acc.totalCPUSaved) / 1000.0
	memGiBSaved := float64(acc.totalMemSaved) / (1024 * 1024 * 1024)
	operatormetrics.SavingsCPU.WithLabelValues(namespace).Set(cpuCoresSaved)
	operatormetrics.SavingsMemory.WithLabelValues(namespace).Set(float64(acc.totalMemSaved))

	cpuPrice, memPrice := getCostPricing(defaults)
	monthlySavings := (cpuCoresSaved*cpuPrice + memGiBSaved*memPrice) * hoursPerMonth
	operatormetrics.SavingsEstimatedMonthly.WithLabelValues(namespace).Set(monthlySavings)
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
// request:limit ratio when the request changes. Protects against int64
// overflow from extreme limit/request ratios.
func scaleLimits(currentReq, currentLim, newReq resource.Quantity) resource.Quantity {
	if currentReq.IsZero() || currentLim.IsZero() {
		// Return zero so buildResizeTarget excludes this limit from the target.
		// Setting limit = request would change the pod's QoS class.
		return resource.Quantity{}
	}
	ratio := float64(currentLim.MilliValue()) / float64(currentReq.MilliValue())
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return newReq.DeepCopy()
	}
	product := float64(newReq.MilliValue()) * ratio
	if product > float64(math.MaxInt64) || product < 0 {
		return currentLim.DeepCopy()
	}
	return *resource.NewMilliQuantity(int64(product), currentLim.Format)
}

// parseFloat64 parses a string as a float64, returning the fallback on error
// or if the value is NaN, Inf, negative, or unreasonably large (>10.0).
// Defense-in-depth: webhook validates first, but this protects when webhooks
// are disabled or bypassed.
func parseFloat64(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 10.0 {
		return fallback
	}
	return v
}

// parseFloat64NonNeg parses a string as a non-negative float64, capped at 1.0.
// Returns fallback on error, NaN, Inf, or negative values.
func parseFloat64NonNeg(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fallback
	}
	if v > 1.0 {
		return 1.0
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

// fetchDefaults fetches the effective defaults for a policy by checking
// namespace-scoped RightSizeNamespaceDefaults first, then falling back to
// cluster-scoped RightSizeDefaults. Returns nil if neither exists.
//
// If multiple defaults objects exist at the same scope, selection is
// deterministic: the lexicographically smallest metadata.name wins.
func (r *RightSizePolicyReconciler) fetchDefaults(ctx context.Context, namespace string) (*rightsizev1alpha1.RightSizeDefaults, error) {
	// Check namespace-scoped defaults first.
	var nsList rightsizev1alpha1.RightSizeNamespaceDefaultsList
	if err := r.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing RightSizeNamespaceDefaults in %s: %w", namespace, err)
	}
	if len(nsList.Items) > 0 {
		nsDefaults := nsList.Items[0]
		for i := 1; i < len(nsList.Items); i++ {
			if nsList.Items[i].Name < nsDefaults.Name {
				nsDefaults = nsList.Items[i]
			}
		}
		// Convert to RightSizeDefaults so callers don't need to know the source.
		return &rightsizev1alpha1.RightSizeDefaults{
			ObjectMeta: nsDefaults.ObjectMeta,
			Spec:       nsDefaults.Spec,
		}, nil
	}

	// Fall back to cluster-scoped defaults.
	var clusterList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &clusterList); err != nil {
		return nil, fmt.Errorf("listing RightSizeDefaults: %w", err)
	}
	if len(clusterList.Items) == 0 {
		return nil, nil
	}
	clusterDefaults := &clusterList.Items[0]
	for i := 1; i < len(clusterList.Items); i++ {
		if clusterList.Items[i].Name < clusterDefaults.Name {
			clusterDefaults = &clusterList.Items[i]
		}
	}
	return clusterDefaults, nil
}

// applyBuiltInDefaults fills strategy and metrics fields still unset after
// mergeDefaults with the operator's built-in default values. This runs AFTER
// mergeDefaults so that cluster-wide RightSizeDefaults take precedence.
//
// Per-resource fields (Percentile, SafetyMargin, Bounds, BurstSensitivity)
// are NOT set here; they are handled defensively at their usage sites in
// buildRecommendationEngines.
func (r *RightSizePolicyReconciler) applyBuiltInDefaults(policy *rightsizev1alpha1.RightSizePolicy) {
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.DefaultUpdateMode
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == nil {
		v := rightsizev1alpha1.DefaultMaxCPUChangePercent
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = &v
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == nil {
		v := rightsizev1alpha1.DefaultMaxMemoryChangePercent
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = &v
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		d, _ := time.ParseDuration(rightsizev1alpha1.DefaultCooldown)
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: d}
	}
	if policy.Spec.UpdateStrategy.AutoRevert == nil {
		v := rightsizev1alpha1.DefaultAutoRevert
		policy.Spec.UpdateStrategy.AutoRevert = &v
	}
	if policy.Spec.UpdateStrategy.ResizeMethod == "" {
		policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.DefaultResizeMethod
	}
	if policy.Spec.MetricsSource.MinimumDataPoints == nil {
		v := rightsizev1alpha1.DefaultMinimumDataPoints
		policy.Spec.MetricsSource.MinimumDataPoints = &v
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		d, _ := time.ParseDuration(rightsizev1alpha1.DefaultHistoryWindow)
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: d}
	}
	if policy.Spec.CPU.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.CPU.ControlledValues = &cv
	}
	if policy.Spec.Memory.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.Memory.ControlledValues = &cv
	}
}

// mergeDefaults merges values from RightSizeDefaults into the policy where
// the policy has not specified its own values.
func (r *RightSizePolicyReconciler) mergeDefaults(policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) {
	if defaults == nil {
		ctrl.Log.V(1).Info("No cluster defaults configured, using built-in values only")
		return
	}
	spec := defaults.Spec

	// Track which fields are inherited for debug logging.
	var inherited []string

	// Merge CPU config
	inherited = append(inherited, mergeResourceConfig(&policy.Spec.CPU, spec.CPU, "cpu")...)

	// Merge Memory config
	inherited = append(inherited, mergeResourceConfig(&policy.Spec.Memory, spec.Memory, "memory")...)

	// Merge MetricsSource
	if spec.MetricsSource != nil {
		if policy.Spec.MetricsSource.HistoryWindow == nil && spec.MetricsSource.HistoryWindow != nil {
			policy.Spec.MetricsSource.HistoryWindow = spec.MetricsSource.HistoryWindow
			inherited = append(inherited, "historyWindow")
		}
		if policy.Spec.MetricsSource.MinimumDataPoints == nil && spec.MetricsSource.MinimumDataPoints != nil {
			policy.Spec.MetricsSource.MinimumDataPoints = spec.MetricsSource.MinimumDataPoints
			inherited = append(inherited, "minimumDataPoints")
		}
		if policy.Spec.MetricsSource.QueryStep == nil && spec.MetricsSource.QueryStep != nil {
			policy.Spec.MetricsSource.QueryStep = spec.MetricsSource.QueryStep
			inherited = append(inherited, "queryStep")
		}
	}

	// Merge UpdateStrategy
	if spec.UpdateStrategy != nil {
		if policy.Spec.UpdateStrategy.Mode == "" {
			policy.Spec.UpdateStrategy.Mode = spec.UpdateStrategy.Mode
			inherited = append(inherited, "mode")
		}
		if policy.Spec.UpdateStrategy.Cooldown == nil && spec.UpdateStrategy.Cooldown != nil {
			policy.Spec.UpdateStrategy.Cooldown = spec.UpdateStrategy.Cooldown
			inherited = append(inherited, "cooldown")
		}
		if policy.Spec.UpdateStrategy.AutoRevert == nil && spec.UpdateStrategy.AutoRevert != nil {
			policy.Spec.UpdateStrategy.AutoRevert = spec.UpdateStrategy.AutoRevert
			inherited = append(inherited, "autoRevert")
		}
		if policy.Spec.UpdateStrategy.ResizeMethod == "" && spec.UpdateStrategy.ResizeMethod != "" {
			policy.Spec.UpdateStrategy.ResizeMethod = spec.UpdateStrategy.ResizeMethod
			inherited = append(inherited, "resizeMethod")
		}
		if policy.Spec.UpdateStrategy.MaxCPUChangePercent == nil && spec.UpdateStrategy.MaxCPUChangePercent != nil {
			policy.Spec.UpdateStrategy.MaxCPUChangePercent = spec.UpdateStrategy.MaxCPUChangePercent
			inherited = append(inherited, "maxCpuChangePercent")
		}
		if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == nil && spec.UpdateStrategy.MaxMemoryChangePercent != nil {
			policy.Spec.UpdateStrategy.MaxMemoryChangePercent = spec.UpdateStrategy.MaxMemoryChangePercent
			inherited = append(inherited, "maxMemoryChangePercent")
		}
		if policy.Spec.UpdateStrategy.MaxConcurrentResizes == 0 && spec.UpdateStrategy.MaxConcurrentResizes != 0 {
			policy.Spec.UpdateStrategy.MaxConcurrentResizes = spec.UpdateStrategy.MaxConcurrentResizes
			inherited = append(inherited, "maxConcurrentResizes")
		}
		if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease == nil && spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
			policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = spec.UpdateStrategy.MaxTotalCPUIncrease
			inherited = append(inherited, "maxTotalCpuIncrease")
		}
		if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease == nil && spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
			policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = spec.UpdateStrategy.MaxTotalMemoryIncrease
			inherited = append(inherited, "maxTotalMemoryIncrease")
		}
		if policy.Spec.UpdateStrategy.Schedule == nil && spec.UpdateStrategy.Schedule != nil {
			policy.Spec.UpdateStrategy.Schedule = spec.UpdateStrategy.Schedule
			inherited = append(inherited, "schedule")
		}
		if policy.Spec.UpdateStrategy.Export == nil && spec.UpdateStrategy.Export != nil {
			policy.Spec.UpdateStrategy.Export = spec.UpdateStrategy.Export
			inherited = append(inherited, "export")
		}
		if policy.Spec.UpdateStrategy.Canary == nil && spec.UpdateStrategy.Canary != nil {
			policy.Spec.UpdateStrategy.Canary = spec.UpdateStrategy.Canary
			inherited = append(inherited, "canary")
		}
	}

	if len(inherited) > 0 {
		ctrl.Log.V(1).Info("Merged cluster defaults into policy",
			"defaultsName", defaults.Name,
			"fieldsInherited", inherited)
	} else {
		ctrl.Log.V(1).Info("All policy fields already set, no defaults applied",
			"defaultsName", defaults.Name)
	}
}

// mergeResourceConfig merges default resource config values into the policy.
func mergeResourceConfig(policy *rightsizev1alpha1.ResourceConfig, defaults *rightsizev1alpha1.ResourceConfig, prefix string) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.Percentile == 0 && defaults.Percentile != 0 {
		policy.Percentile = defaults.Percentile
		inherited = append(inherited, prefix+".percentile")
	}
	if policy.SafetyMargin == "" && defaults.SafetyMargin != "" {
		policy.SafetyMargin = defaults.SafetyMargin
		inherited = append(inherited, prefix+".safetyMargin")
	}
	if policy.Bounds == nil && defaults.Bounds != nil {
		policy.Bounds = defaults.Bounds
		inherited = append(inherited, prefix+".bounds")
	}
	if policy.ControlledValues == nil && defaults.ControlledValues != nil {
		policy.ControlledValues = defaults.ControlledValues
		inherited = append(inherited, prefix+".controlledValues")
	}
	if policy.BurstSensitivity == nil && defaults.BurstSensitivity != nil {
		policy.BurstSensitivity = defaults.BurstSensitivity
		inherited = append(inherited, prefix+".burstSensitivity")
	}
	if policy.AllowDecrease == nil && defaults.AllowDecrease != nil {
		policy.AllowDecrease = defaults.AllowDecrease
		inherited = append(inherited, prefix+".allowDecrease")
	}
	if policy.StartupBoost == nil && defaults.StartupBoost != nil {
		policy.StartupBoost = defaults.StartupBoost
		inherited = append(inherited, prefix+".startupBoost")
	}
	return inherited
}

// isWithinResizeWindow returns true if the current time falls within the
// configured resize schedule. Returns true if no schedule is configured.
func isWithinResizeWindow(schedule *rightsizev1alpha1.ResizeSchedule, now time.Time) bool {
	if schedule == nil {
		return true
	}

	// Load timezone.
	tz := schedule.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Invalid timezone: fail open (allow resize) rather than silently
		// blocking resizes with an undetectable config error.
		return true
	}
	localNow := now.In(loc)

	// Check day-of-week constraint.
	if len(schedule.DaysOfWeek) > 0 {
		dayName := localNow.Weekday().String()
		dayAllowed := false
		for _, d := range schedule.DaysOfWeek {
			if strings.EqualFold(d, dayName) {
				dayAllowed = true
				break
			}
		}
		if !dayAllowed {
			return false
		}
	}

	// Check time windows. If no windows are specified, all times are allowed.
	if len(schedule.Windows) == 0 {
		return true
	}
	nowMinutes := localNow.Hour()*60 + localNow.Minute()
	for _, w := range schedule.Windows {
		start := parseHHMM(w.Start)
		end := parseHHMM(w.End)
		if start < 0 || end < 0 {
			continue
		}
		if start <= end {
			// Normal window: e.g. 02:00-06:00
			if nowMinutes >= start && nowMinutes < end {
				return true
			}
		} else {
			// Overnight window: e.g. 22:00-06:00
			if nowMinutes >= start || nowMinutes < end {
				return true
			}
		}
	}
	return false
}

// parseHHMM parses "HH:MM" into minutes since midnight. Returns -1 on error.
func parseHHMM(s string) int {
	if len(s) != 5 || s[2] != ':' {
		return -1
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
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

// getObservationPeriod returns the safety observation period from the policy's
// canary config, falling back to defaultObservationPeriod.
func getObservationPeriod(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
	}
	return defaultObservationPeriod
}
