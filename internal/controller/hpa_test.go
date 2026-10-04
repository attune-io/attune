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
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
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
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// runHPARetuneWithEvents runs one retune with a fake recorder and returns
// the client and the recorded event notes.
func runHPARetuneWithEvents(t *testing.T, hpa autoscalingv2.HorizontalPodAutoscaler, pod corev1.Pod, history []attunev1alpha1.ResizeHistoryEntry, bounds ...*attunev1alpha1.HPATargetBounds) (client.Client, []string) {
	t.Helper()
	cl, notes, _ := runHPARetuneWithLogs(t, hpa, pod, history, bounds...)
	return cl, notes
}

// runHPARetuneWithLogs is runHPARetuneWithEvents that also returns the
// controller's log output, one JSON object per line.
func runHPARetuneWithLogs(t *testing.T, hpa autoscalingv2.HorizontalPodAutoscaler, pod corev1.Pod, history []attunev1alpha1.ResizeHistoryEntry, bounds ...*attunev1alpha1.HPATargetBounds) (client.Client, []string, string) {
	t.Helper()
	return runHPARetuneWithPods(t, hpa, []corev1.Pod{pod}, history, bounds...)
}

// runHPARetuneWithPods is runHPARetuneWithLogs for several workload pods.
// The first pod with a CPU request is the sampled pod.
func runHPARetuneWithPods(t *testing.T, hpa autoscalingv2.HorizontalPodAutoscaler, pods []corev1.Pod, history []attunev1alpha1.ResizeHistoryEntry, bounds ...*attunev1alpha1.HPATargetBounds) (client.Client, []string, string) {
	t.Helper()
	var logged string
	logger := funcr.NewJSON(func(obj string) { logged += obj + "\n" }, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)
	scheme := testScheme()
	in := hpa.DeepCopy()
	in.ResourceVersion = ""
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(in).Build()
	recorder := events.NewFakeRecorder(8)
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	r.Recorder = recorder
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	if len(bounds) > 0 {
		policy.Spec.UpdateStrategy.HPATargetBounds = bounds[0]
	}
	r.retuneHPAAfterResize(ctx, policy, attunev1alpha1.UpdateTypeAuto,
		history,
		[]attunev1alpha1.WorkloadRecommendation{{Workload: "api-server", Kind: "Deployment"}},
		[]autoscalingv2.HorizontalPodAutoscaler{*in},
		map[string][]corev1.Pod{"api-server": pods})
	return cl, recorderNotes(recorder), logged
}

const (
	logMalformedCPUList = "HPA CPU base container list is malformed"
	logRemovedCPUList   = "HPA CPU base lists containers that are not on the pod"
)

func assertNoBaseRepaired(t *testing.T, notes []string) {
	t.Helper()
	for _, n := range notes {
		assert.NotContains(t, n, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_StoredBaseGrowth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		storedBase  string
		hpaUtil     int32
		appFrom     string
		appTo       string
		sidecar     string
		wantTarget  int32
		wantBase    string
		wantMessage string
	}{
		{
			name:       "FullStoredBaseIgnoresLaterGrowth",
			storedBase: "600m", hpaUtil: 68, appFrom: "500m", appTo: "550m", sidecar: "200m",
			wantTarget: 64, wantBase: "600m",
			wantMessage: "80 * 600/750 = 64; 74 rewrites the base to 700m",
		},
		{
			name:       "ElseBranchDoesNotDoubleCountSidecar",
			storedBase: "600m", hpaUtil: 53, appFrom: "700m", appTo: "750m", sidecar: "200m",
			wantTarget: 50, wantBase: "600m",
			wantMessage: "80 * 600/950 = 50; 67 adds the sidecar again (800m)",
		},
		{
			name:       "PartialBaseAfterShrinkStaysPartial",
			storedBase: "400m", hpaUtil: 80, appFrom: "300m", appTo: "250m", sidecar: "200m",
			wantTarget: 71, wantBase: "400m",
			wantMessage: "80 * 400/450 = 71; stored 400m != history old 300m, so no repair (88 uses 500m)",
		},
		{
			name:       "FullBaseCoincidentalEqualityKeepsBase",
			storedBase: "600m", hpaUtil: 60, appFrom: "600m", appTo: "650m", sidecar: "200m",
			wantTarget: 56, wantBase: "600m",
			wantMessage: "current 60 = 80 * 600/800 reproduces the stored pair: 80 * 600/850 = 56, not 75 from 800m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
				annotationHPAOriginalCPU:        "80",
				annotationHPAOriginalCPURequest: tt.storedBase,
			}, cpuResourceMetric(tt.hpaUtil))
			pod := workloadPod("api-server",
				podContainer(t, "app", tt.appTo, "1000m"),
				podContainer(t, "sidecar", tt.sidecar, "1000m"),
			)
			cl, notes := runHPARetuneWithEvents(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", tt.appFrom, tt.appTo)})

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, tt.wantTarget, metricUtil(t, updated, 0), tt.wantMessage)
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, tt.wantBase, updated.Annotations[annotationHPAOriginalCPURequest])
			assertNoBaseRepaired(t, notes)
		})
	}
}

