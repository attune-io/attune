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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func TestExcludeStartupHistory_NilAndFalse(t *testing.T) {
	t.Parallel()
	trueVal := true
	falseVal := false

	require.False(t, excludeStartupHistory(nil))
	require.False(t, excludeStartupHistory(&attunev1alpha1.AttunePolicy{}))
	require.False(t, excludeStartupHistory(&attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{},
		},
	}))
	require.False(t, excludeStartupHistory(&attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{},
			},
		},
	}))
	require.False(t, excludeStartupHistory(&attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					ExcludeFromHistory: &falseVal,
				},
			},
		},
	}))
	require.True(t, excludeStartupHistory(&attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					ExcludeFromHistory: &trueVal,
				},
			},
		},
	}))
}

func TestFilterStartupCPUSamples(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	boost := 5 * time.Minute
	rateWindow := time.Minute

	youngCreated := now.Add(-1 * time.Minute)
	oldCreated := now.Add(-2 * time.Hour)
	// creation + 5m + 1m. Young pod (created now-1m) cuts off at now+5m.
	// Old pod (created now-2h) cuts off at now-2h+6m.
	oldCutoff := oldCreated.Add(boost).Add(rateWindow)

	young := podAt("young", youngCreated)
	old := podAt("old", oldCreated)
	otherOld := podAt("other-old", oldCreated)

	tests := []struct {
		name        string
		samples     []rsmetrics.Sample
		pods        []corev1.Pod
		mode        rsmetrics.PodAggregationMode
		wantValues  []float64
		wantPods    []string
		wantDropped bool
		wantSkipped bool
		wantLen     int
	}{
		{
			name: "young spike drops and max follows old pod",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 10, Pod: "young"},
				{Timestamp: now.Add(-30 * time.Second), Value: 10, Pod: "young"},
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now.Add(-time.Minute), Value: 0.2, Pod: "old"},
			},
			pods:        []corev1.Pod{young, old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0.2, 0.2},
			wantDropped: true,
			wantLen:     2,
		},
		{
			name: "spike on old pod after cutoff remains",
			samples: []rsmetrics.Sample{
				{Timestamp: oldCutoff, Value: 10, Pod: "old"},
				{Timestamp: oldCutoff.Add(time.Minute), Value: 10, Pod: "old"},
				{Timestamp: oldCutoff.Add(-time.Second), Value: 9, Pod: "old"},
			},
			pods:        []corev1.Pod{old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{10, 10},
			wantDropped: true,
			wantLen:     2,
		},
		{
			name: "all live pods inside the window",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 10, Pod: "young"},
				{Timestamp: now.Add(time.Minute), Value: 8, Pod: "young"},
			},
			pods:        []corev1.Pod{young},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  nil,
			wantDropped: true,
			wantLen:     0,
		},
		{
			name: "zero after the window is kept",
			samples: []rsmetrics.Sample{
				{Timestamp: oldCutoff, Value: 0, Pod: "old"},
				{Timestamp: youngCreated, Value: 4, Pod: "young"},
			},
			pods:        []corev1.Pod{young, old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0},
			wantDropped: true,
			wantLen:     1,
		},
		{
			name: "max reapplied across two old pods at one timestamp",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 0.5, Pod: "other-old"},
			},
			pods:        []corev1.Pod{old, otherOld},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0.5},
			wantDropped: false,
			wantLen:     1,
		},
		{
			name: "empty mode is max",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 0.5, Pod: "other-old"},
			},
			pods:       []corev1.Pod{old, otherOld},
			mode:       "",
			wantValues: []float64{0.5},
			wantLen:    1,
		},
		{
			name: "series with no live pod stays in the max",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 0.9, Pod: "deleted"},
			},
			pods:        []corev1.Pod{old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0.9},
			wantDropped: false,
			wantSkipped: false,
			wantLen:     1,
		},
		{
			name: "unlabeled series is not merged into the max",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 10, Pod: ""},
			},
			pods:        []corev1.Pod{old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0.2, 10},
			wantPods:    []string{"", ""},
			wantDropped: false,
			wantSkipped: true,
			wantLen:     2,
		},
		{
			name: "dropped and skipped together",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 10, Pod: "young"},
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 7, Pod: ""},
			},
			pods:        []corev1.Pod{young, old},
			mode:        rsmetrics.PodAggregationMax,
			wantValues:  []float64{0.2, 7},
			wantPods:    []string{"", ""},
			wantDropped: true,
			wantSkipped: true,
			wantLen:     2,
		},
		{
			name: "avg uses pods present at the timestamp",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 0.6, Pod: "other-old"},
				{Timestamp: now.Add(time.Minute), Value: 0.4, Pod: "old"},
			},
			pods:       []corev1.Pod{old, otherOld},
			mode:       rsmetrics.PodAggregationAvg,
			wantValues: []float64{0.4, 0.4},
			wantLen:    2,
		},
		{
			name: "reused pod name keeps samples older than creation",
			samples: []rsmetrics.Sample{
				{Timestamp: now.Add(-2 * time.Hour), Value: 0.3, Pod: "web-0"},
				{Timestamp: now.Add(-9 * time.Minute), Value: 9, Pod: "web-0"},
				{Timestamp: now, Value: 0.4, Pod: "web-0"},
			},
			pods:        []corev1.Pod{podAt("web-0", now.Add(-10*time.Minute))},
			mode:        rsmetrics.PodAggregationNone,
			wantValues:  []float64{0.3, 0.4},
			wantPods:    []string{"web-0", "web-0"},
			wantDropped: true,
			wantLen:     2,
		},
		{
			name: "none concatenates surviving labeled points",
			samples: []rsmetrics.Sample{
				{Timestamp: now, Value: 0.2, Pod: "old"},
				{Timestamp: now, Value: 0.5, Pod: "other-old"},
				{Timestamp: now.Add(-time.Minute), Value: 10, Pod: "young"},
			},
			pods:        []corev1.Pod{young, old, otherOld},
			mode:        rsmetrics.PodAggregationNone,
			wantValues:  []float64{0.2, 0.5},
			wantPods:    []string{"old", "other-old"},
			wantDropped: true,
			wantLen:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterStartupCPUSamples(tt.samples, tt.pods, boost, rateWindow, tt.mode)
			require.Equal(t, tt.wantDropped, got.Dropped)
			require.Equal(t, tt.wantSkipped, got.Skipped)
			require.Len(t, got.Samples, tt.wantLen)
			if tt.wantValues == nil {
				require.Empty(t, got.Samples)
				return
			}
			gotValues := make([]float64, len(got.Samples))
			for i, sample := range got.Samples {
				gotValues[i] = sample.Value
			}
			require.Equal(t, tt.wantValues, gotValues)
			for i := 1; i < len(got.Samples); i++ {
				if got.Samples[i].Pod == "" && got.Samples[i-1].Pod != "" {
					continue
				}
				require.False(t, got.Samples[i].Timestamp.Before(got.Samples[i-1].Timestamp),
					"labeled series must be time-sorted before unlabeled samples")
			}
			if tt.wantPods != nil {
				gotPods := make([]string, len(got.Samples))
				for i, sample := range got.Samples {
					gotPods[i] = sample.Pod
				}
				require.Equal(t, tt.wantPods, gotPods)
			}
			if tt.name == "unlabeled series is not merged into the max" {
				require.Equal(t, 10.0, got.Samples[1].Value)
				require.Empty(t, got.Samples[1].Pod)
				require.Equal(t, 0.2, got.Samples[0].Value)
			}
		})
	}
}

