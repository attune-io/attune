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
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
)

// ---------- computeRecommendations ----------

func TestComputeRecommendations_HappyPath(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu_usage_seconds_total") {
				return generateSamples(200, 0.1), nil // ~100m CPU
			}
			return generateSamples(200, 128*1024*1024), nil // ~128Mi memory
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
	assert.Greater(t, rec.Containers[0].DataPoints, int32(0))
	assert.Greater(t, rec.Containers[0].Confidence, 0.0)
}

func TestComputeRecommendations_InsufficientDataPoints(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil // Only 20 samples, below 48 threshold
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec) // No recommendation because data points are insufficient
}

// TestComputeRecommendations_EmptyQueryReusesPriorAsStale is the live-red
// contract: an empty Prometheus query must reuse the prior rec and set
// Stale. Removing the production Stale assignment fails this test.
func TestComputeRecommendations_EmptyQueryReusesPriorAsStale(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Stale:        false,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    cpuRec,
				MemoryRequest: memRec,
			},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec, "empty query must reuse the prior recommendation, not drop it")
	assert.True(t, rec.Stale, "reused rec after empty Prometheus query must be marked stale")
	require.NotNil(t, rec.LastDataTime, "LastDataTime must stay the last non-empty sample")
	assert.True(t, rec.LastDataTime.Equal(&priorData), "LastDataTime must not be overwritten with reconcile now")
	require.Len(t, rec.Containers, 1)
	assert.True(t, rec.Containers[0].Recommended.CPURequest.Equal(cpuRec), "reused CPU rec")
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Equal(memRec), "reused memory rec")
	assert.False(t, policy.Status.Recommendations[0].Stale, "reuse must DeepCopy, not mutate status in place")
}

func TestComputeRecommendations_QueryGapsReusePriorAsStale(t *testing.T) {
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)

	nanSamples := make([]rsmetrics.Sample, 50)
	nanNow := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: nanNow.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	tests := []struct {
		name            string
		queryRangeFunc  func(context.Context, string, time.Time, time.Time, time.Duration) ([]rsmetrics.Sample, error)
		wantQueryErrors int
		wantFailedTypes []string
	}{
		{
			name: "both queries error",
			queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
				return nil, fmt.Errorf("connection refused")
			},
			wantQueryErrors: 2,
			wantFailedTypes: []string{"CPU", "memory"},
		},
		{
			name: "insufficient data points",
			queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
				return generateSamples(20, 0.1), nil
			},
		},
		{
			name: "all NaN samples",
			queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
				return nanSamples, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			deploy := newTestDeployment("api-server", "default", nil)
			reconciler := newReconcilerWithClient()
			reconciler.SetNowFunc(func() time.Time { return now })
			policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
				Workload:     "api-server",
				Kind:         "Deployment",
				LastDataTime: &priorData,
				Stale:        false,
				Containers: []attunev1alpha1.ContainerRecommendation{{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    cpuRec,
						MemoryRequest: memRec,
					},
				}},
			}}

			mc := &mockCollector{queryRangeFunc: tt.queryRangeFunc}
			rec, qErrors, failedMetricTypes, _, _, err := reconciler.computeRecommendations(
				context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
			require.NoError(t, err)
			require.NotNil(t, rec, "Prometheus gap must reuse the prior recommendation")
			assert.True(t, rec.Stale, "reused rec must be marked stale")
			require.NotNil(t, rec.LastDataTime)
			assert.True(t, rec.LastDataTime.Equal(&priorData), "LastDataTime must stay the last non-empty sample")
			assert.False(t, policy.Status.Recommendations[0].Stale, "reuse must DeepCopy, not mutate status in place")
			if tt.wantQueryErrors > 0 {
				assert.Equal(t, tt.wantQueryErrors, qErrors)
				assert.ElementsMatch(t, tt.wantFailedTypes, failedMetricTypes)
			}
		})
	}
}