// TestRetuneHPAAfterResize_StoredPairVeto covers the veto on the partial
// base repair: it applies when the target the stored pair publishes for
// this cycle's pre-resize sum, after the band and the limit cap, equals the
// current target exactly. A clamped match is ambiguous and keeps the base.
func TestRetuneHPAAfterResize_StoredPairVeto(t *testing.T) {
	t.Parallel()
	min80 := int32(80)
	min10 := int32(10)
	tests := []struct {
		name         string
		storedTarget string
		storedBase   string
		hpaUtil      int32
		appFrom      string
		appTo        string
		appLimit     string
		bandMin      *int32
		wantTarget   int32
		wantBase     string
		wantRepair   bool
		wantMessage  string
	}{
		{
			name:         "BandMinClampAmbiguousKeepsBase",
			storedTarget: "80", storedBase: "400m", hpaUtil: 80, appFrom: "400m", appTo: "300m", appLimit: "1000m",
			bandMin:    &min80,
			wantTarget: 80, wantBase: "400m", wantRepair: false,
			wantMessage: "80 * 400/600 = 53 publishes Min 80 = current: ambiguous, keep 400m; 80 * 400/500 = 64 stays at Min 80",
		},
		{
			name:         "LimitCapClampAmbiguousKeepsBase",
			storedTarget: "180", storedBase: "400m", hpaUtil: 110, appFrom: "400m", appTo: "300m", appLimit: "460m",
			wantTarget: 132, wantBase: "400m", wantRepair: false,
			wantMessage: "180 * 400/600 = 120 publishes the cap 660/600 = 110 = current: keep 400m; 144 capped to 660/500 = 132",
		},
		{
			name:         "OffByOneTargetDoesNotVeto",
			storedTarget: "80", storedBase: "400m", hpaUtil: 54, appFrom: "400m", appTo: "300m", appLimit: "1000m",
			wantTarget: 96, wantBase: "600m", wantRepair: true,
			wantMessage: "80 * 400/600 = 53 != 54; the operator's own write reproduces exactly, so repair to 96",
		},
		{
			name:         "NonBindingBandKeepsVeto",
			storedTarget: "80", storedBase: "600m", hpaUtil: 60, appFrom: "600m", appTo: "650m", appLimit: "1000m",
			bandMin:    &min10,
			wantTarget: 56, wantBase: "600m", wantRepair: false,
			wantMessage: "Min 10 does not bind 80 * 600/800 = 60, so the veto holds: 80 * 600/850 = 56",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
				annotationHPAOriginalCPU:        tt.storedTarget,
				annotationHPAOriginalCPURequest: tt.storedBase,
			}, cpuResourceMetric(tt.hpaUtil))
			pod := workloadPod("api-server",
				podContainer(t, "app", tt.appTo, tt.appLimit),
				podContainer(t, "sidecar", "200m", "200m"),
			)
			var bounds *attunev1alpha1.HPATargetBounds
			if tt.bandMin != nil {
				bounds = &attunev1alpha1.HPATargetBounds{CPU: &attunev1alpha1.HPATargetBound{Min: tt.bandMin}}
			}
			cl, notes := runHPARetuneWithEvents(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", tt.appFrom, tt.appTo)}, bounds)

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, tt.wantTarget, metricUtil(t, updated, 0), tt.wantMessage)
			assert.Equal(t, tt.storedTarget, updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, tt.wantBase, updated.Annotations[annotationHPAOriginalCPURequest])
			repaired := false
			for _, n := range notes {
				if strings.Contains(n, "HPABaseRepaired") {
					repaired = true
				}
			}
			assert.Equal(t, tt.wantRepair, repaired, "HPABaseRepaired event")
		})
	}
}

// TestRetuneHPAAfterResize_FullBaseBandMinClampKeepsBase walks a full base
// through growth under a binding band Min and then a shrink whose history
// old equals the stored base. The clamped target matches what the stored
// pair publishes, so the base is kept rather than rewritten to the pod sum.
func TestRetuneHPAAfterResize_FullBaseBandMinClampKeepsBase(t *testing.T) {
	t.Parallel()
	min80 := int32(80)
	bounds := &attunev1alpha1.HPATargetBounds{CPU: &attunev1alpha1.HPATargetBound{Min: &min80}}
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "600m",
	}, cpuResourceMetric(80))
	steps := []struct{ from, to string }{
		{"400m", "500m"},
		{"500m", "600m"},
		{"600m", "400m"},
	}
	for _, st := range steps {
		pod := workloadPod("api-server",
			podContainer(t, "app", st.to, "1000m"),
			podContainer(t, "sidecar", "200m", "1000m"),
		)
		cl, notes := runHPARetuneWithEvents(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", st.from, st.to)}, bounds)
		hpa = storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, int32(80), metricUtil(t, hpa, 0), "app %s->%s: target stays at Min 80", st.from, st.to)
		assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
		assert.Equal(t, "600m", hpa.Annotations[annotationHPAOriginalCPURequest],
			"app %s->%s: full base kept (repair would write 800m, target 106)", st.from, st.to)
		assertNoBaseRepaired(t, notes)
	}
}

func TestRetuneHPAAfterResize_FullStoredBaseThirdResizeKeepsOriginal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "600m",
	}, cpuResourceMetric(68))
	pod := workloadPod("api-server",
		podContainer(t, "app", "550m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, notes := runHPARetuneWithEvents(t, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")})
	first := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(64), metricUtil(t, first, 0), "80 * 600/750 = 64")
	assert.Equal(t, "600m", first.Annotations[annotationHPAOriginalCPURequest])
	assertNoBaseRepaired(t, notes)

	pod = workloadPod("api-server",
		podContainer(t, "app", "600m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, notes = runHPARetuneWithEvents(t, first, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "550m", "600m")})
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(60), metricUtil(t, second, 0), "80 * 600/800 = 60")
	assert.Equal(t, "80", second.Annotations[annotationHPAOriginalCPU])
	assert.Equal(t, "600m", second.Annotations[annotationHPAOriginalCPURequest])
	assertNoBaseRepaired(t, notes)
}