func TestStartupHistory_DropsYoungSpikeAndNotes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("test-policy", "default")
	minPoints := int32(8)
	policy.Spec.MetricsSource.MinimumDataPoints = &minPoints
	rate := metav1.Duration{Duration: time.Minute}
	policy.Spec.MetricsSource.RateWindow = &rate
	policy.Spec.CPU.Overhead = "0"
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier:         "2.0",
		Duration:           metav1.Duration{Duration: 5 * time.Minute},
		ExcludeFromHistory: boolPtr(true),
	}
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })

	young := podAt("young", now.Add(-time.Minute))
	old := podAt("old", now.Add(-2*time.Hour))
	qb := &rsmetrics.PromQLQueryBuilder{Aggregation: rsmetrics.PodAggregationMax}

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "container_cpu_usage_seconds_total") {
				assert.Contains(t, query, "max by (pod, container)")
				assert.NotContains(t, query, "max by (container)")
				assert.NotContains(t, query, "avg by")
				return map[string][]rsmetrics.Sample{
					"main": append(steadyPodSamples(now, "old", 12, 0.05), rsmetrics.Sample{
						Timestamp: now, Value: 10, Pod: "young",
					}),
				}, nil
			}
			assert.Contains(t, query, "max by (container)")
			return map[string][]rsmetrics.Sample{
				"main": steadyPodSamples(now, "", 12, 128*1024*1024),
			}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(
		context.Background(), policy, deploy, mc, qb, nil, nil, nil, []corev1.Pod{young, old})
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	cpu := rec.Containers[0].Recommended.CPURequest
	ceiling, parseErr := resource.ParseQuantity("400m")
	require.NoError(t, parseErr)
	assert.True(t, cpu.Cmp(ceiling) < 0, "young spike must not set the CPU rec, got %s", cpu.String())

	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.CPU)
	cpuNote := rec.Containers[0].Explanation.CPU.FinalAdjustment
	assert.Contains(t, cpuNote, "podAggregation=Max")
	assert.Contains(t, cpuNote, "startupExcluded")
	assert.NotContains(t, cpuNote, "startupExcluded=skipped")
	require.NotNil(t, rec.Containers[0].Explanation.Memory)
	assert.Contains(t, rec.Containers[0].Explanation.Memory.FinalAdjustment, "startupExcluded")
	assert.Contains(t, rec.Containers[0].Explanation.Memory.FinalAdjustment, "podAggregation=Max")
}

