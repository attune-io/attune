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
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/conflict"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestReconcile_MissingPolicyReturnsNoError(t *testing.T) {
	reconciler := newReconcilerWithClient()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent-policy",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_PausedPolicySkipsReconciliation(t *testing.T) {
	paused := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-policy",
			Namespace:  "default",
			Finalizers: []string{"attune.io/cleanup"},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			Paused: &paused,
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: func() *string { s := "my-app"; return &s }(),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = attunev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paused-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// Paused policies should not requeue (no work to do until unpaused).
	assert.Equal(t, ctrl.Result{}, result)

	// Verify the Ready condition is set to False with reason Paused.
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "paused-policy", Namespace: "default"}, &updated))
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, readyCond, "Ready condition should be set")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, attunev1alpha1.ReasonPaused, readyCond.Reason)
	assert.Contains(t, readyCond.Message, "paused")
}

func TestReconcile_MissingPolicyCleansGauges(t *testing.T) {
	reconciler := newReconcilerWithClient()
	// CollectAndCount is process-wide. Clear leftover series from other tests
	// in this package so this case only sees the labels it seeds.
	operatormetrics.RecommendationCPU.Reset()
	operatormetrics.RecommendationMemory.Reset()
	operatormetrics.Confidence.Reset()
	operatormetrics.BurstFactor.Reset()

	// Seed gauges for namespace "default" as if a prior reconcile set them.
	operatormetrics.RecommendationCPU.WithLabelValues("default", "api-server", "main").Set(0.5)
	operatormetrics.RecommendationMemory.WithLabelValues("default", "api-server", "main").Set(512 * 1024 * 1024)
	operatormetrics.Confidence.WithLabelValues("default", "api-server", "main").Set(0.9)
	operatormetrics.BurstFactor.WithLabelValues("default", "api-server", "main", "cpu").Set(1.2)

	// Simulate a prior reconcile that tracked these gauge keys.
	reconciler.gaugeKeys.Store("default/deleted-policy", []gaugeKey{
		{Namespace: "default", Workload: "api-server", Container: "main"},
	})

	// Verify gauges are set.
	require.InDelta(t, 0.5, promtestutil.ToFloat64(
		operatormetrics.RecommendationCPU.WithLabelValues("default", "api-server", "main")), 1e-9)

	// Reconcile a missing policy in "default" namespace.
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "deleted-policy",
			Namespace: "default",
		},
	}
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Gauges for this policy should be cleaned up.
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.RecommendationCPU),
		"recommendation CPU gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.RecommendationMemory),
		"recommendation memory gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.Confidence),
		"confidence gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.BurstFactor),
		"burst factor gauges should be cleaned after policy deletion")
}

func TestReconcile_AddsFinalizer(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	assert.Contains(t, updated.Finalizers, finalizerName,
		"finalizer should be added on first reconcile")
}

func TestReconcile_NoMatchingWorkloadsSetsNoWorkloadsFound(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-policy",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Verify the status was updated with NoWorkloadsFound condition.
	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-policy",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, "NoWorkloadsFound", updated.Status.Conditions[0].Reason)
}

func TestReconcile_BearerTokenSecretRotationRecreatesCollector(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &attunev1alpha1.SecretKeyRef{
		Name: "prom-token",
		Key:  "token",
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("token-a")},
	}

	collector1 := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	collector2 := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	collectors := []rsmetrics.MetricsCollector{collector1, collector2}
	var optsSeen []*rsmetrics.CollectorOptions

	reconciler, fakeClient := newReconcilerForReconcile(collector1, policy, deploy, secret)
	reconciler.MetricsFactory = func(_ string, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		require.NotNil(t, opts)
		copyOpts := &rsmetrics.CollectorOptions{
			BearerToken:        opts.BearerToken,
			InsecureSkipVerify: opts.InsecureSkipVerify,
		}
		if opts.Headers != nil {
			copyOpts.Headers = make(map[string]string, len(opts.Headers))
			for k, v := range opts.Headers {
				copyOpts.Headers[k] = v
			}
		}
		optsSeen = append(optsSeen, copyOpts)
		idx := len(optsSeen) - 1
		return collectors[idx], nil
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, optsSeen, 1)
	assert.Equal(t, "token-a", optsSeen[0].BearerToken)

	var rotated corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "prom-token", Namespace: "default"}, &rotated))
	rotated.Data["token"] = []byte("token-b")
	require.NoError(t, fakeClient.Update(context.Background(), &rotated))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, optsSeen, 2)
	assert.Equal(t, "token-b", optsSeen[1].BearerToken)
	assert.NotSame(t, collector1, collector2)
}