// cpuBaseAnnotations is a stored pod CPU pair with target 80. A list value,
// even an empty one, is set when given.
func cpuBaseAnnotations(base string, list ...string) map[string]string {
	ann := map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: base,
	}
	if len(list) > 0 {
		ann[annotationHPAOriginalCPURequestContainers] = list[0]
	}
	return ann
}

func hasBaseRepaired(notes []string) bool {
	for _, n := range notes {
		if strings.Contains(n, "HPABaseRepaired") && strings.Contains(n, annotationHPAOriginalCPURequest) {
			return true
		}
	}
	return false
}

func nativeSidecar(t *testing.T, name, cpuReq string) corev1.Container {
	t.Helper()
	c := podContainer(t, name, cpuReq, "1000m")
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

func TestRetuneHPAAfterResize_FirstWriteStoresCPUContainerList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ann  map[string]string
	}{
		{name: "NoAttuneKeys"},
		{name: "StaleListWithoutBaseIsOverwritten", ann: map[string]string{annotationHPAOriginalCPURequestContainers: "old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", tt.ann, cpuResourceMetric(80))
			pod := workloadPod("api-server",
				podContainer(t, "sidecar", "200m", "1000m"),
				podContainer(t, "app", "200m", "1000m"),
			)
			pod.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "100m")}
			cl, notes := runHPARetuneWithEvents(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")})

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(120), metricUtil(t, updated, 0), "80 * 600/400 = 120; native sidecar mesh is outside the sum")
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
			assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers],
				"first write lists app+sidecar sorted; mesh is not in the Resource sum")
			assertNoBaseRepaired(t, notes)
		})
	}
}

// TestRetuneHPAAfterResize_FirstWriteListsOffPodRow covers a history row
// for a container that is not on the sampled pod: podCPUMillis sums it, so
// the list names it.
func TestRetuneHPAAfterResize_FirstWriteListsOffPodRow(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"))
	cl, notes := runHPARetuneWithEvents(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		cpuHistory("app", "400m", "200m"),
		cpuHistory("gone", "100m", "50m"),
	})

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "80 * (400+100)/(200+50) = 160")
	assert.Equal(t, "500m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,gone", updated.Annotations[annotationHPAOriginalCPURequestContainers],
		"off-pod row gone is in the base, so it is listed")
	assertNoBaseRepaired(t, notes)
}

func TestRetuneHPAAfterResize_CPUListRepairsMissingContainer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		storedBase  string
		list        string
		hpaUtil     int32
		containers  func(t *testing.T) []corev1.Container
		history     []attunev1alpha1.ResizeHistoryEntry
		wantTarget  int32
		wantBase    string
		wantList    string
		wantEvent   bool
		wantMessage string
	}{
		{
			name: "SidecarAddedAfterFirstWrite", storedBase: "400m", list: "app", hpaUtil: 70,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{podContainer(t, "app", "550m", "1000m"), podContainer(t, "sidecar", "200m", "1000m")}
			},
			history:    []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")},
			wantTarget: 64, wantBase: "600m", wantList: "app,sidecar", wantEvent: true,
			wantMessage: "80 * (400+200)/750 = 64; the legacy rule keeps 400m: 80 * 400/750 = 42",
		},
		{
			name: "LargeSidecarAddedAfterFirstWrite", storedBase: "400m", list: "app", hpaUtil: 70,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{podContainer(t, "app", "550m", "1000m"), podContainer(t, "sidecar", "2000m", "4000m")}
			},
			history:    []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")},
			wantTarget: 75, wantBase: "2400m", wantList: "app,sidecar", wantEvent: true,
			wantMessage: "80 * (400+2000)/2550 = 75 (cap 5000/2550 = 196); the legacy rule gives 80 * 400/2550 = 12",
		},
		{
			name: "SidecarAddedAfterShrink", storedBase: "400m", list: "app", hpaUtil: 80,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{podContainer(t, "app", "250m", "1000m"), podContainer(t, "sidecar", "200m", "1000m")}
			},
			history:    []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "300m", "250m")},
			wantTarget: 106, wantBase: "600m", wantList: "app,sidecar", wantEvent: true,
			wantMessage: "80 * (400+200)/450 = 106; the legacy rule keeps 400m: 80 * 400/450 = 71",
		},
		{
			name: "MissingContainerResizedThisCycle", storedBase: "400m", list: "app", hpaUtil: 70,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{podContainer(t, "app", "400m", "1000m"), podContainer(t, "sidecar", "100m", "1000m")}
			},
			history:    []attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "200m", "100m")},
			wantTarget: 96, wantBase: "600m", wantList: "app,sidecar", wantEvent: true,
			wantMessage: "sidecar is added at its row From 200m: 80 * 600/500 = 96; the legacy rule gives 80 * 400/500 = 64",
		},
		{
			name: "UnlistedOffPodRowAdded", storedBase: "400m", list: "app", hpaUtil: 80,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{podContainer(t, "app", "300m", "1000m")}
			},
			history: []attunev1alpha1.ResizeHistoryEntry{
				cpuHistory("app", "400m", "300m"),
				cpuHistory("sidecar", "200m", "100m"),
			},
			wantTarget: 120, wantBase: "600m", wantList: "app,sidecar", wantEvent: true,
			wantMessage: "off-pod sidecar is added at row.old 200m: 80 * 600/(300+100) = 120; the legacy rule gives 80 * 400/400 = 80 (no write)",
		},
		{
			name: "ZeroRequestContainerListedWithoutEvent", storedBase: "600m", list: "app,sidecar", hpaUtil: 68,
			containers: func(t *testing.T) []corev1.Container {
				return []corev1.Container{
					podContainer(t, "app", "550m", "1000m"),
					podContainer(t, "sidecar", "200m", "1000m"),
					podContainer(t, "debug", "", ""),
				}
			},
			history:    []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")},
			wantTarget: 64, wantBase: "600m", wantList: "app,debug,sidecar", wantEvent: false,
			wantMessage: "debug adds 0m: 80 * 600/750 = 64, listed without HPABaseRepaired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations(tt.storedBase, tt.list), cpuResourceMetric(tt.hpaUtil))
			pod := workloadPod("api-server", tt.containers(t)...)
			cl, notes := runHPARetuneWithEvents(t, hpa, pod, tt.history)

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, tt.wantTarget, metricUtil(t, updated, 0), tt.wantMessage)
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, tt.wantBase, updated.Annotations[annotationHPAOriginalCPURequest], tt.wantMessage)
			assert.Equal(t, tt.wantList, updated.Annotations[annotationHPAOriginalCPURequestContainers])
			assert.Equal(t, tt.wantEvent, hasBaseRepaired(notes), "HPABaseRepaired naming %s", annotationHPAOriginalCPURequest)
		})
	}
}

