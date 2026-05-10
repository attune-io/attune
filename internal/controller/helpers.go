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
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardif/kube-rightsize/internal/resize"
)

// isResizeMode returns true if the policy mode performs actual pod resizes.
func isResizeMode(mode string) bool {
	return mode == "OneShot" || mode == "Canary" || mode == "Auto"
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
		Method:    "InPlace",
		Result:    result,
	}
}

// removeTrackingAnnotations removes the resize-tracking annotations from a pod.
func removeTrackingAnnotations(pod *corev1.Pod) {
	delete(pod.Annotations, "rightsize.io/resized-at")
	delete(pod.Annotations, "rightsize.io/resized-container")
	delete(pod.Annotations, "rightsize.io/original-cpu-request")
	delete(pod.Annotations, "rightsize.io/original-memory-request")
}

// setFailedCondition sets a Ready=False condition on the policy and updates
// the status subresource. Errors from the status update are logged but not
// returned, since the caller typically returns a requeue result regardless.
func (r *RightSizePolicyReconciler) setFailedCondition(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, reason, message string) {
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
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
		return policy.Spec.UpdateStrategy.Cooldown.Duration
	}
	return defaultCooldown
}

// computeSavings calculates the aggregate resource savings across all recommendations.
func (r *RightSizePolicyReconciler) computeSavings(namespace string, recommendations []rightsizev1alpha1.WorkloadRecommendation) rightsizev1alpha1.SavingsStatus {
	var totalCPUSaved, totalMemSaved int64

	for _, rec := range recommendations {
		for _, c := range rec.Containers {
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
	if totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(totalCPUSaved, resource.DecimalSI).String()
		operatormetrics.SavingsCPU.WithLabelValues(namespace).Set(float64(totalCPUSaved) / 1000.0)
	}
	if totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(totalMemSaved, resource.BinarySI).String()
		operatormetrics.SavingsMemory.WithLabelValues(namespace).Set(float64(totalMemSaved))
	}
	return savings
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
	cooldown := r.parseCooldown(policy)
	return time.Since(last) < cooldown
}

// markResizeTime sets the last-resize-time annotation on the policy.
func (r *RightSizePolicyReconciler) markResizeTime(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) error {
	if policy.Annotations == nil {
		policy.Annotations = make(map[string]string)
	}
	policy.Annotations[lastResizeAnnotation] = time.Now().UTC().Format(time.RFC3339)
	return r.Update(ctx, policy)
}

// appendHistory appends new entries to existing history, capping at maxEntries.
//
//nolint:unparam // maxEntries is a parameter for configurability
func appendHistory(existing []rightsizev1alpha1.ResizeHistoryEntry,
	newEntries []rightsizev1alpha1.ResizeHistoryEntry, maxEntries int) []rightsizev1alpha1.ResizeHistoryEntry {
	result := append(existing, newEntries...)
	if len(result) > maxEntries {
		result = result[len(result)-maxEntries:]
	}
	return result
}

// mergeDefaults reads the cluster-scoped RightSizeDefaults and merges values
// into the policy where the policy has not specified its own values.
func (r *RightSizePolicyReconciler) mergeDefaults(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) {
	var defaultsList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &defaultsList); err != nil || len(defaultsList.Items) == 0 {
		return
	}
	defaults := defaultsList.Items[0].Spec

	// Merge CPU config
	if policy.Spec.CPU.Percentile == 0 && defaults.CPU != nil {
		policy.Spec.CPU.Percentile = defaults.CPU.Percentile
	}
	if policy.Spec.CPU.SafetyMargin == "" && defaults.CPU != nil {
		policy.Spec.CPU.SafetyMargin = defaults.CPU.SafetyMargin
	}

	// Merge Memory config
	if policy.Spec.Memory.Percentile == 0 && defaults.Memory != nil {
		policy.Spec.Memory.Percentile = defaults.Memory.Percentile
	}
	if policy.Spec.Memory.SafetyMargin == "" && defaults.Memory != nil {
		policy.Spec.Memory.SafetyMargin = defaults.Memory.SafetyMargin
	}

	// Merge UpdateStrategy mode
	if policy.Spec.UpdateStrategy.Mode == "" && defaults.UpdateStrategy != nil {
		policy.Spec.UpdateStrategy.Mode = defaults.UpdateStrategy.Mode
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