func TestStartupHistory_NilExcludeKeepsYoungSpike(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("test-policy", "default")
	minPoints := int32(8)
	policy.Spec.MetricsSource.MinimumDataPoints = &minPoints
	policy.Spec.CPU.Overhead = "0"
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 5 * time.Minute},
	}
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })
	young := podAt("young", now.Add(-time.Minute))

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "container_cpu_usage_seconds_total") {
				assert.Contains(t, query, "max by (container)")
				return map[string][]rsmetrics.Sample{
					"main": steadyPodSamples(now, "young", 12, 10),
				}, nil
			}
			return map[string][]rsmetrics.Sample{
				"main": steadyPodSamples(now, "", 12, 128*1024*1024),
			}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(
		context.Background(), policy, deploy, mc, nil, nil, nil, nil, []corev1.Pod{young})
	require.NoError(t, err)
	require.NotNil(t, rec)
	floor, parseErr := resource.ParseQuantity("600m")
	require.NoError(t, parseErr)
	cpu := rec.Containers[0].Recommended.CPURequest
	assert.True(t, cpu.Cmp(floor) > 0, "nil exclude must keep the spike, got %s", cpu.String())
	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.CPU)
	assert.NotContains(t, rec.Containers[0].Explanation.CPU.FinalAdjustment, "startupExcluded")
}