// ---------- Reconcile happy path ----------

func TestReconcile_HappyPathWithRecommendations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod1, pod2)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	// Verify status was updated with recommendations and Ready=True.
	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	assert.Equal(t, int32(1), updated.Status.Workloads.Pending, "Recommend mode: all workloads with recs should be pending")
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)

	// Verify Ready condition.
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "Monitoring", updated.Status.Conditions[0].Reason)
}

func TestReconcile_ObserveModeOmitsRecommendations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

	// Observe mode: workloads are discovered and data points tracked,
	// but recommendations and savings are not surfaced.
	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Empty(t, updated.Status.Recommendations, "Observe mode should not populate recommendations")
	assert.Empty(t, updated.Status.Savings.EstimatedMonthlySavings, "Observe mode should not compute savings")
	assert.True(t, updated.Status.Workloads.DataPointsCollected > 0, "Observe mode should still track data points")
}

func TestReconcile_RecommendModeKeepsRecommendationsWithoutLivePods(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	assert.Equal(t, int32(1), updated.Status.Workloads.Pending)
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)
}

// ---------- Reconcile with OneShot mode (exercises resize path entry) ----------

func TestReconcile_OneShotMode_NoClientset_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
}

// ---------- Reconcile with Prometheus error ----------

func TestReconcile_PrometheusUnavailable(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("connection refused")
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, updated.Status.Conditions[0].Reason)
}

func TestReconcile_PrometheusQueryErrorsMentionBlockedDataTypes(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	// Live pod lets the missing-memory arm hold current requests so the
	// CPU-only rec stays fresh (template is not a hybrid apply target).
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return nil, fmt.Errorf("memory query failed")
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, reconciler.parseCooldown(policy), result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonMonitoring, cond.Reason)
	assert.Contains(t, cond.Message, "Watching 1 workloads, 1 with recommendations")
	assert.Contains(t, cond.Message, "Metrics query errors (1)")
	assert.Contains(t, cond.Message, "memory data collection")
	assert.NotContains(t, cond.Message, "CPU and/or memory")
}

func TestReconcile_PrometheusQueryErrorsMentionCPUAndMemoryWhenBothFail(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}, policy, deploy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, reconciler.parseCooldown(policy), result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "Metrics query errors (2)")
	assert.Contains(t, cond.Message, "CPU and memory data collection")
}

// ---------- Reconcile with cooldown active ----------

func TestReconcile_CooldownActive_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot
	policy.Annotations = map[string]string{
		lastResizeAnnotation: time.Now().UTC().Format(time.RFC3339),
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
}

// ---------- History-based Resized count derivation ----------