// TestComputeRecommendations_LastDataTimeOlderThanFreshnessBoundIsStale
// covers the live stale check (now.Sub(last) > 3*queryStep). A
// historyWindow comparison cannot fire: the range is [now-window, now],
// so last is never older than historyWindow.
func TestComputeRecommendations_LastDataTimeOlderThanFreshnessBoundIsStale(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 2 * time.Hour}
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(1)
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	// 30m is inside the 2h history window and outside 3*queryStep (15m).
	old := now.Add(-30 * time.Minute)
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return []rsmetrics.Sample{{Timestamp: old, Value: 0.1}}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.Stale, "last sample older than 3*queryStep must mark the rec stale")
	require.NotNil(t, rec.LastDataTime)
	assert.True(t, rec.LastDataTime.Equal(&metav1.Time{Time: old}), "LastDataTime is the last non-empty sample, not now")
}

func TestComputeRecommendations_InWindowSampleOlderThanFreshnessIsStale(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 7 * 24 * time.Hour}
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(1)
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	// Two hours old is inside a 7d history window but outside 3*queryStep (15m).
	old := now.Add(-2 * time.Hour)
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, start, end time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if old.Before(start) || old.After(end) {
				return nil, nil
			}
			return []rsmetrics.Sample{{Timestamp: old, Value: 0.1}}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.Stale, "in-window sample older than 3*queryStep must be stale")
	require.NotNil(t, rec.LastDataTime)
	assert.True(t, rec.LastDataTime.Equal(&metav1.Time{Time: old}))
}

// TestComputeRecommendations_NewestNonFiniteDoesNotRefreshLastDataTime is
// the live-red contract: NaN/Inf at now must not advance LastDataTime or
// clear stale. Removing the finite-only filter fails this test.
func TestComputeRecommendations_NewestNonFiniteDoesNotRefreshLastDataTime(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(1)
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	old := now.Add(-30 * time.Minute)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{
				"main": {
					{Timestamp: old, Value: 0.1},
					{Timestamp: now, Value: math.NaN()},
					{Timestamp: now, Value: math.Inf(1)},
				},
			}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.Stale, "newest NaN/Inf must not refresh LastDataTime; 30m-old finite sample is stale")
	require.NotNil(t, rec.LastDataTime)
	assert.True(t, rec.LastDataTime.Equal(&metav1.Time{Time: old}), "LastDataTime must stay the last finite sample, not now")
	assert.False(t, rec.LastDataTime.Time.Equal(now), "NaN/Inf at now must not become LastDataTime")
}

func TestComputeRecommendations_ExpiredReuseIsDropped(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	expired := metav1.NewTime(now.Add(-time.Hour))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &expired,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpuRec},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "reuse must expire when LastDataTime is older than 3*queryStep")
}

func TestComputeRecommendations_ReuseIgnoresMissingLastDataTime(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: nil,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpuRec},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "reuse must refuse a prior rec with missing LastDataTime")
}

func TestComputeRecommendations_ReuseIgnoresDifferentKind(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		},
	}
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpuRec},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, sts, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "must not reuse a Deployment rec for a StatefulSet of the same name")
}

func TestComputeRecommendations_ReuseIgnoresEmptyPriorKind(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "",
		LastDataTime: &priorData,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpuRec},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "empty prior Kind must not reuse onto a typed workload")
}

func TestComputeRecommendations_FreshDataClearsStale(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(1)
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	staleStamp := metav1.NewTime(now.Add(-48 * time.Hour))
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &staleStamp,
		Stale:        true,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
		}},
	}}

	fresh := now.Add(-5 * time.Minute)
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return []rsmetrics.Sample{{Timestamp: fresh, Value: 0.1}}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.Stale, "fresh Prometheus data must clear stale")
	require.NotNil(t, rec.LastDataTime)
	assert.True(t, rec.LastDataTime.After(staleStamp.Time), "LastDataTime must advance to the new samples")
}

func TestComputeRecommendations_AllNaNInfSamplesLogsDataQuality(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return samples with all NaN values. BuildProfile filters them out,
	// producing DataPoints == 0 while len(samples) > 0. This exercises
	// the V(1) "All CPU/memory samples were NaN/Inf" log path added in #171.
	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nanSamples, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "no recommendation when all samples are NaN")
}