func TestRetuneHPAAfterResize_CPUListFullBaseNeverRepairs(t *testing.T) {
	t.Parallel()
	min80 := int32(80)
	tests := []struct {
		name        string
		hpaUtil     int32
		appFrom     string
		appTo       string
		bandMin     *int32
		wantTarget  int32
		wantMessage string
	}{
		{
			name: "Growth", hpaUtil: 68, appFrom: "500m", appTo: "550m",
			wantTarget: 64, wantMessage: "80 * 600/750 = 64",
		},
		{
			name: "CoincidentalEquality", hpaUtil: 60, appFrom: "600m", appTo: "650m",
			wantTarget: 56, wantMessage: "80 * 600/850 = 56, not 80 * 800/850 = 75",
		},
		{
			name: "FailedPriorUpdate", hpaUtil: 80, appFrom: "600m", appTo: "650m",
			wantTarget: 56, wantMessage: "stale 80 after a failed Update: 80 * 600/850 = 56; without a list 75/800m",
		},
		{
			name: "ClampedMatchWithBand", hpaUtil: 80, appFrom: "600m", appTo: "400m", bandMin: &min80,
			wantTarget: 80, wantMessage: "80 * 600/600 = 80 = current: skip, nothing written",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("600m", "app,sidecar"), cpuResourceMetric(tt.hpaUtil))
			pod := workloadPod("api-server",
				podContainer(t, "app", tt.appTo, "1000m"),
				podContainer(t, "sidecar", "200m", "1000m"),
			)
			var bounds *attunev1alpha1.HPATargetBounds
			if tt.bandMin != nil {
				bounds = &attunev1alpha1.HPATargetBounds{CPU: &attunev1alpha1.HPATargetBound{Min: tt.bandMin}}
			}
			cl, notes := runHPARetuneWithEvents(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", tt.appFrom, tt.appTo)}, bounds)

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, tt.wantTarget, metricUtil(t, updated, 0), tt.wantMessage)
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest], tt.wantMessage)
			assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
			assertNoBaseRepaired(t, notes)
		})
	}
}

func TestRetuneHPAAfterResize_CPUListRemovedContainerKeepsBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		storedBase  string
		list        string
		other       string
		wantBase    string
		wantList    string
		wantEvent   bool
		wantRemoved string
		wantMessage string
	}{
		{
			name: "RemovedContainer", storedBase: "800m", list: "app,mesh,sidecar", other: "sidecar",
			wantBase: "800m", wantList: "app,mesh,sidecar", wantRemoved: `"containers":["mesh"]`,
			wantMessage: "mesh is gone, base and name kept: 80 * 800/500 = 128",
		},
		{
			name: "RenamedContainer", storedBase: "600m", list: "app,envoy", other: "proxy",
			wantBase: "800m", wantList: "app,envoy,proxy", wantEvent: true, wantRemoved: `"containers":["envoy"]`,
			wantMessage: "proxy is added, envoy kept (double count): 80 * (600+200)/500 = 128; without a list 96/600m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations(tt.storedBase, tt.list), cpuResourceMetric(100))
			pod := workloadPod("api-server",
				podContainer(t, "app", "300m", "1000m"),
				podContainer(t, tt.other, "200m", "1000m"),
			)
			cl, notes, logged := runHPARetuneWithLogs(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")})

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(128), metricUtil(t, updated, 0), tt.wantMessage)
			assert.Equal(t, tt.wantBase, updated.Annotations[annotationHPAOriginalCPURequest], tt.wantMessage)
			assert.Equal(t, tt.wantList, updated.Annotations[annotationHPAOriginalCPURequestContainers])
			assert.Equal(t, tt.wantEvent, hasBaseRepaired(notes), "HPABaseRepaired")
			assertLogLine(t, logged, logRemovedCPUList, `"hpa":"api-server-hpa"`, tt.wantRemoved)
			assert.NotContains(t, logged, logMalformedCPUList)
		})
	}
}

