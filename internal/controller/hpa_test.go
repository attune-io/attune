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
	"k8s.io/client-go/tools/events"
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
	assert.Equal(t, "80", hpa.Annotations[annotationHPACPUTargetPrefix+"app"])
	assert.Equal(t, "200m", hpa.Annotations[annotationHPACPUBasePrefix+"app"])
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

func cpuResourceMetric(util int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(util),
			},
		},
	}
}

func cpuContainerMetric(container string, util int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ContainerResourceMetricSourceType,
		ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
			Name:      corev1.ResourceCPU,
			Container: container,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(util),
			},
		},
	}
}

func newAutoTuneHPA(name, kind string, annotations map[string]string, metrics ...autoscalingv2.MetricSpec) autoscalingv2.HorizontalPodAutoscaler {
	ann := map[string]string{annotationHPAAutoTune: "true"}
	for k, v := range annotations {
		ann[k] = v
	}
	if kind == "" {
		kind = "Deployment"
	}
	return autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: ann,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: kind,
				Name: "api-server",
			},
			Metrics: metrics,
		},
	}
}

func podContainer(t *testing.T, name, cpuReq, cpuLim string) corev1.Container {
	t.Helper()
	c := corev1.Container{Name: name, Image: "nginx"}
	if cpuReq != "" {
		q, err := resource.ParseQuantity(cpuReq)
		require.NoError(t, err)
		c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: q}
	}
	if cpuLim != "" {
		q, err := resource.ParseQuantity(cpuLim)
		require.NoError(t, err)
		c.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: q}
	}
	return c
}

func workloadPod(name string, containers ...corev1.Container) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": name},
		},
		Spec:   corev1.PodSpec{Containers: containers},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func cpuHistory(container, from, to string) attunev1alpha1.ResizeHistoryEntry {
	return attunev1alpha1.ResizeHistoryEntry{
		Workload:  "api-server",
		Container: container,
		Resource:  "cpu",
		From:      from,
		To:        to,
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}
}

func runHPARetune(t *testing.T, hpas []autoscalingv2.HorizontalPodAutoscaler, pod *corev1.Pod, history []attunev1alpha1.ResizeHistoryEntry, onUpdate func()) client.Client {
	t.Helper()
	scheme := testScheme()
	objs := make([]client.Object, 0, len(hpas))
	for i := range hpas {
		objs = append(objs, hpas[i].DeepCopy())
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	if onUpdate != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				onUpdate()
				return c.Update(ctx, obj, opts...)
			},
		})
	}
	cl := builder.Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	var pods map[string][]corev1.Pod
	if pod != nil {
		pods = map[string][]corev1.Pod{"api-server": {*pod}}
	}
	r.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		history,
		[]attunev1alpha1.WorkloadRecommendation{{
			Workload: "api-server",
			Kind:     "Deployment",
		}},
		hpas, pods)
	return cl
}

func storedHPA(t *testing.T, cl client.Client, name string) autoscalingv2.HorizontalPodAutoscaler {
	t.Helper()
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{
		Name: name, Namespace: "default",
	}, &hpa))
	return hpa
}

func metricUtil(t *testing.T, hpa autoscalingv2.HorizontalPodAutoscaler, idx int) int32 {
	t.Helper()
	require.Less(t, idx, len(hpa.Spec.Metrics))
	m := hpa.Spec.Metrics[idx]
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		require.NotNil(t, m.Resource)
		require.NotNil(t, m.Resource.Target.AverageUtilization)
		return *m.Resource.Target.AverageUtilization
	case autoscalingv2.ContainerResourceMetricSourceType:
		require.NotNil(t, m.ContainerResource)
		require.NotNil(t, m.ContainerResource.Target.AverageUtilization)
		return *m.ContainerResource.Target.AverageUtilization
	default:
		t.Fatalf("metric %d is not a CPU utilization metric", idx)
		return 0
	}
}