func TestComputeRecommendations_CPUAllNaNMemoryValid(t *testing.T) {
	// CPU samples are all NaN, but memory samples are valid.
	// The recommendation should still be produced using memory data,
	// with CPU staying at the current value.
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	reconciler := newReconcilerWithClient()

	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				// Valid memory samples (~256Mi usage).
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 256*1024*1024)}, nil
			}
			// CPU: all NaN.
			return map[string][]rsmetrics.Sample{"main": nanSamples}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec, "recommendation should be produced from memory data even when CPU is all NaN")
	require.Len(t, rec.Containers, 1)
	// CPU should stay at current because CPU had no valid data points.
	assert.Equal(t, resource.MustParse("100m"), rec.Containers[0].Recommended.CPURequest,
		"CPU should stay at current when CPU samples are all NaN")
	// Memory should differ from current (recommendations engine processes valid memory data).
	assert.NotEqual(t, resource.MustParse("128Mi"), rec.Containers[0].Recommended.MemoryRequest,
		"Memory recommendation should change when memory samples are valid")
}

// TestComputeRecommendations_OneSidedMemoryGapDoesNotRecommendTemplate is
// the live-red contract: CPU series can be fresh while memory is empty or
// all NaN. The missing-memory arm must stay fresh and keep live/last
// (1Gi), not the pod-template request (256Mi). A Stale early-return would
// still pass if the hold were deleted.
func TestComputeRecommendations_OneSidedMemoryGapDoesNotRecommendTemplate(t *testing.T) {
	templateMem := resource.MustParse("256Mi")
	liveMem := resource.MustParse("1Gi")

	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	tests := []struct {
		name          string
		memorySamples map[string][]rsmetrics.Sample
		useLivePod    bool
		usePriorRec   bool
		liveRequest   string
	}{
		{name: "empty memory samples, live 1Gi", useLivePod: true},
		{name: "NaN memory samples, live 1Gi", memorySamples: map[string][]rsmetrics.Sample{"main": nanSamples}, useLivePod: true},
		{name: "empty memory samples, last rec 1Gi", usePriorRec: true},
		// Finding 2: pods still at the template must not overwrite last rec.
		{name: "empty memory samples, live at template, last rec 1Gi", useLivePod: true, usePriorRec: true, liveRequest: "256Mi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			deploy := newTestDeployment("api-server", "default", nil)
			deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = templateMem.DeepCopy()

			if tt.usePriorRec {
				priorData := metav1.NewTime(now.Add(-time.Minute))
				policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
					Workload:     "api-server",
					Kind:         "Deployment",
					LastDataTime: &priorData,
					Stale:        false,
					Containers: []attunev1alpha1.ContainerRecommendation{{
						Name: "main",
						Recommended: attunev1alpha1.ResourceValues{
							CPURequest:    resource.MustParse("500m"),
							MemoryRequest: liveMem.DeepCopy(),
						},
					}},
				}}
			}

			var pods []corev1.Pod
			if tt.useLivePod {
				liveReq := "1Gi"
				if tt.liveRequest != "" {
					liveReq = tt.liveRequest
				}
				pods = []corev1.Pod{*newResizePod("api-server", "500m", liveReq, "1000m", "1Gi")}
			}

			reconciler := newReconcilerWithClient()
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					if strings.Contains(query, "memory_working_set_bytes") {
						if tt.memorySamples != nil {
							return tt.memorySamples, nil
						}
						return map[string][]rsmetrics.Sample{}, nil
					}
					return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
				},
			}

			rec, _, _, _, _, err := reconciler.computeRecommendations(
				context.Background(), policy, deploy, mc, nil, nil, nil, nil, pods)
			require.NoError(t, err)
			require.NotNil(t, rec, "CPU-only data must still produce a recommendation")
			require.Len(t, rec.Containers, 1)
			require.False(t, rec.Stale,
				"held missing-memory arm must stay fresh; a stale-skip would green without a hold")

			got := rec.Containers[0].Recommended.MemoryRequest
			assert.True(t, got.Equal(liveMem),
				"fresh rec must keep live/last memory %s, got %s", liveMem.String(), got.String())
		})
	}
}