// TestRetuneHPAAfterResize_CPUListRolloutAlternationDoesNotRatchet feeds
// pods of two revisions in turn, as a StatefulSet (partition, OnDelete) or
// DaemonSet rollout does; a RollingUpdate Deployment mid-rollout skips
// resizes. Names are append-only, so the base grows once.
// 192 in run 2 is the inflated target accepted for a missing listed
// container (FR-013).
func TestRetuneHPAAfterResize_CPUListRolloutAlternationDoesNotRatchet(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("400m", "app"), cpuResourceMetric(80))
	runs := []struct {
		from, to    string
		sidecar     bool
		wantTarget  int32
		wantEvent   bool
		wantRemoved bool
		wantMessage string
	}{
		{"400m", "300m", true, 96, true, false, "run 1: sidecar added: 80 * 600/500 = 96"},
		{"300m", "250m", false, 192, false, true, "run 2: old-revision pod without sidecar: 80 * 600/250 = 192 (cap 400)"},
		{"250m", "300m", true, 96, false, false, "run 3: sidecar back, already listed: 80 * 600/500 = 96"},
	}
	for i, run := range runs {
		containers := []corev1.Container{podContainer(t, "app", run.to, "1000m")}
		if run.sidecar {
			containers = append(containers, podContainer(t, "sidecar", "200m", "1000m"))
		}
		pod := workloadPod("api-server", containers...)
		cl, notes, logged := runHPARetuneWithLogs(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", run.from, run.to)})
		hpa = storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, run.wantTarget, metricUtil(t, hpa, 0), run.wantMessage)
		assert.Equal(t, "600m", hpa.Annotations[annotationHPAOriginalCPURequest], "run %d", i+1)
		assert.Equal(t, "app,sidecar", hpa.Annotations[annotationHPAOriginalCPURequestContainers], "run %d", i+1)
		assert.Equal(t, run.wantEvent, hasBaseRepaired(notes), "run %d HPABaseRepaired", i+1)
		if run.wantRemoved {
			assertLogLine(t, logged, logRemovedCPUList, `"hpa":"api-server-hpa"`, `"containers":["sidecar"]`)
		} else {
			assert.NotContains(t, logged, logRemovedCPUList, "run %d", i+1)
		}
	}
}

// TestRetuneHPAAfterResize_CPUListSkipsOffPodNativeSidecar covers a
// native sidecar seen only as a history row of another workload pod. It is
// an init container there, so a list repair does not add it; once the
// sampled pod has it as an init container the base still holds app and
// sidecar only.
func TestRetuneHPAAfterResize_CPUListSkipsOffPodNativeSidecar(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("600m", "app,sidecar"), cpuResourceMetric(80))
	runs := []struct {
		history     []attunev1alpha1.ResizeHistoryEntry
		pods        func(t *testing.T) []corev1.Pod
		wantTarget  int32
		wantMessage string
	}{
		{
			history: []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m"), cpuHistory("mesh", "100m", "80m")},
			pods: func(t *testing.T) []corev1.Pod {
				sampled := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"), podContainer(t, "sidecar", "200m", "1000m"))
				other := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"), podContainer(t, "sidecar", "200m", "1000m"))
				other.Name = "api-server-other-pod"
				other.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "80m")}
				return []corev1.Pod{sampled, other}
			},
			wantTarget:  82,
			wantMessage: "run 1: mesh is not added: 80 * 600/(300+200+80) = 82",
		},
		{
			history: []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "300m", "250m")},
			pods: func(t *testing.T) []corev1.Pod {
				sampled := workloadPod("api-server", podContainer(t, "app", "250m", "1000m"), podContainer(t, "sidecar", "200m", "1000m"))
				sampled.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "80m")}
				return []corev1.Pod{sampled}
			},
			wantTarget:  106,
			wantMessage: "run 2: mesh is a native sidecar on the sampled pod: 80 * 600/450 = 106",
		},
	}
	for i, run := range runs {
		cl, notes, logged := runHPARetuneWithPods(t, hpa, run.pods(t), run.history)
		hpa = storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, run.wantTarget, metricUtil(t, hpa, 0), run.wantMessage)
		assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU], "run %d", i+1)
		assert.Equal(t, "600m", hpa.Annotations[annotationHPAOriginalCPURequest], "run %d", i+1)
		assert.Equal(t, "app,sidecar", hpa.Annotations[annotationHPAOriginalCPURequestContainers], "run %d", i+1)
		assertNoBaseRepaired(t, notes)
		assert.NotContains(t, logged, logRemovedCPUList, "run %d", i+1)
	}
}

// TestRetuneHPAAfterResize_BaseWriteSkipsOffPodNativeSidecar covers the
// first write and the repair without a list when a history row belongs to
// a native sidecar of another workload pod. The target of this cycle uses
// the podCPUMillis sums, which count the row; the written base is the sum
// of the listed containers, which leave it out.
func TestRetuneHPAAfterResize_BaseWriteSkipsOffPodNativeSidecar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ann       map[string]string
		hpaUtil   int32
		wantEvent bool
	}{
		{name: "FirstWrite", hpaUtil: 80},
		{name: "RepairWithoutList", ann: cpuBaseAnnotations("500m"), hpaUtil: 70, wantEvent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", tt.ann, cpuResourceMetric(tt.hpaUtil))
			sampled := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"), podContainer(t, "sidecar", "200m", "1000m"))
			other := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"), podContainer(t, "sidecar", "200m", "1000m"))
			other.Name = "api-server-other-pod"
			other.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "80m")}
			cl, notes, _ := runHPARetuneWithPods(t, hpa, []corev1.Pod{sampled, other},
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m"), cpuHistory("mesh", "100m", "80m")})

			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(116), metricUtil(t, updated, 0), "80 * (400+200+100)/(200+200+80) = 116")
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest],
				"written base is the sum of the listed containers, without mesh")
			assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
			assert.Equal(t, tt.wantEvent, hasBaseRepaired(notes))
		})
	}
}