func TestStartupHistory_BlockedReuseSkipsStaleRatio(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("test-policy", "default")
	minPoints := int32(4)
	policy.Spec.MetricsSource.MinimumDataPoints = &minPoints
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier:         "2.0",
		Duration:           metav1.Duration{Duration: 5 * time.Minute},
		ExcludeFromHistory: boolPtr(true),
	}
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{
		ratioDerivedWorkloadRec(now, "derived from CPU via memoryFromCpuRatio=2.0"),
	}
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })
	young := podAt("young", now.Add(-time.Minute))

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "container_cpu_usage_seconds_total") {
				return map[string][]rsmetrics.Sample{
					"main": steadyPodSamples(now, "young", 12, 10),
				}, nil
			}
			return map[string][]rsmetrics.Sample{
				"main": steadyPodSamples(now, "", 12, 128*1024*1024),
			}, nil
		},
	}

	rec, _, _, _, _, err := reconciler.computeRecommendations(
		context.Background(), policy, deploy, mc, nil, nil, nil, nil, []corev1.Pod{young})
	require.NoError(t, err)
	assert.Nil(t, rec, "startup exclusion below minimumDataPoints must not reuse a ratio CPU rec")
}

func TestStartupHistory_GapStillReusesPrior(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	priorCPU, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	old := podAt("old", now.Add(-2*time.Hour))

	tests := []struct {
		name      string
		query     func(query string) (map[string][]rsmetrics.Sample, error)
		pods      []corev1.Pod
		wantError int
	}{
		{
			name: "empty cpu query",
			query: func(string) (map[string][]rsmetrics.Sample, error) {
				return map[string][]rsmetrics.Sample{}, nil
			},
		},
		{
			name: "cpu query error",
			query: func(query string) (map[string][]rsmetrics.Sample, error) {
				if strings.Contains(query, "container_cpu_usage_seconds_total") {
					return nil, errors.New("connection refused")
				}
				return map[string][]rsmetrics.Sample{}, nil
			},
			wantError: 1,
		},
		{
			name: "post-window series still under the minimum",
			query: func(query string) (map[string][]rsmetrics.Sample, error) {
				if strings.Contains(query, "container_cpu_usage_seconds_total") {
					return map[string][]rsmetrics.Sample{
						"main": {
							{Timestamp: now, Value: 0.05, Pod: "old"},
							{Timestamp: now.Add(-time.Minute), Value: 0.05, Pod: "old"},
						},
					}, nil
				}
				return map[string][]rsmetrics.Sample{}, nil
			},
			pods: []corev1.Pod{old},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := newTestPolicy("test-policy", "default")
			minPoints := int32(8)
			policy.Spec.MetricsSource.MinimumDataPoints = &minPoints
			policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
				Multiplier:         "2.0",
				Duration:           metav1.Duration{Duration: 5 * time.Minute},
				ExcludeFromHistory: boolPtr(true),
			}
			policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
				Workload:     "api-server",
				Kind:         "Deployment",
				LastDataTime: &metav1.Time{Time: now.Add(-time.Minute)},
				Containers: []attunev1alpha1.ContainerRecommendation{{
					Name:        "main",
					Recommended: attunev1alpha1.ResourceValues{CPURequest: priorCPU},
				}},
			}}
			deploy := newTestDeployment("api-server", "default", nil)
			reconciler := newReconcilerWithClient()
			reconciler.SetNowFunc(func() time.Time { return now })
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					return tt.query(query)
				},
			}
			rec, qErrors, _, _, _, recErr := reconciler.computeRecommendations(
				context.Background(), policy, deploy, mc, nil, nil, nil, nil, tt.pods)
			require.NoError(t, recErr)
			require.NotNil(t, rec, "a gap that did not drop startup points must reuse the prior rec")
			assert.True(t, rec.Stale)
			require.Len(t, rec.Containers, 1)
			assert.True(t, rec.Containers[0].Recommended.CPURequest.Equal(priorCPU))
			assert.Equal(t, tt.wantError, qErrors)
		})
	}
}