// TestComputeRecommendations_OneSidedCPUGapDoesNotRecommendTemplate is the
// CPU twin of the memory-gap hold: memory series can be fresh while CPU
// is empty or all NaN. The missing-CPU arm must stay fresh at live/last,
// not the pod-template request.
func TestComputeRecommendations_OneSidedCPUGapDoesNotRecommendTemplate(t *testing.T) {
	templateCPU := resource.MustParse("100m")
	liveCPU := resource.MustParse("500m")

	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	tests := []struct {
		name        string
		cpuSamples  map[string][]rsmetrics.Sample
		useLivePod  bool
		usePriorRec bool
		liveRequest string
	}{
		{name: "empty CPU samples, live 500m", useLivePod: true},
		{name: "NaN CPU samples, live 500m", cpuSamples: map[string][]rsmetrics.Sample{"main": nanSamples}, useLivePod: true},
		{name: "empty CPU samples, last rec 500m", usePriorRec: true},
		{name: "empty CPU samples, live at template, last rec 500m", useLivePod: true, usePriorRec: true, liveRequest: "100m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			deploy := newTestDeployment("api-server", "default", nil)
			deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = templateCPU.DeepCopy()

			if tt.usePriorRec {
				priorData := metav1.NewTime(now.Add(-time.Minute))
				policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
					Workload:     "api-server",
					Kind:         "Deployment",
					LastDataTime: &priorData,
					Stale:        false,
					Containers: []attunev1alpha1.ContainerRecommendation{{
						Name: "main",
						Recommended: attunev1alpha1.ResourceValues{
							CPURequest:    liveCPU.DeepCopy(),
							MemoryRequest: resource.MustParse("1Gi"),
						},
					}},
				}}
			}

			var pods []corev1.Pod
			if tt.useLivePod {
				liveReq := "500m"
				if tt.liveRequest != "" {
					liveReq = tt.liveRequest
				}
				pods = []corev1.Pod{*newResizePod("api-server", liveReq, "1Gi", "1000m", "1Gi")}
			}

			reconciler := newReconcilerWithClient()
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					if strings.Contains(query, "cpu_usage_seconds_total") {
						if tt.cpuSamples != nil {
							return tt.cpuSamples, nil
						}
						return map[string][]rsmetrics.Sample{}, nil
					}
					return map[string][]rsmetrics.Sample{"main": generateSamples(200, 256*1024*1024)}, nil
				},
			}

			rec, _, _, _, _, err := reconciler.computeRecommendations(
				context.Background(), policy, deploy, mc, nil, nil, nil, nil, pods)
			require.NoError(t, err)
			require.NotNil(t, rec, "memory-only data must still produce a recommendation")
			require.Len(t, rec.Containers, 1)
			require.False(t, rec.Stale,
				"held missing-CPU arm must stay fresh; a stale-skip would green without a hold")

			got := rec.Containers[0].Recommended.CPURequest
			assert.True(t, got.Equal(liveCPU),
				"fresh rec must keep live/last CPU %s, got %s", liveCPU.String(), got.String())
		})
	}
}

