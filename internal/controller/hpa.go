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
	"strconv"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// retuneHPAAfterResize rebases auto-tune HPA CPU targets from this-cycle
// successful in-place CPU apply (dest-clamped), not the raw recommendation.
func (r *AttunePolicyReconciler) retuneHPAAfterResize(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	mode attunev1alpha1.UpdateType,
	history []attunev1alpha1.ResizeHistoryEntry,
	recommendations []attunev1alpha1.WorkloadRecommendation,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	podsByWorkload map[string][]corev1.Pod,
) {
	if r == nil || len(hpas) == 0 {
		return
	}
	for _, rec := range recommendations {
		if mode == attunev1alpha1.UpdateTypeCanary && policy != nil && !policy.Status.Canary.AllowsHPARetune(rec.Workload) {
			continue
		}
		oldCPU, newCPU, ok := hpaCPUFromResizeHistory(history, rec.Workload)
		if !ok {
			continue
		}
		if oldCPU.Equal(newCPU) {
			continue
		}
		var pods []corev1.Pod
		if podsByWorkload != nil {
			pods = podsByWorkload[rec.Workload]
		}
		cpuLimit := destCPULimitFromPods(pods)
		if !resourceControlledRequestsOnly(policy, corev1.ResourceCPU) {
			// After RequestsAndLimits apply, leftover dest is the applied To.
			// podsByWorkload is the pre-resize list and still has old limits.
			cpuLimit = newCPU.DeepCopy()
		} else if cpuLimit.IsZero() {
			cpuLimit = newCPU.DeepCopy()
		}
		r.adjustHPATargets(ctx, hpas, rec.Workload, rec.Kind, oldCPU, newCPU, cpuLimit)
	}
}

// hpaCPUFromResizeHistory sums From/To on this-cycle successful in-place
// CPU rows for workload. ok is false when no parseable rows exist.
func hpaCPUFromResizeHistory(history []attunev1alpha1.ResizeHistoryEntry, workload string) (oldCPU, newCPU resource.Quantity, ok bool) {
	var oldMilli, newMilli int64
	found := false
	for _, h := range history {
		if h.Workload != workload || h.Resource != "cpu" || !isSuccessfulInPlaceHistory(h) {
			continue
		}
		from, fromErr := resource.ParseQuantity(h.From)
		to, toErr := resource.ParseQuantity(h.To)
		if fromErr != nil || toErr != nil {
			continue
		}
		oldMilli += from.MilliValue()
		newMilli += to.MilliValue()
		found = true
	}
	if !found {
		return resource.Quantity{}, resource.Quantity{}, false
	}
	return *resource.NewMilliQuantity(oldMilli, resource.DecimalSI),
		*resource.NewMilliQuantity(newMilli, resource.DecimalSI), true
}

// destCPULimitFromPods sums leftover dest CPU limits across listed pods.
func destCPULimitFromPods(pods []corev1.Pod) resource.Quantity {
	var total int64
	for i := range pods {
		for _, c := range pods[i].Spec.Containers {
			if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok && !lim.IsZero() {
				total += lim.MilliValue()
			}
		}
	}
	if total == 0 {
		return resource.Quantity{}
	}
	return *resource.NewMilliQuantity(total, resource.DecimalSI)
}