// TestRetuneHPAAfterResize_RepairWithoutListNeedsGrowthWithoutInitNames
// covers a base without a list that passes the equality rule only because
// the stored base holds an init container row of another pod. Leaving that
// row out adds nothing, so the base is kept and no event is sent.
func TestRetuneHPAAfterResize_RepairWithoutListNeedsGrowthWithoutInitNames(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("500m"), cpuResourceMetric(70))
	sampled := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"), podContainer(t, "sidecar", "100m", "1000m"))
	other := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"), podContainer(t, "sidecar", "100m", "1000m"))
	other.Name = "api-server-other-pod"
	other.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "80m")}
	cl, notes, _ := runHPARetuneWithPods(t, hpa, []corev1.Pod{sampled, other},
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m"), cpuHistory("mesh", "100m", "80m")})

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(105), metricUtil(t, updated, 0), "stored base kept: 80 * 500/(200+100+80) = 105")
	assert.Equal(t, "500m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.NotContains(t, updated.Annotations, annotationHPAOriginalCPURequestContainers)
	assertNoBaseRepaired(t, notes)
}

// assertLogLine asserts that one logged line holds msg and every want.
func assertLogLine(t *testing.T, logged, msg string, want ...string) {
	t.Helper()
	for line := range strings.SplitSeq(logged, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		for _, w := range want {
			assert.Contains(t, line, w)
		}
		return
	}
	assert.Failf(t, "log line missing", "no line with %q in:\n%s", msg, logged)
}

func assertMalformedCPUListLogged(t *testing.T, logged string) {
	t.Helper()
	assertLogLine(t, logged, logMalformedCPUList,
		`"hpa":"api-server-hpa"`, `"annotation":"`+annotationHPAOriginalCPURequestContainers+`"`)
	assert.NotContains(t, logged, logRemovedCPUList)
}

// TestRetuneHPAAfterResize_CPUListBandRemovedBetweenResizes reaches the
// target of 80 through a real band: run 1 clamps to Min 80 and writes
// nothing, then the band is removed. Run 2 keeps the listed full base,
// where the rule without a list would repair to 75/800m.
func TestRetuneHPAAfterResize_CPUListBandRemovedBetweenResizes(t *testing.T) {
	t.Parallel()
	min80 := int32(80)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("600m", "app,sidecar"), cpuResourceMetric(80))
	runs := []struct {
		from, to    string
		bandMin     *int32
		wantTarget  int32
		wantMessage string
	}{
		{"400m", "600m", &min80, 80, "run 1: 80 * 600/800 = 60, clamped to band Min 80 = current: skip"},
		{"600m", "650m", nil, 56, "run 2: band removed: 80 * 600/850 = 56; without a list 75/800m"},
	}
	for i, run := range runs {
		pod := workloadPod("api-server",
			podContainer(t, "app", run.to, "1000m"),
			podContainer(t, "sidecar", "200m", "1000m"),
		)
		var bounds *attunev1alpha1.HPATargetBounds
		if run.bandMin != nil {
			bounds = &attunev1alpha1.HPATargetBounds{CPU: &attunev1alpha1.HPATargetBound{Min: run.bandMin}}
		}
		cl, notes := runHPARetuneWithEvents(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", run.from, run.to)}, bounds)
		hpa = storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, run.wantTarget, metricUtil(t, hpa, 0), run.wantMessage)
		assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU], "run %d", i+1)
		assert.Equal(t, "600m", hpa.Annotations[annotationHPAOriginalCPURequest], "run %d", i+1)
		assert.Equal(t, "app,sidecar", hpa.Annotations[annotationHPAOriginalCPURequestContainers], "run %d", i+1)
		assertNoBaseRepaired(t, notes)
	}
}

func TestRetuneHPAAfterResize_MalformedCPUListIsLegacy(t *testing.T) {
	t.Parallel()
	for _, list := range []string{"", "app,,sidecar", "app, sidecar", "App_1", "-app"} {
		t.Run("Partial/"+list, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("400m", list), cpuResourceMetric(80))
			pod := workloadPod("api-server",
				podContainer(t, "app", "300m", "1000m"),
				podContainer(t, "sidecar", "200m", "1000m"),
			)
			cl, notes, logged := runHPARetuneWithLogs(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")})
			assertMalformedCPUListLogged(t, logged)
			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(96), metricUtil(t, updated, 0), "legacy repair: 80 * 600/500 = 96")
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
			assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers],
				"legacy repair rewrites the malformed list %q", list)
			assert.True(t, hasBaseRepaired(notes), "HPABaseRepaired")
		})
		t.Run("Growth/"+list, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("600m", list), cpuResourceMetric(68))
			pod := workloadPod("api-server",
				podContainer(t, "app", "550m", "1000m"),
				podContainer(t, "sidecar", "200m", "1000m"),
			)
			cl, notes, logged := runHPARetuneWithLogs(t, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")})
			assertMalformedCPUListLogged(t, logged)
			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(64), metricUtil(t, updated, 0), "legacy keep: 80 * 600/750 = 64")
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
			assert.Equal(t, list, updated.Annotations[annotationHPAOriginalCPURequestContainers],
				"no base write, so the malformed list %q stays", list)
			assertNoBaseRepaired(t, notes)
		})
	}
}

func TestRetuneHPAAfterResize_LegacyCPUBaseListWrites(t *testing.T) {
	t.Parallel()
	t.Run("RepairWritesList", func(t *testing.T) {
		t.Parallel()
		hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("400m"), cpuResourceMetric(80))
		pod := workloadPod("api-server",
			podContainer(t, "app", "300m", "1000m"),
			podContainer(t, "sidecar", "200m", "1000m"),
		)
		cl, notes := runHPARetuneWithEvents(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")})
		updated := storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, int32(96), metricUtil(t, updated, 0), "80 * 600/500 = 96")
		assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
		assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
		assert.True(t, hasBaseRepaired(notes), "HPABaseRepaired")
	})
	t.Run("NoRepairWritesNoList", func(t *testing.T) {
		t.Parallel()
		hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("600m"), cpuResourceMetric(68))
		pod := workloadPod("api-server",
			podContainer(t, "app", "550m", "1000m"),
			podContainer(t, "sidecar", "200m", "1000m"),
		)
		cl, notes := runHPARetuneWithEvents(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")})
		updated := storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, int32(64), metricUtil(t, updated, 0), "80 * 600/750 = 64")
		_, hasList := updated.Annotations[annotationHPAOriginalCPURequestContainers]
		assert.False(t, hasList, "a legacy base without a write gets no list (FR-009)")
		assertNoBaseRepaired(t, notes)
	})
}