// TestComputeRecommendations_HoldDropsLeftoverTemplateLimit is the live-red
// contract for a hold whose live request has no limit: Recommended must
// not keep the template limit, or clampRequestsToLimits shrinks the hold.
func TestComputeRecommendations_HoldDropsLeftoverTemplateLimit(t *testing.T) {
	templateMem := resource.MustParse("256Mi")
	templateLim := resource.MustParse("512Mi")
	liveMem := resource.MustParse("1Gi")

	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = templateMem.DeepCopy()
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = templateLim.DeepCopy()

	pod := newResizePod("api-server", "500m", "1Gi", "1000m", "1Gi")
	delete(pod.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)

	reconciler := newReconcilerWithClient()
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return map[string][]rsmetrics.Sample{}, nil
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(
		context.Background(), policy, deploy, mc, nil, nil, nil, nil, []corev1.Pod{*pod})
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	require.False(t, rec.Stale, "held live memory must stay fresh")

	got := rec.Containers[0]
	assert.True(t, got.Recommended.MemoryRequest.Equal(liveMem),
		"held memory request must stay live %s, got %s", liveMem.String(), got.Recommended.MemoryRequest.String())
	assert.True(t, got.Recommended.MemoryLimit.IsZero(),
		"held rec must not keep template memory limit %s when live has none, got %s",
		templateLim.String(), got.Recommended.MemoryLimit.String())

	target, clamped := buildResizeTarget(got)
	gotTargetMem := target.Requests[corev1.ResourceMemory]
	assert.True(t, gotTargetMem.Equal(liveMem),
		"resize target must keep held %s, got %s", liveMem.String(), gotTargetMem.String())
	assert.Empty(t, clamped, "leftover template limit must not clamp the held request")
}

func TestComputeRecommendations_QueryError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	rec, qErrors, failedMetricTypes, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.Greater(t, qErrors, 0, "query failures should be counted")
	assert.ElementsMatch(t, []string{"CPU", "memory"}, failedMetricTypes)
}

func TestComputeRecommendations_PartialQueryErrorTracksFailedMetricType(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return nil, fmt.Errorf("memory query failed")
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}

	rec, qErrors, failedMetricTypes, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 1, qErrors)
	assert.Equal(t, []string{"memory"}, failedMetricTypes)
}

// TestComputeRecommendations_SeriesCappedUsesPartialData is the live-red
// contract: ErrSeriesCapped is a soft error. Treating it as a hard fail
// reuses the prior stale row and fails this test.
func TestComputeRecommendations_SeriesCappedUsesPartialData(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(1)
	deploy := newTestDeployment("api-server", "default", nil)
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-2 * time.Minute))
	priorCPU, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	priorMem, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Stale:        false,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    priorCPU,
				MemoryRequest: priorMem,
			},
		}},
	}}

	fresh := now.Add(-30 * time.Second)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			samples := []rsmetrics.Sample{{Timestamp: fresh, Value: 0.1}}
			if !strings.Contains(query, "cpu_usage_seconds_total") {
				samples = []rsmetrics.Sample{{Timestamp: fresh, Value: 128 * 1024 * 1024}}
			}
			return map[string][]rsmetrics.Sample{"main": samples}, fmt.Errorf("%w: kept 1 series", rsmetrics.ErrSeriesCapped)
		},
	}

	rec, qErrors, _, _, seriesCapped, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.True(t, seriesCapped, "ErrSeriesCapped must surface as seriesCapped, not a hard query error")
	assert.Equal(t, 0, qErrors, "series cap is a soft error; partial samples must not increment queryErrors")
	require.NotNil(t, rec)
	assert.False(t, rec.Stale, "partial capped samples must produce a fresh rec, not reuse the prior status row")
	require.NotNil(t, rec.LastDataTime)
	assert.True(t, rec.LastDataTime.Equal(&metav1.Time{Time: fresh}), "LastDataTime must come from the partial samples")
	assert.False(t, rec.LastDataTime.Equal(&priorData), "must not reuse the prior status LastDataTime")
}

func TestComputeRecommendations_ContextCancelledDuringParallelQueries(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	ctx, cancel := context.WithCancel(context.Background())
	var queryCalls atomic.Int32
	mc := &mockCollector{
		queryRangeGroupedFunc: func(qctx context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			queryCalls.Add(1)
			// Simulate slow query: wait for context cancellation.
			cancel() // Cancel as soon as first query starts.
			<-qctx.Done()
			return nil, qctx.Err()
		},
	}

	rec, qErrors, _, _, _, err := reconciler.computeRecommendations(ctx, policy, deploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.Equal(t, 2, qErrors, "both queries should report failure when context is cancelled")
}

func TestComputeRecommendations_EmptyContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	emptyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{}},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, emptyDeploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
}