// their resource-based target utilization to maintain the same absolute resource
// threshold after a resize changes the request baseline.
func (r *AttunePolicyReconciler) adjustHPATargets(
	ctx context.Context,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	workloadName, workloadKind string,
	oldCPURequest, newCPURequest, cpuLimit resource.Quantity,
) {
	logger := log.FromContext(ctx)
	for i := range hpas {
		hpa := &hpas[i]
		if hpa.Spec.ScaleTargetRef.Name != workloadName || hpa.Spec.ScaleTargetRef.Kind != workloadKind {
			continue
		}
		if hpa.Annotations == nil || hpa.Annotations[annotationHPAAutoTune] != "true" {
			continue
		}
		if oldCPURequest.IsZero() || newCPURequest.IsZero() || oldCPURequest.Equal(newCPURequest) {
			continue
		}

		adjusted := false
		for j := range hpa.Spec.Metrics {
			m := &hpa.Spec.Metrics[j]
			if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
				continue
			}
			if m.Resource.Name != corev1.ResourceCPU || m.Resource.Target.Type != autoscalingv2.UtilizationMetricType || m.Resource.Target.AverageUtilization == nil {
				continue
			}
			currentTarget := *m.Resource.Target.AverageUtilization
			// Preserve the original absolute CPU threshold across N resizes:
			// newTarget = originalTarget * (originalRequest / newRequest).
			// Using the stored percent with this cycle's old/new request
			// rebases against the last request and drifts (200m@80% -> 400m
			// is 40%; 400m -> 800m must be 20%, not 40%).
			// Legacy HPAs that only have original-target-cpu fall back to
			// currentTarget * (oldRequest / newRequest).
			baseTarget := currentTarget
			baseRequest := oldCPURequest
			storedTarget := hpa.Annotations[annotationHPAOriginalCPU]
			storedRequest := hpa.Annotations[annotationHPAOriginalCPURequest]
			if storedTarget != "" && storedRequest != "" {
				if v, parseErr := strconv.ParseInt(storedTarget, 10, 32); parseErr == nil {
					if q, qErr := resource.ParseQuantity(storedRequest); qErr == nil && !q.IsZero() {
						baseTarget = int32(v)
						baseRequest = q
					}
				}
			}
			// QoS-aware upper cap: Burstable pods (limit > request) can
			// use targets above 100% up to floor(limit/request*100);
			// Guaranteed pods (limit == request) are capped at 100%.
			newTarget := int32(float64(baseTarget) * float64(baseRequest.MilliValue()) / float64(newCPURequest.MilliValue()))
			maxTarget := int32(100)
			if !cpuLimit.IsZero() && cpuLimit.Cmp(newCPURequest) > 0 {
				maxTarget = int32(float64(cpuLimit.MilliValue()) / float64(newCPURequest.MilliValue()) * 100)
			}
			if newTarget > maxTarget {
				newTarget = maxTarget
			}
			if newTarget < 1 {
				newTarget = 1
			}
			if newTarget == currentTarget {
				adjusted = true // no change needed but metric was found
				break
			}

			// Store original target and request on first adjustment only.
			// Do not backfill original-cpu-request on legacy HPAs that
			// already have original-target-cpu; this cycle's old request
			// is not the original.
			if hpa.Annotations[annotationHPAOriginalCPU] == "" {
				if hpa.Annotations == nil {
					hpa.Annotations = make(map[string]string)
				}
				hpa.Annotations[annotationHPAOriginalCPU] = strconv.FormatInt(int64(currentTarget), 10)
				hpa.Annotations[annotationHPAOriginalCPURequest] = oldCPURequest.String()
			}
			logger.Info("Auto-tuning HPA CPU target after resize",
				"hpa", hpa.Name, "workload", workloadName,
				"currentTarget", currentTarget, "newTarget", newTarget,
				"oldRequest", oldCPURequest.String(), "newRequest", newCPURequest.String())
			m.Resource.Target.AverageUtilization = &newTarget
			// Re-fetch the HPA to get a fresh resourceVersion. The HPA list
			// was fetched at the start of Reconcile and the HPA controller
			// may have updated it since then (e.g., during concurrent resizes).
			var fresh autoscalingv2.HorizontalPodAutoscaler
			// Live API read so strip-transformed cache entries cannot wipe metrics.
			if getErr := r.liveReader().Get(ctx, types.NamespacedName{Name: hpa.Name, Namespace: hpa.Namespace}, &fresh); getErr != nil {
				logger.Error(getErr, "Failed to re-fetch HPA for target update", "hpa", hpa.Name)
				break
			}
			// Apply only our operator annotations to the fresh copy.
			// Copying ALL annotations from the stale hpa would overwrite
			// annotations set by other controllers (ArgoCD, Flux, etc.)
			// between the initial List and this re-fetch.
			if fresh.Annotations == nil {
				fresh.Annotations = make(map[string]string)
			}
			if v, ok := hpa.Annotations[annotationHPAAutoTune]; ok {
				fresh.Annotations[annotationHPAAutoTune] = v
			}
			if v, ok := hpa.Annotations[annotationHPAOriginalCPU]; ok {
				fresh.Annotations[annotationHPAOriginalCPU] = v
			}
			if v, ok := hpa.Annotations[annotationHPAOriginalCPURequest]; ok {
				fresh.Annotations[annotationHPAOriginalCPURequest] = v
			}
			for fj := range fresh.Spec.Metrics {
				fm := &fresh.Spec.Metrics[fj]
				if fm.Type == autoscalingv2.ResourceMetricSourceType && fm.Resource != nil &&
					fm.Resource.Name == corev1.ResourceCPU && fm.Resource.Target.AverageUtilization != nil {
					fm.Resource.Target.AverageUtilization = &newTarget
					break
				}
			}
			if err := r.Update(ctx, &fresh); err != nil {
				logger.Error(err, "Failed to update HPA target", "hpa", hpa.Name)
			}
			adjusted = true
			break
		}
		if !adjusted {
			logger.Info("HPA has auto-tune annotation but no adjustable CPU utilization metric",
				"hpa", hpa.Name, "workload", workloadName)
		}
	}
}