// TestRetuneHPAAfterResize_CPUListRepairSkippedWhenTargetUnchanged covers
// the newTarget == currentTarget skip: nothing is written, and the next
// cycle repairs.
func TestRetuneHPAAfterResize_CPUListRepairSkippedWhenTargetUnchanged(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", cpuBaseAnnotations("400m", "app"), cpuResourceMetric(64))
	pod := workloadPod("api-server",
		podContainer(t, "app", "550m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, notes := runHPARetuneWithEvents(t, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "500m", "550m")})
	first := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(64), metricUtil(t, first, 0), "80 * 600/750 = 64 = current: skip; the legacy rule writes 42")
	assert.Equal(t, "400m", first.Annotations[annotationHPAOriginalCPURequest], "skip writes nothing")
	assert.Equal(t, "app", first.Annotations[annotationHPAOriginalCPURequestContainers], "skip writes nothing")
	assertNoBaseRepaired(t, notes)

	pod = workloadPod("api-server",
		podContainer(t, "app", "600m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, notes = runHPARetuneWithEvents(t, first, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "550m", "600m")})
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(60), metricUtil(t, second, 0), "next cycle repairs: 80 * 600/800 = 60")
	assert.Equal(t, "600m", second.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalCPURequestContainers])
	assert.True(t, hasBaseRepaired(notes), "HPABaseRepaired")
}

// TestRetuneHPAAfterResize_CPUListOnlyForPodCPUResource covers FR-016:
// ContainerResource CPU and pod-level memory bases get no list.
func TestRetuneHPAAfterResize_CPUListOnlyForPodCPUResource(t *testing.T) {
	t.Parallel()
	t.Run("ContainerResourceOnly", func(t *testing.T) {
		t.Parallel()
		hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuContainerMetric("app", 80))
		pod := workloadPod("api-server",
			podContainer(t, "app", "200m", "1000m"),
			podContainer(t, "sidecar", "200m", "1000m"),
		)
		cl, _ := runHPARetuneWithEvents(t, hpa, pod,
			[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")})
		updated := storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, int32(160), metricUtil(t, updated, 0), "80 * 400/200 = 160")
		assert.Equal(t, "400m", updated.Annotations[annotationHPACPUBasePrefix+"app"])
		_, hasList := updated.Annotations[annotationHPAOriginalCPURequestContainers]
		assert.False(t, hasList, "ContainerResource base gets no list")
	})
	t.Run("MemoryResourceWithContainerCPU", func(t *testing.T) {
		t.Parallel()
		hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
			cpuContainerMetric("app", 80),
			memoryResourceMetric(80),
		)
		pod := workloadPod("api-server",
			mixedContainer(t, "app", "200m", "1000m", "512Mi", "2Gi"),
			mixedContainer(t, "sidecar", "200m", "1000m", "1Gi", "2Gi"),
		)
		cl, _ := runHPARetuneWithEvents(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "200m"),
			memoryHistory("app", "1Gi", "512Mi"),
		})
		updated := storedHPA(t, cl, "api-server-hpa")
		assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest], "memory base was written")
		assert.Equal(t, "400m", updated.Annotations[annotationHPACPUBasePrefix+"app"], "container CPU base was written")
		_, hasList := updated.Annotations[annotationHPAOriginalCPURequestContainers]
		assert.False(t, hasList, "memory and ContainerResource bases get no list")
	})
}

func TestCopyHPATuneAnnotations_CPUBaseContainerListAsUnit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		src, dst map[string]string
		want     map[string]string
	}{
		{
			name: "StaleBaseWithoutListDropsLiveList",
			src:  map[string]string{annotationHPAOriginalCPU: "80", annotationHPAOriginalCPURequest: "400m"},
			dst:  map[string]string{annotationHPAOriginalCPU: "80", annotationHPAOriginalCPURequest: "600m", annotationHPAOriginalCPURequestContainers: "app,sidecar"},
			want: map[string]string{annotationHPAOriginalCPU: "80", annotationHPAOriginalCPURequest: "400m"},
		},
		{
			name: "BaseWithListCopiesBoth",
			src:  map[string]string{annotationHPAOriginalCPU: "80", annotationHPAOriginalCPURequest: "600m", annotationHPAOriginalCPURequestContainers: "app,sidecar"},
			dst:  map[string]string{"other": "x"},
			want: map[string]string{"other": "x", annotationHPAOriginalCPU: "80", annotationHPAOriginalCPURequest: "600m", annotationHPAOriginalCPURequestContainers: "app,sidecar"},
		},
		{
			name: "NoCPUBaseLeavesList",
			src:  map[string]string{annotationHPAAutoTune: "true"},
			dst:  map[string]string{annotationHPAOriginalCPURequest: "600m", annotationHPAOriginalCPURequestContainers: "app"},
			want: map[string]string{annotationHPAAutoTune: "true", annotationHPAOriginalCPURequest: "600m", annotationHPAOriginalCPURequestContainers: "app"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			copyHPATuneAnnotations(tt.dst, tt.src)
			assert.Equal(t, tt.want, tt.dst)
		})
	}
}

