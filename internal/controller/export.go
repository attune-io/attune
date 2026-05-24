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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

// exportRecommendationConfigMaps creates or updates ConfigMaps with
// recommendation data for GitOps workflows.
func (r *RightSizePolicyReconciler) exportRecommendationConfigMaps(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	recommendations []rightsizev1alpha1.WorkloadRecommendation,
) {
	logger := log.FromContext(ctx)
	for _, rec := range recommendations {
		cmName := fmt.Sprintf("%s-%s-recommendations", policy.Name, rec.Workload)
		data := map[string]string{
			"workload": rec.Workload,
			"kind":     rec.Kind,
		}
		for _, c := range rec.Containers {
			prefix := c.Name + "."
			data[prefix+"cpu-request"] = c.Recommended.CPURequest.String()
			data[prefix+"memory-request"] = c.Recommended.MemoryRequest.String()
			if !c.Recommended.CPULimit.IsZero() {
				data[prefix+"cpu-limit"] = c.Recommended.CPULimit.String()
			}
			if !c.Recommended.MemoryLimit.IsZero() {
				data[prefix+"memory-limit"] = c.Recommended.MemoryLimit.String()
			}
			data[prefix+"confidence"] = fmt.Sprintf("%.2f", c.Confidence)
		}
		data["last-updated"] = r.now().UTC().Format(time.RFC3339)

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: policy.Namespace,
				Labels: map[string]string{
					"rightsize.io/policy":   policy.Name,
					"rightsize.io/workload": rec.Workload,
				},
			},
			Data: data,
		}
		if err := ctrl.SetControllerReference(policy, cm, r.Scheme); err != nil {
			logger.Error(err, "Failed to set owner reference on recommendation ConfigMap", "configmap", cmName)
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ExportFailed", "export",
				"Failed to export recommendations to ConfigMap %s: %v", cmName, err)
			continue
		}

		var existing corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKeyFromObject(cm), &existing); err != nil {
			if apierrors.IsNotFound(err) {
				if createErr := r.Create(ctx, cm); createErr != nil {
					logger.Error(createErr, "Failed to create recommendation ConfigMap", "configmap", cmName)
					r.emitEventOnce(policy, corev1.EventTypeWarning, "ExportFailed", "export",
						"Failed to create recommendation ConfigMap %s: %v", cmName, createErr)
				}
			} else {
				logger.Error(err, "Failed to check recommendation ConfigMap", "configmap", cmName)
			}
			continue
		}
		existing.Data = data
		// Merge operator labels into existing labels instead of replacing
		// all labels. This preserves labels set by users, GitOps tools, or
		// other controllers.
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
		}
		for k, v := range cm.Labels {
			existing.Labels[k] = v
		}
		if updateErr := r.Update(ctx, &existing); updateErr != nil {
			logger.Error(updateErr, "Failed to update recommendation ConfigMap", "configmap", cmName)
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ExportFailed", "export",
				"Failed to update recommendation ConfigMap %s: %v", cmName, updateErr)
		} else {
			logger.V(1).Info("Exported recommendations to ConfigMap",
				"configMap", cmName, "workload", rec.Workload)
		}
	}
}
