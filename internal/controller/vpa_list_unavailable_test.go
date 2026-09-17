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
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func vpaListFailInterceptor(fail *atomic.Bool) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok &&
				u.GroupVersionKind().Kind == "VerticalPodAutoscalerList" {
				if fail == nil || fail.Load() {
					return fmt.Errorf("simulated VPA list failure")
				}
			}
			return c.List(ctx, list, opts...)
		},
	}
}

func TestReconcile_VPAListError_SetsResizeBlocked(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	ns := newTestNamespace("default", nil)

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(vpaListFailInterceptor(nil)).
		Build()
	reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler.Clientset = clientset

	var eventReason, eventNote string
	reconciler.Recorder = &hpaListEventRecorder{reason: &eventReason, note: &eventNote}

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("list_vpas"))

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Greater(t, updated.Status.Workloads.WithRecommendations, int32(0))
	require.NotEmpty(t, updated.Status.Recommendations)
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)

	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	require.NotNil(t, cond, "VPA list error must set ResizeBlocked so an applying VPA is not treated as absent")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonVPAListUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "verticalpodautoscalers")

	assert.Equal(t, attunev1alpha1.ReasonVPAListUnavailable, eventReason)

	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("list_vpas"))
	assert.Equal(t, before+1, after)

	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "resize", a.GetSubresource(), "VPA list error must not resize leftover pods")
	}
}

func TestReconcile_VPAListError_ClearsWhenListSucceeds(t *testing.T) {
	// Shares ReconcileErrorsTotal{list_vpas} with TestReconcile_VPAListError_SetsResizeBlocked.
	// Recommend + blocker hold so setResizeBlockedCondition does not wipe
	// ResizeBlocked on the second pass; only clearVPAListUnavailable can.
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	ns := newTestNamespace("default", nil)

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	var failList atomic.Bool
	failList.Store(true)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(vpaListFailInterceptor(&failList)).
		Build()
	reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler.BlockerRefreshInterval = 5 * time.Minute
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	reconciler.SetNowFunc(func() time.Time { return now })

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var blocked attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &blocked))
	cond := meta.FindStatusCondition(blocked.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonVPAListUnavailable, cond.Reason)

	failList.Store(false)
	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var cleared attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &cleared))
	cond = meta.FindStatusCondition(cleared.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	if cond != nil {
		assert.NotEqual(t, attunev1alpha1.ReasonVPAListUnavailable, cond.Reason)
	}
}