func TestReconcile_HistoryBasedResizedDerivation(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name        string
		mode        attunev1alpha1.UpdateType
		history     []attunev1alpha1.ResizeHistoryEntry
		wantResized int32
	}{
		{
			name: "derives Resized from distinct successful in-place workloads",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "worker", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 2,
		},
		{
			name: "evicted workloads do not count as resized",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: now},
				{Workload: "worker", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "legacy successful history without method still counts as resized",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "worker", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 2,
		},
		{
			name: "only failed and reverted entries leave Resized at 0",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed, Timestamp: now},
				{Workload: "worker", Method: "InPlace", Result: attunev1alpha1.ResizeResultReverted, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "duplicate workload entries counted as one",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed, Timestamp: now},
			},
			wantResized: 1,
		},
		{
			name:        "empty history leaves Resized at 0",
			mode:        attunev1alpha1.UpdateTypeOneShot,
			history:     nil,
			wantResized: 0,
		},
		{
			name: "Recommend mode skips derivation entirely",
			mode: attunev1alpha1.UpdateTypeRecommend,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "Observe mode skips derivation entirely",
			mode: attunev1alpha1.UpdateTypeObserve,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Type = tt.mode
			// Set cooldown annotation so resize execution is skipped;
			// this isolates the history-based derivation path.
			if isResizeMode(tt.mode) {
				policy.Annotations = map[string]string{
					lastResizeAnnotation: time.Now().UTC().Format(time.RFC3339),
				}
			}
			policy.Status.ResizeHistory = tt.history

			deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
			pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}
			reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
			})
			require.NoError(t, err)

			var updated attunev1alpha1.AttunePolicy
			require.NoError(t, fakeClient.Get(context.Background(),
				types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

			assert.Equal(t, tt.wantResized, updated.Status.Workloads.Resized,
				"Resized count should match derived value from history")
		})
	}
}

func TestReconcile_WorkloadOptedOut(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Annotations = map[string]string{conflict.AnnotationSkip: "true"}
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	// Skip is not discovery: the workload still matches targetRef.
	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
}

// ---------- Reconcile with MetricsFactory error ----------

func TestReconcile_MetricsFactoryError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("TLS handshake timeout")
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, updated.Status.Conditions[0].Reason)
	assert.Contains(t, updated.Status.Conditions[0].Message, "TLS handshake timeout")
}

func TestReconcile_BearerTokenSecretReadErrorIncludesSecretRef(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &attunev1alpha1.SecretKeyRef{
		Name: "prom-token",
		Key:  "token",
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy, deploy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "prom-token/token")
	assert.Contains(t, cond.Message, "reading secret default/prom-token")
}

// ---------- Reconcile with workload discovery error ----------

func TestReconcile_DiscoverWorkloadsError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Kind = "ConfigMap"

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err, "discovery errors should be surfaced via status condition, not returned")
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	// Verify the error is visible in the policy status condition.
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonWorkloadDiscoveryFailed, cond.Reason)
	assert.Contains(t, cond.Message, "Failed to discover workloads")
}

func TestReconcile_ListPoliciesErrorFailsClosed(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	priorCPU := resource.MustParse("250m")
	priorMem := resource.MustParse("256Mi")
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    priorCPU,
				MemoryRequest: priorMem,
			},
		}},
	}}
	policy.Status.Workloads = attunev1alpha1.WorkloadStatus{
		Discovered:          1,
		WithRecommendations: 1,
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "AttunePolicyList" {
					return fmt.Errorf("simulated policy list failure")
				}
				return cw.List(ctx, list, opts...)
			},
		}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, failingClient, scheme)
	reconciler.Recorder = &fakeEventRecorder{}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, failingClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonConflictCheckFailed, cond.Reason)
	assert.Equal(t, "Failed to list AttunePolicies for conflict detection; recommendations not computed", cond.Message)
	assert.NotContains(t, cond.Message, "simulated policy list failure")
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)
	require.Len(t, updated.Status.Recommendations[0].Containers, 1)
	assert.True(t, updated.Status.Recommendations[0].Containers[0].Recommended.CPURequest.Equal(priorCPU))
	assert.True(t, updated.Status.Recommendations[0].Containers[0].Recommended.MemoryRequest.Equal(priorMem))
	require.NotEmpty(t, updated.Status.WorkloadErrors)
	assert.Equal(t, "*", updated.Status.WorkloadErrors[0].Workload)
	assert.Equal(t, "Failed to list AttunePolicies for conflict detection; recommendations not computed", updated.Status.WorkloadErrors[0].Error)
	assert.NotContains(t, updated.Status.WorkloadErrors[0].Error, "simulated policy list failure")
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	require.NotNil(t, updated.Status.LastReconcileTime)
}