func TestStartupHistory_DroppedCPUStaysAtLiveRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("test-policy", "default")
	minPoints := int32(4)
	policy.Spec.MetricsSource.MinimumDataPoints = &minPoints
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier:         "2.0",
		Duration:           metav1.Duration{Duration: 5 * time.Minute},
		ExcludeFromHistory: boolPtr(true),
	}
	high, err := resource.ParseQuantity("2000m")
	require.NoError(t, err)
	live, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &metav1.Time{Time: now.Add(-time.Minute)},
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: high},
		}},
	}}
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()
	reconciler.SetNowFunc(func() time.Time { return now })
	young := podAt("young", now.Add(-time.Minute))
	young.Spec.Containers = []corev1.Container{{
		Name: "main",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: live.DeepCopy()},
		},
	}}

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "container_cpu_usage_seconds_total") {
				return map[string][]rsmetrics.Sample{
					"main": steadyPodSamples(now, "young", 12, 10),
				}, nil
			}
			return map[string][]rsmetrics.Sample{
				"main": steadyPodSamples(now, "", 12, 128*1024*1024),
			}, nil
		},
	}

	rec, _, _, _, _, recErr := reconciler.computeRecommendations(
		context.Background(), policy, deploy, mc, nil, nil, nil, nil, []corev1.Pod{young})
	require.NoError(t, recErr)
	require.NotNil(t, rec, "memory points must still produce a recommendation")
	require.Len(t, rec.Containers, 1)
	assert.True(t, rec.Containers[0].Recommended.CPURequest.Equal(live),
		"dropped CPU must stay at the live request, got %s", rec.Containers[0].Recommended.CPURequest.String())
	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.Memory)
	assert.Contains(t, rec.Containers[0].Explanation.Memory.FinalAdjustment, "startupExcluded")
}

func TestStartupHistory_DatadogBuilderUnchanged(t *testing.T) {
	t.Parallel()
	qb := &rsmetrics.DatadogQueryBuilder{}
	got := cpuQueryBuilderForStartupHistory(qb)
	assert.Same(t, qb, got)
	query := got.BuildQuery("default", "api-.*", "", "cpu", time.Minute)
	assert.Contains(t, query, "pod_name")
	assert.NotContains(t, query, "max by (pod, container)")

	cw := &rsmetrics.CloudWatchQueryBuilder{}
	assert.Same(t, cw, cpuQueryBuilderForStartupHistory(cw))

	prom := &rsmetrics.PromQLQueryBuilder{Aggregation: rsmetrics.PodAggregationMax, CPUMetric: "attune:cpu:rate5m"}
	wrapped := cpuQueryBuilderForStartupHistory(prom)
	cpuQuery := wrapped.BuildQuery("default", "api-.*", "app", "cpu", 5*time.Minute)
	assert.Contains(t, cpuQuery, "max by (pod, container)")
	assert.Contains(t, cpuQuery, "attune:cpu:rate5m")
	assert.NotContains(t, cpuQuery, "max by (container)")
	memQuery := wrapped.BuildQuery("default", "api-.*", "app", "memory", time.Minute)
	assert.NotContains(t, memQuery, "max by (pod, container)")
	assert.Equal(t, rsmetrics.PodAggregationMax, prom.Aggregation)
	assert.Equal(t, "attune:cpu:rate5m", prom.CPUMetric)

	none := cpuQueryBuilderForStartupHistory(nil)
	noneCPU := none.BuildQuery("ns", "pod-.*", "", "cpu", time.Minute)
	assert.Contains(t, noneCPU, "max by (pod, container) (rate(")
	assert.NotContains(t, noneCPU, "max by (container)")

	var typedNil *rsmetrics.PromQLQueryBuilder
	typedCPU := cpuQueryBuilderForStartupHistory(typedNil).BuildQuery("ns", "pod-.*", "", "cpu", time.Minute)
	assert.Contains(t, typedCPU, "max by (pod, container) (rate(")
}

func steadyPodSamples(now time.Time, pod string, count int, value float64) []rsmetrics.Sample {
	out := make([]rsmetrics.Sample, count)
	for i := 0; i < count; i++ {
		out[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(count-1-i) * time.Minute),
			Value:     value,
			Pod:       pod,
		}
	}
	return out
}

func podAt(name string, created time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
		},
	}
}