func TestComputeRecommendations_AllowDecreaseBlocked(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// AllowDecrease is nil (default) — memory decreases should be clamped.

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return very low memory usage (0.001 cores CPU, ~1MiB memory)
	// to produce recommendations lower than current (512Mi).
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.001), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// Memory should be clamped to current (512Mi) since AllowDecrease is nil.
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Cmp(resource.MustParse("512Mi")) >= 0,
		"memory should not decrease below current when AllowDecrease is nil, got %s", rec.Containers[0].Recommended.MemoryRequest.String())
}

func TestComputeRecommendations_CPUAllowDecreaseNilAllowsDecrease(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// CPU AllowDecrease is nil (default) — CPU decreases should be allowed.
	require.Nil(t, policy.Spec.CPU.AllowDecrease)
	// Use 4000m current request with 200m actual usage (0.2 cores). With 500
	// data points (good confidence), the recommendation should be well under 4000m.
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("4000m")
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(500, 0.2), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// CPU should decrease below current (4000m) when AllowDecrease is nil (defaults to true for CPU).
	cpuRec := rec.Containers[0].Recommended.CPURequest
	assert.True(t, cpuRec.Cmp(resource.MustParse("4000m")) < 0,
		"CPU should decrease below current when AllowDecrease is nil, got %s", cpuRec.String())
}

func TestComputeRecommendations_CPUAllowDecreaseBlocked(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// Explicitly disable CPU decreases. nil defaults to true for CPU.
	policy.Spec.CPU.AllowDecrease = boolPtr(false)

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return very low CPU usage to produce a recommendation lower than current (500m).
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.001), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// CPU should be clamped to current (500m) since AllowDecrease is explicitly false.
	assert.True(t, rec.Containers[0].Recommended.CPURequest.Cmp(resource.MustParse("500m")) >= 0,
		"CPU should not decrease below current when AllowDecrease is false, got %s", rec.Containers[0].Recommended.CPURequest.String())
}

func TestComputeRecommendations_RequestsOnly(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// ControlledValues defaults to RequestsOnly when nil.
	// Also verify the explicit "RequestsOnly" value behaves identically.
	for _, cv := range []struct {
		name string
		val  *string
	}{
		{"nil (default)", nil},
		{"explicit RequestsOnly", stringPtr("RequestsOnly")},
	} {
		t.Run(cv.name, func(t *testing.T) {
			policy.Spec.CPU.ControlledValues = cv.val
			policy.Spec.Memory.ControlledValues = cv.val

			deploy := newTestDeployment("api-server", "default", nil)
			reconciler := newReconcilerWithClient()

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}

			rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
			require.NoError(t, err)
			require.NotNil(t, rec)
			require.Len(t, rec.Containers, 1)

			c := rec.Containers[0]
			// Requests should be adjusted by the recommendation engine.
			assert.False(t, c.Recommended.CPURequest.IsZero(), "CPURequest should be set")
			assert.False(t, c.Recommended.MemoryRequest.IsZero(), "MemoryRequest should be set")

			// With RequestsOnly, limits should stay at the CURRENT values (not scaled).
			// The deployment has limits: CPU=1000m, Memory=1Gi.
			assert.True(t, c.Recommended.CPULimit.Equal(resource.MustParse("1000m")),
				"CPULimit should be unchanged at 1000m, got %s", c.Recommended.CPULimit.String())
			assert.True(t, c.Recommended.MemoryLimit.Equal(resource.MustParse("1Gi")),
				"MemoryLimit should be unchanged at 1Gi, got %s", c.Recommended.MemoryLimit.String())

			// Verify requests actually changed from the original 500m/512Mi.
			original := resource.MustParse("500m")
			assert.NotEqual(t, original.MilliValue(), c.Recommended.CPURequest.MilliValue(),
				"CPURequest should differ from the original 500m")
		})
	}
}

