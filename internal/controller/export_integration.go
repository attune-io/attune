//go:build integration

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// PersistResizeAnnotationsForTest exposes persistResizeAnnotations for
// envtest API-fault tests in test/integration. It is compiled only with
// -tags=integration so the production manager binary does not include it.
func (r *AttunePolicyReconciler) PersistResizeAnnotationsForTest(
	ctx context.Context,
	pod *corev1.Pod,
	containerRec attunev1alpha1.ContainerRecommendation,
	policyName, workloadName string,
	now metav1.Time,
	restartCount int32,
) (string, error) {
	return r.persistResizeAnnotations(ctx, pod, containerRec, policyName, workloadName, now, restartCount, "")
}

// MarkResizeTimeForTest exposes markResizeTime so envtest can check that
// the apiserver accepts the per-workload cooldown annotation keys (#1135).
func (r *AttunePolicyReconciler) MarkResizeTimeForTest(
	ctx context.Context, policy *attunev1alpha1.AttunePolicy, workloads ...string,
) error {
	return r.markResizeTime(ctx, policy, workloads...)
}

// WorkloadCooldownActiveForTest exposes isWorkloadCooldownActive.
func (r *AttunePolicyReconciler) WorkloadCooldownActiveForTest(
	policy *attunev1alpha1.AttunePolicy, workload string,
) bool {
	return r.isWorkloadCooldownActive(policy, workload)
}
