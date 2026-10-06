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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

const otherControllerAnnotation = "example.com/other-controller"

func runScriptedHPARetune(
	t *testing.T,
	policy *attunev1alpha1.AttunePolicy,
	hpa autoscalingv2.HorizontalPodAutoscaler,
	pod corev1.Pod,
	history []attunev1alpha1.ResizeHistoryEntry,
	onUpdate func(ctx context.Context, raw client.WithWatch, obj client.Object, opts []client.UpdateOption, attempt int32) error,
) (client.Client, int32, []string) {
	t.Helper()
	scheme := testScheme()
	var updates atomic.Int32
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, raw client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			n := updates.Add(1)
			if onUpdate != nil {
				return onUpdate(ctx, raw, obj, opts, n)
			}
			return raw.Update(ctx, obj, opts...)
		},
	}).Build()
	recorder := events.NewFakeRecorder(8)
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = cl
	reconciler.Scheme = scheme
	reconciler.Recorder = recorder
	if policy == nil {
		policy = newTestPolicy("p", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	}
	reconciler.retuneHPAAfterResize(context.Background(), policy, attunev1alpha1.UpdateTypeAuto,
		history,
		[]attunev1alpha1.WorkloadRecommendation{{Workload: "api-server", Kind: "Deployment"}},
		[]autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {pod}},
	)
	return cl, updates.Load(), recorderNotes(recorder)
}

func conflictThenForeignAnnotation(t *testing.T) func(context.Context, client.WithWatch, client.Object, []client.UpdateOption, int32) error {
	t.Helper()
	hpaGR := schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}
	return func(ctx context.Context, raw client.WithWatch, obj client.Object, opts []client.UpdateOption, attempt int32) error {
		if attempt == 1 {
			var current autoscalingv2.HorizontalPodAutoscaler
			if err := raw.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
				return err
			}
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[otherControllerAnnotation] = "keep"
			if err := raw.Update(ctx, &current); err != nil {
				return err
			}
			return apierrors.NewConflict(hpaGR, current.Name, fmt.Errorf("status write"))
		}
		return raw.Update(ctx, obj, opts...)
	}
}

func TestRetuneHPAAfterResize_ConflictThenSuccessWritesTarget(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "100m", "1000m"))
	cl, updates, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "100m", "200m")},
		conflictThenForeignAnnotation(t))

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(2), updates)
	assert.Equal(t, int32(40), metricUtil(t, updated, 0), "80 * 100/200 = 40")
	assert.Equal(t, "100m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "keep", updated.Annotations[otherControllerAnnotation])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
		assert.NotContains(t, note, "HPATargetClamped")
	}
}

func TestRetuneHPAAfterResize_ConflictExhaustedSkipsEvents(t *testing.T) {
	t.Parallel()
	max50 := int32(50)
	policy := policyWithBounds(&attunev1alpha1.HPATargetBounds{
		CPU: &attunev1alpha1.HPATargetBound{Max: &max50},
	})
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "400m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	hpaGR := schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}
	cl, updates, notes := runScriptedHPARetune(t, policy, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")},
		func(context.Context, client.WithWatch, client.Object, []client.UpdateOption, int32) error {
			return apierrors.NewConflict(hpaGR, "api-server-hpa", fmt.Errorf("status write"))
		})

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(retry.DefaultRetry.Steps), updates)
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Equal(t, "400m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Empty(t, notes)
}

func TestRetuneHPAAfterResize_NonConflictUpdateTriesOnce(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "400m",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, updates, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")},
		func(context.Context, client.WithWatch, client.Object, []client.UpdateOption, int32) error {
			return fmt.Errorf("apiserver unavailable")
		})

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(1), updates)
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Equal(t, "400m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Empty(t, notes)
}

func TestRetuneHPAAfterResize_MemoryConflictThenSuccess(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, updates, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
		conflictThenForeignAnnotation(t))

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(2), updates)
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "80 * 1Gi/512Mi = 160")
	assert.Equal(t, "1Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, "keep", updated.Annotations[otherControllerAnnotation])
	assert.Empty(t, notes)
}
