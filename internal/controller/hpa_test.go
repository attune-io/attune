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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestHPACPUFromResizeHistory(t *testing.T) {
	t.Parallel()
	history := []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "cpu",
			From:      "100m",
			To:        "200m",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "api-server",
			Container: "sidecar",
			Resource:  "cpu",
			From:      "50m",
			To:        "50m",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "memory",
			From:      "256Mi",
			To:        "256Mi",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "other",
			Container: "main",
			Resource:  "cpu",
			From:      "1",
			To:        "2",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
	}

	oldCPU, newCPU, ok := hpaCPUFromResizeHistory(history, "api-server")
	require.True(t, ok)
	assert.Equal(t, int64(150), oldCPU.MilliValue())
	assert.Equal(t, int64(250), newCPU.MilliValue())

	_, _, ok = hpaCPUFromResizeHistory(nil, "api-server")
	assert.False(t, ok)

	_, _, ok = hpaCPUFromResizeHistory(history, "missing")
	assert.False(t, ok)
}

func TestHPACPUFromResizeHistory_TwoPodsSameContainerDoesNotDouble(t *testing.T) {
	t.Parallel()
	history := []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "cpu",
			From:      "200m",
			To:        "400m",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "cpu",
			From:      "200m",
			To:        "400m",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
	}
	oldCPU, newCPU, ok := hpaCPUFromResizeHistory(history, "api-server")
	require.True(t, ok)
	assert.Equal(t, int64(200), oldCPU.MilliValue())
	assert.Equal(t, int64(400), newCPU.MilliValue())
}

func TestRetuneHPAAfterResize_UsesAppliedCPU(t *testing.T) {
	t.Parallel()
	scheme := testScheme()
	oldTarget := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "api-server",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}
	pod := newResizePod("api-server", "100m", "256Mi", "200m", "256Mi")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("100m"),
				CPULimit:      resource.MustParse("200m"),
				MemoryRequest: resource.MustParse("256Mi"),
				MemoryLimit:   resource.MustParse("256Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				CPULimit:      resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("256Mi"),
				MemoryLimit:   resource.MustParse("256Mi"),
			},
		}},
	}}
	history := []attunev1alpha1.ResizeHistoryEntry{{
		Workload:  "api-server",
		Container: "main",
		Resource:  "cpu",
		From:      "100m",
		To:        "200m",
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}}

	r.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		history, recs, []autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {*pod}})

	var updated autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server-hpa", Namespace: "default",
	}, &updated))
	require.NotNil(t, updated.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(40), *updated.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"HPA target must be 80*100/200=40 from dest-clamped apply, not 80*100/500=16")
}

func TestRetuneHPAAfterResize_RequestsAndLimitsCapsAtAppliedLimit(t *testing.T) {
	t.Parallel()
	scheme := testScheme()
	oldTarget := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "api-server",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}
	// Pre-resize list still has the old Guaranteed 500m limit.
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.ControlledValues = &cv
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
	}}
	history := []attunev1alpha1.ResizeHistoryEntry{{
		Workload:  "api-server",
		Container: "main",
		Resource:  "cpu",
		From:      "500m",
		To:        "200m",
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}}

	r.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		history, recs, []autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {*pod}})

	var updated autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server-hpa", Namespace: "default",
	}, &updated))
	require.NotNil(t, updated.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(100), *updated.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"R+L apply dest is the new 200m limit; 80*500/200=200 must cap at 100, not 200")
}

func TestAdjustHPATargets_ScalesTargetUtilization(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// CPU went from 200m to 400m, so target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])
}

func TestAdjustHPATargets_ContainerResourceScalesTargetUtilization(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ContainerResourceMetricSourceType,
						ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
							Name:      corev1.ResourceCPU,
							Container: "app",
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// CPU went from 200m to 400m, so target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.NotNil(t, hpa.Spec.Metrics[0].ContainerResource)
	require.NotNil(t, hpa.Spec.Metrics[0].ContainerResource.Target.AverageUtilization)
	assert.Equal(t, int32(40), *hpa.Spec.Metrics[0].ContainerResource.Target.AverageUtilization)
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])
}

func TestAdjustHPATargets_PreservesThirdPartyAnnotations(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)

	// The stale HPA from the initial List has only our annotation.
	staleHPA := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}

	// The "fresh" HPA in the cluster has a third-party annotation added
	// by ArgoCD between the initial List and the re-fetch.
	freshHPA := staleHPA.DeepCopy()
	freshHPA.Annotations["argocd.argoproj.io/managed-by"] = "argo-controller"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(freshHPA).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{staleHPA},
		"my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)

	// Our annotations should be set.
	assert.Equal(t, "true", hpa.Annotations[annotationHPAAutoTune])
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])

	// Third-party annotation must survive (the bug was that the stale
	// copy's annotations overwrote the fresh copy, dropping this).
	assert.Equal(t, "argo-controller", hpa.Annotations["argocd.argoproj.io/managed-by"],
		"third-party annotations must not be overwritten by stale HPA copy")
}

func TestAdjustHPATargets_IdempotentOnSecondCall(t *testing.T) {
	scheme := testScheme()
	origTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &origTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// First call: 200m -> 400m, target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	// Re-fetch the HPA to get updated state.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)

	// Second call with same args (e.g., canary promote). Target should stay 40.
	updatedHPAs := []autoscalingv2.HorizontalPodAutoscaler{hpa}
	r.adjustHPATargets(context.Background(), updatedHPAs, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	err = fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	// Should be 40 (idempotent), not 20 (double-adjusted).
	assert.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])
}

