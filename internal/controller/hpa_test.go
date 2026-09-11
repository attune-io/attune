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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