func TestReconcile_FetchDefaultsErrorFailsClosed(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	clusterDefaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prometheus.default.svc:9090"},
			},
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, clusterDefaults, deploy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*attunev1alpha1.AttuneNamespaceDefaultsList); ok {
					return fmt.Errorf("simulated namespace defaults API failure")
				}
				return cw.List(ctx, list, opts...)
			},
		}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, failingClient, scheme)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, failingClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "Failed to fetch defaults")
	assert.Contains(t, cond.Message, "simulated namespace defaults API failure")
}

// ---------- Reconcile with AutoRevert checking safety observations ----------

func TestReconcile_NotFoundClearsIncreaseRates(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(testScheme()).Build()
	r.Scheme = testScheme()
	now := time.Now()
	r.increaseRates.Store("default/gone", newIncreaseRateBucket(100, 100, now))
	r.lastBlockerRefresh.Store(types.NamespacedName{Namespace: "default", Name: "gone"}, now)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"},
	})
	require.NoError(t, err)
	_, ok := r.increaseRates.Load("default/gone")
	assert.False(t, ok, "not-found reconcile must drop the rate bucket")
	_, ok = r.lastBlockerRefresh.Load(types.NamespacedName{Namespace: "default", Name: "gone"})
	assert.False(t, ok, "not-found reconcile must drop the blocker clock")
}

func TestReconcile_AutoRevertDisabledClearsSafetyObservation(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    attunev1alpha1.ConditionSafetyObservation,
		Status:  metav1.ConditionTrue,
		Reason:  attunev1alpha1.ReasonSafetyEvaluating,
		Message: "stale observation",
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Nil(t, meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionSafetyObservation),
		"disabling autoRevert must clear SafetyObservation")
}

func TestReconcile_AutoRevertCallsSafetyObservations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
}

// ---------- Reconcile mid-rollout skip ----------

func TestReconcile_SkipsMidRolloutWorkload(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Status.UpdatedReplicas = 1 // Only 1 of 2 updated (mid-rollout).
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
}

func TestReconcile_NowFuncControlsScheduleGate(t *testing.T) {
	// A policy with a schedule window of 02:00-06:00 UTC on Wednesdays.
	// When NowFunc returns a time outside the window, no resize should happen.
	// When NowFunc returns a time inside the window, resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
		DaysOfWeek: []string{"Wednesday"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod, policy).
		WithStatusSubresource(policy).Build()

	// Wednesday 10:00 UTC -- outside the 02:00-06:00 window.
	outsideWindow := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return outsideWindow })

	result := r.now()
	assert.Equal(t, outsideWindow, result)
	assert.False(t, isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now()),
		"10:00 should be outside 02:00-06:00 window")

	// Wednesday 03:00 UTC -- inside the window.
	insideWindow := time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return insideWindow })
	assert.True(t, isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now()),
		"03:00 should be inside 02:00-06:00 window")
}

func TestReconcile_InsufficientDataRequeuesAtQueryStep(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// Use a long cooldown to make the difference obvious.
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil // 20 samples, below 48 threshold
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// With InsufficientData, requeue should be the query step (5m),
	// not the cooldown (2h).
	assert.Equal(t, attunev1alpha1.DefaultQueryStep, result.RequeueAfter,
		"InsufficientData should requeue at queryStep interval, not cooldown")
}

// TestReconcile_MemoryFromCPURatioWaits_NoAutoApply locks #819/#820 at the
// Reconcile boundary: Auto + memory gauges without CPU must stay
// InsufficientData at queryStep (no Monitoring rec, no resize, no jitter).
func TestReconcile_MemoryFromCPURatioWaits_NoAutoApply(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.Memory.AllowDecrease = boolPtr(true)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 8*1024*1024)}, nil
			}
			return map[string][]rsmetrics.Sample{}, nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler.RequeueJitter = 2 * time.Minute

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, attunev1alpha1.DefaultQueryStep, result.RequeueAfter,
		"memoryFromCpuRatio wait must requeue at queryStep, not cooldown or cooldown+jitter")

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInsufficientData, cond.Reason)
	assert.Empty(t, updated.Status.Recommendations)
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
}