func TestAdjustHPATargets_PreservesAbsoluteThresholdAcrossResizes(t *testing.T) {
	// Two distinct resizes must keep the original absolute threshold
	// (200m * 80% = 160m): 200m@80% -> 400m is 40%; 400m -> 800m is 20%.
	// Rebasing the stored original percent on this cycle's old/new request
	// would leave the second resize at 40%.
	scheme := testScheme()
	origTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &origTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])

	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("400m"), resource.MustParse("800m"), resource.Quantity{})

	err = fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	assert.Equal(t, int32(20), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"second distinct resize must use original request 200m, not last request 400m")
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPAOriginalCPURequest])
}

func TestAdjustHPATargets_LegacyPercentOnlyUsesCurrentTarget(t *testing.T) {
	// HPAs written before original-cpu-request was stored have only the
	// original percent. Do not rebase that percent on this cycle's
	// old/new request (80 * 400/800 = 40). Use currentTarget * old/new
	// (40 * 400/800 = 20) and do not backfill a wrong original request.
	scheme := testScheme()
	current := int32(40)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune:    "true",
				annotationHPAOriginalCPU: "80",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &current,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("400m"), resource.MustParse("800m"), resource.Quantity{})

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "legacy-hpa"}, &stored))
	assert.Equal(t, int32(20), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, "80", stored.Annotations[annotationHPAOriginalCPU])
	assert.Empty(t, stored.Annotations[annotationHPAOriginalCPURequest],
		"must not backfill this cycle's request as the original")
}

func TestAdjustHPATargets_SkipsWithoutAnnotation(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				// No auto-tune annotation.
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	// Target should be unchanged since no annotation.
	assert.Equal(t, int32(80), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_GetErrorDoesNotCrash(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	// HPA in the slice but NOT registered with the fake client, so Get returns NotFound.
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ghost-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	// Empty client: HPA does not exist, Get will fail.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Should not panic; logs the Get error and moves on.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})
}

func TestAdjustHPATargets_UpdateErrorPreservesOriginal(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}

	// Inject an Update error.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return fmt.Errorf("simulated conflict")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Should not panic; logs the Update error.
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	// The stored HPA should still have the original target since update failed.
	var storedHPA autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "conflict-hpa",
	}, &storedHPA)
	require.NoError(t, err)
	assert.Equal(t, int32(80), *storedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_ClampsAbove100(t *testing.T) {
	// When CPU request decreases dramatically, the computed target can exceed
	// 100%. Verify it is clamped to 100.
	scheme := testScheme()
	target := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clamp-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=1000m, new=100m -> 80 * 1000/100 = 800, clamped to 100 (Guaranteed QoS: no limit)
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("1000m"), resource.MustParse("100m"), resource.Quantity{})

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "clamp-hpa"}, &stored))
	assert.Equal(t, int32(100), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_ClampsBelow1(t *testing.T) {
	// When CPU request increases dramatically, the computed target can drop
	// below 1. Verify it is clamped to 1.
	scheme := testScheme()
	target := int32(50)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clamp-low-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=10m, new=10000m -> 50 * 10/10000 = 0.05, int32 = 0, clamped to 1
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("10m"), resource.MustParse("10000m"), resource.Quantity{})

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "clamp-low-hpa"}, &stored))
	assert.Equal(t, int32(1), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_BurstableAllowsAbove100(t *testing.T) {
	// Burstable QoS: limit > request. The computed target can exceed 100%
	// and should be capped at floor(limit/request*100) instead of 100.
	scheme := testScheme()
	target := int32(70)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burstable-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=500m, new=300m, limit=1000m -> 70 * 500/300 = 116
	// Burstable cap: floor(1000/300 * 100) = 333
	// 116 < 333, so newTarget = 116 (not clamped to 100)
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("500m"), resource.MustParse("300m"), resource.MustParse("1000m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "burstable-hpa"}, &stored))
	assert.Equal(t, int32(116), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Burstable pod should allow target above 100")
}

func TestAdjustHPATargets_BurstableCapsAtLimitRatio(t *testing.T) {
	// Burstable QoS: verify the target is capped at floor(limit/request*100)
	// when the computed target exceeds the limit ratio.
	scheme := testScheme()
	target := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burstable-cap-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=1000m, new=100m, limit=200m -> 80 * 1000/100 = 800
	// Burstable cap: floor(200/100 * 100) = 200
	// 800 > 200, so newTarget = 200
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("1000m"), resource.MustParse("100m"), resource.MustParse("200m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "burstable-cap-hpa"}, &stored))
	assert.Equal(t, int32(200), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Burstable target should be capped at floor(limit/request*100)")
}

func TestAdjustHPATargets_GuaranteedCapsAt100(t *testing.T) {
	// Guaranteed QoS: limit == request. Target should be capped at 100
	// even though cpuLimit is set (not zero).
	scheme := testScheme()
	target := int32(70)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guaranteed-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=500m, new=300m, limit=300m (Guaranteed: limit == request)
	// 70 * 500/300 = 116
	// Guaranteed cap: floor(300/300 * 100) = 100
	// 116 > 100, so newTarget = 100
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("500m"), resource.MustParse("300m"), resource.MustParse("300m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "guaranteed-hpa"}, &stored))
	assert.Equal(t, int32(100), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Guaranteed pod (limit==request) should cap at 100")
}