func TestRetuneHPAAfterResize_ResourceUsesUnchangedLiveContainer(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(120), metricUtil(t, updated, 0),
		"80 * 600/400 = 120 from pod total, not 80 * 400/200 = 160")
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_PartialStoredBaseUsesPreResizeSum(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "400m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(96), metricUtil(t, updated, 0),
		"80 * 600/500 = 96; 64 keeps the partial base and 80 rebases onto the new total")
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_PartialBaseEventNamesOriginalRequest(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "400m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa.DeepCopy()).Build()
	recorder := events.NewFakeRecorder(4)
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	r.Recorder = recorder
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	r.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")},
		[]attunev1alpha1.WorkloadRecommendation{{Workload: "api-server", Kind: "Deployment"}},
		[]autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {pod}})

	notes := recorderNotes(recorder)
	require.NotEmpty(t, notes)
	assert.Contains(t, notes[0], "HPABaseRepaired")
	assert.Contains(t, notes[0], annotationHPAOriginalCPURequest)
}

func TestRetuneHPAAfterResize_InitHistoryDoesNotShrinkPartialBase(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "400m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	pod.Spec.InitContainers = []corev1.Container{podContainer(t, "migrate", "50m", "1000m")}
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "300m"),
			cpuHistory("migrate", "100m", "50m"),
		}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(96), metricUtil(t, updated, 0),
		"init history is outside the Resource sum: 80 * 600/500 = 96")
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_FullStoredBaseStays(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "600m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(96), metricUtil(t, updated, 0))
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_SecondResizeUsesPodTotal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "600m",
	}, cpuResourceMetric(120))
	pod := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "200m", "150m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(137), metricUtil(t, updated, 0),
		"80 * 600/350 = 137; stored base stays the first pod total")
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_UnparseableCPURowSkipsResource(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "bogus"),
			cpuHistory("sidecar", "200m", "100m"),
		}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPU])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_ZeroContainerDoesNotBlockPodTotal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuContainerMetric("app", 80),
		cpuResourceMetric(80),
	)
	pod := workloadPod("api-server",
		podContainer(t, "app", "0", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "0", "100m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0),
		"zero app ContainerResource is skipped")
	assert.Equal(t, int32(53), metricUtil(t, updated, 1),
		"80 * 200/300 = 53; a zero container must not block the pod total")
}

func TestRetuneHPAAfterResize_TwoHPAsBothUpdated(t *testing.T) {
	t.Parallel()
	deployA := newAutoTuneHPA("hpa-a", "Deployment", nil, cpuResourceMetric(80))
	deployB := newAutoTuneHPA("hpa-b", "Deployment", nil, cpuResourceMetric(50))
	sts := newAutoTuneHPA("hpa-sts", "StatefulSet", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{deployA, deployB, sts}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")}, nil)

	updatedA := storedHPA(t, cl, "hpa-a")
	assert.Equal(t, int32(120), metricUtil(t, updatedA, 0))
	assert.Equal(t, "80", updatedA.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updatedA.Annotations[annotationHPAOriginalCPURequest])

	updatedB := storedHPA(t, cl, "hpa-b")
	assert.Equal(t, int32(75), metricUtil(t, updatedB, 0), "50 * 600/400 = 75")
	assert.Equal(t, "50", updatedB.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updatedB.Annotations[annotationHPAOriginalCPURequest])

	updatedSTS := storedHPA(t, cl, "hpa-sts")
	assert.Equal(t, int32(80), metricUtil(t, updatedSTS, 0))
	assert.Empty(t, updatedSTS.Annotations[annotationHPAOriginalCPU])
	assert.Empty(t, updatedSTS.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_ResourceAndContainerResourceOneUpdate(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(80),
		cpuContainerMetric("app", 80),
	)
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	updates := 0
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")},
		func() { updates++ })

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(120), metricUtil(t, updated, 0))
	assert.Equal(t, int32(160), metricUtil(t, updated, 1), "80 * 400/200 = 160 for app")
	assert.Equal(t, 1, updates, "both metrics are written by one HPA update")
}