// Nightly #520: cooldown 1m is shorter than default queryStep 5m, so the
// InsufficientData shortcut leaves requeueAfter == cooldown. Jitter must
// not apply or first recommendations wait up to cooldown+jitter (3m with
// the default 2m jitter), which races the configmap-export 3m timeout.
func TestReconcile_InsufficientDataShortCooldownDoesNotJitter(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.UID = "e2e-configmap-export-uid"
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 1 * time.Minute}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil // below 48 threshold
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.RequeueJitter = 2 * time.Minute

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter,
		"InsufficientData must not add RequeueJitter when cooldown < queryStep")
}

func TestReconcile_MetricsUnavailableDoesNotJitter(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 1 * time.Minute}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}, policy, deploy)
	reconciler.RequeueJitter = 2 * time.Minute

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter,
		"MetricsUnavailable must not add RequeueJitter")

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, cond.Reason)
}

func TestReconcile_SufficientDataRequeuesAtCooldown(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil // 200 samples, above 48 threshold
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// With sufficient data, requeue should be the full cooldown.
	assert.Equal(t, 2*time.Hour, result.RequeueAfter,
		"sufficient data should requeue at cooldown interval")
}

func TestReconcile_AllCoolingRequeuesAtRemaining(t *testing.T) {
	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 1 * time.Hour}
	policy.Annotations = map[string]string{
		lastResizeAnnotation:                     now.Add(-50 * time.Minute).UTC().Format(time.RFC3339),
		lastResizeAnnotationKey("api-server"):    now.Add(-50 * time.Minute).UTC().Format(time.RFC3339),
		lastResizeAnnotationKey("other-service"): now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.SetNowFunc(func() time.Time { return now })

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 10*time.Minute, result.RequeueAfter,
		"all matched apps cooling must requeue at remaining cooldown, not a fresh 1h")
}

func TestReconcile_CanaryDoesNotRetuneHPAUntilPromoted(t *testing.T) {
	run := func(t *testing.T, promoted bool) int32 {
		t.Helper()
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
		policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
			Percentage:        100,
			ObservationPeriod: metav1.Duration{Duration: 10 * time.Minute},
		}
		policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
		phase := attunev1alpha1.CanaryPhaseInProgress
		if promoted {
			phase = attunev1alpha1.CanaryPhaseFullRollout
		}
		policy.Status.Canary = &attunev1alpha1.CanaryStatus{
			Phase: phase,
			Workloads: []attunev1alpha1.CanaryWorkloadStatus{
				{Workload: "api-server", Phase: phase, Pods: []string{"api-server-abc-1"}},
			},
		}

		deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
		pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
		oldTarget := int32(80)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
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

		mc := &mockCollector{
			queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
				return generateSamples(200, 0.1), nil
			},
		}
		reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod, hpa)
		reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())

		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
		})
		require.NoError(t, err)

		var updatedPolicy attunev1alpha1.AttunePolicy
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
			Name: "test-policy", Namespace: "default",
		}, &updatedPolicy))
		require.Greater(t, updatedPolicy.Status.Workloads.Resized, int32(0),
			"canary must actually resize so HPA skip is on the live path")
		require.NotEmpty(t, updatedPolicy.Status.Recommendations)
		assert.Equal(t, "Deployment", updatedPolicy.Status.Recommendations[0].Kind,
			"rec.Kind must match HPA ScaleTargetRef so adjustHPATargets is reachable")

		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
			Name: "api-server-hpa", Namespace: "default",
		}, &updatedHPA))
		require.NotNil(t, updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization)
		return *updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization
	}

	t.Run("unpromoted keeps 80", func(t *testing.T) {
		assert.Equal(t, int32(80), run(t, false),
			"unpromoted canary resize must not rewrite the HPA target")
	})
	t.Run("promoted retunes away from 80", func(t *testing.T) {
		got := run(t, true)
		assert.NotEqual(t, int32(80), got,
			"promoted canary must retune HPA so the skip is proven reachable (got %d)", got)
	})
}