func TestComputeRecommendations_RequestsAndLimits(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ral := "RequestsAndLimits"
	policy.Spec.CPU.ControlledValues = &ral
	policy.Spec.Memory.ControlledValues = &ral

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	c := rec.Containers[0]
	// With RequestsAndLimits, limits must be scaled proportionally.
	assert.False(t, c.Recommended.CPULimit.IsZero(), "CPULimit should be set when ControlledValues=RequestsAndLimits")
	assert.False(t, c.Recommended.MemoryLimit.IsZero(), "MemoryLimit should be set when ControlledValues=RequestsAndLimits")

	// The deployment has 2:1 ratio (limits=1000m, requests=500m for CPU; limits=1Gi, requests=512Mi for memory).
	// Limits should be proportionally scaled from the new request.
	cpuRatio := float64(c.Recommended.CPULimit.MilliValue()) / float64(c.Recommended.CPURequest.MilliValue())
	assert.InDelta(t, 2.0, cpuRatio, 0.01, "CPU limit/request ratio should preserve the original 2:1 ratio")

	memRatio := float64(c.Recommended.MemoryLimit.Value()) / float64(c.Recommended.MemoryRequest.Value())
	assert.InDelta(t, 2.0, memRatio, 0.01, "Memory limit/request ratio should preserve the original ~2:1 ratio")
}

func TestComputeRecommendations_BatchesQueriesPerWorkload(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers = append(deploy.Spec.Template.Spec.Containers, corev1.Container{
		Name:  "sidecar",
		Image: "busybox",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	reconciler := newReconcilerWithClient()

	var mu sync.Mutex
	calls := make(map[string]int)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			mu.Lock()
			calls[query]++
			mu.Unlock()
			return map[string][]rsmetrics.Sample{
				"main":    generateSamples(200, 0.1),
				"sidecar": generateSamples(200, 0.05),
			}, nil
		},
	}

	rec, qErrors, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, qErrors)
	require.Len(t, rec.Containers, 2)
	assert.Len(t, calls, 2, "expected one CPU query and one memory query per workload")
	for _, count := range calls {
		assert.Equal(t, 1, count)
	}
}

func TestComputeRecommendations_UsesPodLevelSeriesWithoutExtraQuery(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu_usage_seconds_total") {
				return map[string][]rsmetrics.Sample{"": generateSamples(200, 0.1)}, nil
			}
			return map[string][]rsmetrics.Sample{"": generateSamples(200, 128*1024*1024)}, nil
		},
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("unexpected extra fallback query: %s", query)
		},
	}

	rec, qErrors, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, qErrors)
	require.Len(t, rec.Containers, 1)
}

// TestComputeRecommendations_MemoryFromCPURatioWaitsForCPU is the live-red
// contract for #819: memoryFromCpuRatio must not publish a memory-usage rec
// when CPU rate() is empty. That rec flipped Ready to Monitoring and
// stretched requeue to cooldown+jitter (up to 3m), so the nightly wait
// never saw derived from CPU via memoryFromCpuRatio=2.0.
func TestComputeRecommendations_MemoryFromCPURatioWaitsForCPU(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.Memory.AllowDecrease = boolPtr(true)
	deploy := newTestDeployment("api-server", "default", nil)
	pods := []corev1.Pod{*newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")}
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 8*1024*1024)}, nil
			}
			return map[string][]rsmetrics.Sample{}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, pods)
	require.NoError(t, err)
	assert.Nil(t, rec, "memoryFromCpuRatio must not publish a usage rec while CPU samples are missing")
}

func TestComputeRecommendations_MemoryFromCPURatioDoesNotReuseUsageRec(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.Memory.AllowDecrease = boolPtr(true)
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	now := time.Date(2026, 9, 19, 8, 18, 12, 0, time.UTC)
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-time.Minute))
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Stale:        false,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("64Mi"),
			},
			Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
				Memory: &attunev1alpha1.ResourceRecommendationExplanation{
					FinalAdjustment: "podAggregation=Max; burstSensitivity=0.1",
				},
			},
		}},
	}}

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 8*1024*1024)}, nil
			}
			return map[string][]rsmetrics.Sample{}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "must not reuse a usage rec while memoryFromCpuRatio is waiting for CPU")
}