func TestRetuneHPAAfterResize_ContainerResourceIgnoresOtherContainer(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuContainerMetric("app", 80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "100m", "50m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations["attune.io/hpa-cpu-target.app"])
	assert.Empty(t, updated.Annotations["attune.io/hpa-cpu-base.app"])
}

func TestRetuneHPAAfterResize_ContainerResourceUsesNamedContainerOnly(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuContainerMetric("sidecar", 80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "100m", "50m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "80 * 100/50 = 160")
	assert.Equal(t, "80", updated.Annotations["attune.io/hpa-cpu-target.sidecar"])
	assert.Equal(t, "100m", updated.Annotations["attune.io/hpa-cpu-base.sidecar"])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPU])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_ContainerResourceSecondStepUsesStoredBase(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		"attune.io/hpa-cpu-target.sidecar": "80",
		"attune.io/hpa-cpu-base.sidecar":   "100m",
	}, cpuContainerMetric("sidecar", 160))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "800m"),
		podContainer(t, "sidecar", "50m", "200m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "50m", "30m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(266), metricUtil(t, updated, 0), "80 * 100/30 = 266")
	assert.Equal(t, "80", updated.Annotations["attune.io/hpa-cpu-target.sidecar"])
	assert.Equal(t, "100m", updated.Annotations["attune.io/hpa-cpu-base.sidecar"])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPU])
}

func TestRetuneHPAAfterResize_ContainerResourceCapUsesContainerLimit(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuContainerMetric("app", 80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "200m"),
		podContainer(t, "sidecar", "200m", "800m"),
	)
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(100), metricUtil(t, updated, 0),
		"app limit equals the new 200m request, so 160 caps at 100, not the pod limit")
}

func TestRetuneHPAAfterResize_InitContainerStaysOutOfPodTotal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(80),
		cpuContainerMetric("migrate", 80),
	)
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	pod.Spec.InitContainers = []corev1.Container{podContainer(t, "migrate", "100m", "1000m")}
	cl := runHPARetune(t, []autoscalingv2.HorizontalPodAutoscaler{hpa}, &pod,
		[]attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "200m"),
			cpuHistory("migrate", "100m", "50m"),
		}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(120), metricUtil(t, updated, 0),
		"init history stays out of the pod total: 80 * 600/400 = 120")
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, int32(160), metricUtil(t, updated, 1),
		"named init ContainerResource uses that container: 80 * 100/50 = 160")
	assert.Equal(t, "80", updated.Annotations["attune.io/hpa-cpu-target.migrate"])
	assert.Equal(t, "100m", updated.Annotations["attune.io/hpa-cpu-base.migrate"])
}

// adjustHPATargets applies one precomputed CPU pair to every adjustable
// CPU utilization metric.
func (r *AttunePolicyReconciler) adjustHPATargets(
	ctx context.Context,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	workloadName, workloadKind string,
	oldCPURequest, newCPURequest, cpuLimit resource.Quantity,
) {
	if oldCPURequest.IsZero() || newCPURequest.IsZero() || oldCPURequest.Equal(newCPURequest) {
		return
	}
	// One app container plus a history row. requestsOnly uses cpuLimit as the
	// live limit cap. The container name matches ContainerResource fixtures
	// that call this helper with container "app".
	const name = "app"
	container := corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: newCPURequest},
		},
	}
	if !cpuLimit.IsZero() {
		container.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: cpuLimit}
	}
	r.tuneHPAs(ctx, hpas, workloadName, workloadKind, hpaTuneScope{
		rows: map[string]hpaCPURow{
			name: {old: oldCPURequest.MilliValue(), neu: newCPURequest.MilliValue(), ok: true},
		},
		pod:          &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}},
		requestsOnly: true,
	})
}

func TestRetuneHPAAfterResize_RequestsAndLimitsCapsAtMultipliedLimit(t *testing.T) {
	t.Parallel()
	scheme := testScheme()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(90),
		cpuContainerMetric("app", 90),
	)
	pod := workloadPod("api-server", podContainer(t, "app", "200m", "200m"))
	cpuLim, err := resource.ParseQuantity("400m")
	require.NoError(t, err)
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.ControlledValues = &both
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa.DeepCopy()).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPULimit: cpuLim,
			},
		}},
	}}
	r.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "800m", "200m")},
		recs,
		[]autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {pod}},
	)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(200), metricUtil(t, updated, 0),
		"90*800/200=360 must cap at multiplied limit 400m/200m=200, not the live 200m limit")
	assert.Equal(t, int32(200), metricUtil(t, updated, 1),
		"container metric uses the same multiplied limit")
	assert.Equal(t, "90", updated.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "800m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "90", updated.Annotations[annotationHPACPUTargetPrefix+"app"])
	assert.Equal(t, "800m", updated.Annotations[annotationHPACPUBasePrefix+"app"])
}
