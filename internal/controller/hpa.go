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
)

// their resource-based target utilization to maintain the same absolute resource
// threshold after a resize changes the request baseline.
func (r *AttunePolicyReconciler) adjustHPATargets(
	ctx context.Context,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	workloadName, workloadKind string,
	oldCPURequest, newCPURequest resource.Quantity,
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
			// Use the stored original target (from first adjustment) to avoid
			// progressive drift on subsequent cycles.
			baseTarget := currentTarget
			if stored := hpa.Annotations[annotationHPAOriginalCPU]; stored != "" {
				if v, parseErr := strconv.ParseInt(stored, 10, 32); parseErr == nil {
					baseTarget = int32(v)
				}
			}
			// newTarget = baseTarget * (oldRequest / newRequest), capped at 100.
			newTarget := int32(float64(baseTarget) * float64(oldCPURequest.MilliValue()) / float64(newCPURequest.MilliValue()))
			if newTarget > 100 {
				newTarget = 100
			}
			if newTarget < 1 {
				newTarget = 1
			}
			if newTarget == currentTarget {
				adjusted = true // no change needed but metric was found
				break
			}

			// Store original target for rollback if not already stored.
			if hpa.Annotations[annotationHPAOriginalCPU] == "" {
				if hpa.Annotations == nil {
					hpa.Annotations = make(map[string]string)
				}
				hpa.Annotations[annotationHPAOriginalCPU] = strconv.FormatInt(int64(currentTarget), 10)
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
			if getErr := r.Get(ctx, types.NamespacedName{Name: hpa.Name, Namespace: hpa.Namespace}, &fresh); getErr != nil {
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