func TestComputeRecommendations_MemoryFromCPURatioEmptyQueryStillReuses(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	now := time.Date(2026, 9, 19, 8, 18, 12, 0, time.UTC)
	reconciler.SetNowFunc(func() time.Time { return now })

	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Stale:        false,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    cpuRec,
				MemoryRequest: memRec,
			},
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec, "Prometheus outage must still reuse the prior rec when both queries are empty")
	assert.True(t, rec.Stale)
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Equal(memRec))
}

func TestComputeRecommendations_MemoryFromCPURatioDerivesFromCPU(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.Memory.AllowDecrease = boolPtr(true)
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 8*1024*1024)}, nil
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.Memory)
	assert.Contains(t, rec.Containers[0].Explanation.Memory.FinalAdjustment, "memoryFromCpuRatio=2.0")
	floor, err := resource.ParseQuantity("128Mi")
	require.NoError(t, err)
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Cmp(floor) > 0,
		"ratio-derived memory must exceed idle RSS minAllowed (got %s)",
		rec.Containers[0].Recommended.MemoryRequest.String())
}

func TestComputeRecommendations_PopulatesExplanation(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{
				"main": generateSamples(200, 0.1),
			}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.CPU)
	require.NotNil(t, rec.Containers[0].Explanation.Memory)
	assert.False(t, rec.Containers[0].Explanation.CPU.RawPercentile.IsZero())
	assert.False(t, rec.Containers[0].Explanation.CPU.Final.IsZero())
	assert.False(t, rec.Containers[0].Explanation.Memory.RawPercentile.IsZero())
	assert.False(t, rec.Containers[0].Explanation.Memory.Final.IsZero())
}

// ---------- excludedContainers ----------

func TestComputeRecommendations_ExcludedContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"istio-proxy"}

	// Deployment with two containers: main + istio-proxy sidecar.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)

	// Only "main" should have a recommendation; "istio-proxy" is excluded.
	assert.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
}

func TestComputeRecommendations_ExcludeAllContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"main"}

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "all containers excluded, should return nil")
}

func TestComputeRecommendations_ExcludeAllDoesNotReusePriorAsStale(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"main"}
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	priorData := metav1.NewTime(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &priorData,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
		}},
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rec, "exclude-all must drop the rec, not reuse it as a Prometheus gap")
}

func TestComputeRecommendations_KnownSidecarsExcludedByDefault(t *testing.T) {
	// No ExcludedContainers and no ExcludeKnownSidecars set: default true
	// still skips istio-proxy via EffectiveExcludedContainers.
	policy := newTestPolicy("test-policy", "default")

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	reconciler := newReconcilerWithClient()
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	// Pass nil excludeSet so computeRecommendations builds via EffectiveExcludedContainers.
	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
}

func TestComputeRecommendations_KnownSidecarsOptOut(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	falseVal := false
	policy.Spec.ExcludeKnownSidecars = &falseVal

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	reconciler := newReconcilerWithClient()
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	names := make([]string, 0, len(rec.Containers))
	for _, c := range rec.Containers {
		names = append(names, c.Name)
	}
	assert.ElementsMatch(t, []string{"main", "istio-proxy"}, names)
}

func TestComputeRecommendations_NanInfSamplesMetric(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Real Prometheus collector: NaN/Inf are dropped before samples leave,
	// so the operator counter must increment at the collector, not only
	// when a mock skips that filter.
	response := `{
		"status": "success",
		"data": {
			"resultType": "matrix",
			"result": [
				{
					"metric": {"container": "main"},
					"values": [
						[1700000000, "NaN"],
						[1700000060, "Inf"],
						[1700000120, "-Inf"]
					]
				}
			]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	collector, err := rsmetrics.NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	beforeCPU := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "untracked", "cpu"))
	beforeMem := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "untracked", "memory"))
	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, collector, nil, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "should produce no recommendation when all data is NaN/Inf")
	afterCPU := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "untracked", "cpu"))
	afterMem := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "untracked", "memory"))
	assert.Equal(t, beforeCPU+1, afterCPU, "collector must increment once when the CPU series is entirely non-finite")
	assert.Equal(t, beforeMem+1, afterMem, "collector must increment once when the memory series is entirely non-finite")
}