func TestParseCPUBaseContainers(t *testing.T) {
	t.Parallel()
	valid := map[string][]string{
		"app":               {"app"},
		"app,sidecar":       {"app", "sidecar"},
		"sidecar,app":       {"app", "sidecar"},
		"app,app,sidecar":   {"app", "sidecar"},
		"a-1,istio-proxy,z": {"a-1", "istio-proxy", "z"},
	}
	for in, want := range valid {
		set, ok := parseCPUBaseContainers(in)
		require.True(t, ok, "%q is a valid list", in)
		got := make([]string, 0, len(set))
		for name := range set {
			got = append(got, name)
		}
		assert.ElementsMatch(t, want, got, "%q", in)
	}
	for _, in := range []string{"", ",", "app,", "app,,sidecar", "app, sidecar", " app", "App_1", "-app", "app/x"} {
		set, ok := parseCPUBaseContainers(in)
		assert.False(t, ok, "%q is malformed", in)
		assert.Nil(t, set, "%q yields no partial set", in)
	}
}

func TestFormatCPUBaseContainers(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "app,debug,sidecar", formatCPUBaseContainers(map[string]struct{}{"sidecar": {}, "app": {}, "debug": {}}))
	assert.Equal(t, "app", formatCPUBaseContainers(map[string]struct{}{"app": {}}))
}

func TestCPUBaseContainerMillisMatchesPodCPUMillis(t *testing.T) {
	t.Parallel()
	initPod := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	initPod.Spec.InitContainers = []corev1.Container{podContainer(t, "migrate", "50m", "1000m")}
	tests := []struct {
		name string
		pod  corev1.Pod
		rows map[string]hpaCPURow
		want map[string]int64
	}{
		{
			name: "ResourceUsesUnchangedLiveContainer",
			pod: workloadPod("api-server",
				podContainer(t, "app", "200m", "1000m"),
				podContainer(t, "sidecar", "200m", "1000m"),
			),
			rows: map[string]hpaCPURow{"app": {old: 400, neu: 200, ok: true}},
			want: map[string]int64{"app": 400, "sidecar": 200},
		},
		{
			name: "InitContainerStaysOutOfPodTotal",
			pod:  initPod,
			rows: map[string]hpaCPURow{
				"app":     {old: 400, neu: 200, ok: true},
				"migrate": {old: 100, neu: 50, ok: true},
			},
			want: map[string]int64{"app": 400, "sidecar": 200},
		},
		{
			name: "OffPodRowIncluded",
			pod: workloadPod("api-server",
				podContainer(t, "app", "300m", "1000m"),
				podContainer(t, "debug", "", ""),
			),
			rows: map[string]hpaCPURow{
				"app":  {old: 400, neu: 300, ok: true},
				"gone": {old: 200, neu: 100, ok: true},
			},
			want: map[string]int64{"app": 400, "debug": 0, "gone": 200},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cpuBaseContainerMillis(&tt.pod, tt.rows, nil)
			assert.Equal(t, tt.want, got)
			oldMilli, _, ok := podCPUMillis(&tt.pod, tt.rows)
			require.True(t, ok)
			var sum int64
			for _, v := range got {
				sum += v
			}
			assert.Equal(t, oldMilli, sum, "per-container values sum to the podCPUMillis old sum")
		})
	}
}

// TestCPUBaseContainerMillisSkipsWorkloadInitNames covers the two places
// where the per-container set leaves out a name podCPUMillis counts: an
// init container of another workload pod seen as a history row, or as a
// regular container of the sampled pod.
func TestInitContainerNamesSkipsFinishedPods(t *testing.T) {
	t.Parallel()
	running := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"))
	running.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "proxy", "50m")}
	evicted := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"))
	evicted.Name = "api-server-evicted"
	evicted.Status.Phase = corev1.PodFailed
	evicted.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "100m")}
	done := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"))
	done.Name = "api-server-done"
	done.Status.Phase = corev1.PodSucceeded
	done.Spec.InitContainers = []corev1.Container{nativeSidecar(t, "mesh", "100m")}

	assert.Equal(t, map[string]struct{}{"proxy": {}},
		initContainerNames([]corev1.Pod{running, evicted, done}),
		"a mesh native sidecar only on finished pods does not keep mesh out of the base")
}

func TestCPUBaseContainerMillisSkipsWorkloadInitNames(t *testing.T) {
	t.Parallel()
	initNames := map[string]struct{}{"mesh": {}}
	tests := []struct {
		name    string
		pod     corev1.Pod
		rows    map[string]hpaCPURow
		want    map[string]int64
		wantOld int64
	}{
		{
			name: "OffPodRow",
			pod:  workloadPod("api-server", podContainer(t, "app", "300m", "1000m")),
			rows: map[string]hpaCPURow{
				"app":  {old: 400, neu: 300, ok: true},
				"mesh": {old: 100, neu: 80, ok: true},
			},
			want:    map[string]int64{"app": 400},
			wantOld: 500,
		},
		{
			name: "RegularOnSampledPod",
			pod: workloadPod("api-server",
				podContainer(t, "app", "300m", "1000m"),
				podContainer(t, "mesh", "100m", "1000m"),
			),
			rows:    map[string]hpaCPURow{"app": {old: 400, neu: 300, ok: true}},
			want:    map[string]int64{"app": 400},
			wantOld: 500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, cpuBaseContainerMillis(&tt.pod, tt.rows, initNames))
			oldMilli, _, ok := podCPUMillis(&tt.pod, tt.rows)
			require.True(t, ok)
			assert.Equal(t, tt.wantOld, oldMilli, "podCPUMillis still counts mesh")
		})
	}
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
