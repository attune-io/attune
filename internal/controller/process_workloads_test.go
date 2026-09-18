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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func TestProcessWorkloads_StaleRecNotCountedForReady(t *testing.T) {
	now := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	prior := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)

	dep := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(dep).Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{{
		Workload:     "api-server",
		Kind:         "Deployment",
		LastDataTime: &prior,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpuRec},
		}},
	}}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return now })
	r.MetricsFactory = mockMetricsFactory(&mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	})

	result := r.processWorkloads(context.Background(), policy, []client.Object{dep}, &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, nil
		},
	}, nil, nil)

	require.Len(t, result.recommendations, 1)
	assert.True(t, result.recommendations[0].Stale)
	assert.Equal(t, int32(0), result.workloadsWithRecs, "stale reuse must not keep Ready=Monitoring")
}

func TestProcessWorkloads_Parallel(t *testing.T) {
	// Track peak concurrent queries to prove parallelism.
	var inflight atomic.Int32
	var peakInflight atomic.Int32

	// Build 60 samples (enough to exceed the default minimumDataPoints of 48).
	now := time.Now()
	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}

	collector := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			cur := inflight.Add(1)
			// Track the peak concurrency.
			for {
				peak := peakInflight.Load()
				if cur <= peak || peakInflight.CompareAndSwap(peak, cur) {
					break
				}
			}
			// Simulate query latency to allow goroutines to overlap.
			time.Sleep(5 * time.Millisecond)
			inflight.Add(-1)
			return grouped, nil
		},
	}

	// Create 20 deployments to process in parallel.
	const numWorkloads = 20
	objs := make([]runtime.Object, 0, numWorkloads)
	workloads := make([]client.Object, 0, numWorkloads)
	for i := range numWorkloads {
		name := fmt.Sprintf("deploy-%d", i)
		dep := newTestDeployment(name, "default", map[string]string{"app": name})
		objs = append(objs, dep)
		workloads = append(workloads, dep)
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })

	result := r.processWorkloads(context.Background(), policy, workloads, collector, nil, nil)

	// All 20 workloads should produce recommendations.
	assert.Equal(t, int32(numWorkloads), result.workloadsWithRecs,
		"all workloads should have recommendations")
	assert.Len(t, result.recommendations, numWorkloads)
	assert.Equal(t, 0, result.totalQueryErrors)

	// Verify actual concurrency occurred (peak > 1 proves parallelism).
	assert.Greater(t, peakInflight.Load(), int32(1),
		"expected concurrent queries (peak inflight > 1)")
}

func TestProcessWorkloads_ParallelPartialFailure(t *testing.T) {
	// Verify that partial query failures don't corrupt results under
	// concurrent access. Even-numbered workloads return errors; odd succeed.
	var inflight atomic.Int32
	var peakInflight atomic.Int32

	now := time.Now()
	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}

	// Track which workloads should fail based on query content.
	// Each workload's pod regex contains its name, so we can match on it.
	collector := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			cur := inflight.Add(1)
			for {
				peak := peakInflight.Load()
				if cur <= peak || peakInflight.CompareAndSwap(peak, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inflight.Add(-1)

			// Fail queries for even-numbered workloads.
			for i := 0; i < 20; i += 2 {
				if strings.Contains(query, fmt.Sprintf("deploy-%d-", i)) {
					return nil, fmt.Errorf("prometheus timeout for %d", i)
				}
			}
			return grouped, nil
		},
	}

	const numWorkloads = 20
	objs := make([]runtime.Object, 0, numWorkloads)
	workloads := make([]client.Object, 0, numWorkloads)
	for i := range numWorkloads {
		name := fmt.Sprintf("deploy-%d", i)
		dep := newTestDeployment(name, "default", map[string]string{"app": name})
		objs = append(objs, dep)
		workloads = append(workloads, dep)
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })

	result := r.processWorkloads(context.Background(), policy, workloads, collector, nil, nil)

	// Odd-numbered workloads (10 of 20) should succeed.
	assert.Equal(t, int32(10), result.workloadsWithRecs,
		"only odd-numbered workloads should have recommendations")
	assert.Len(t, result.recommendations, 10)
	// Even-numbered workloads fail both CPU and memory queries.
	assert.Greater(t, result.totalQueryErrors, 0, "should have query errors")
	// Verify parallelism still occurred despite failures.
	assert.Greater(t, peakInflight.Load(), int32(1),
		"expected concurrent queries even with partial failures")
}

func TestProcessWorkloads_MixedStaleAndFresh(t *testing.T) {
	now := time.Now()
	priorData := metav1.NewTime(now.Add(-time.Minute))
	cpuRec, err := resource.ParseQuantity("250m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)

	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}

	collector := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "app-a-") {
				return nil, fmt.Errorf("connection refused")
			}
			return grouped, nil
		},
	}

	depA := newTestDeployment("app-a", "default", map[string]string{"app": "app-a"})
	depB := newTestDeployment("app-b", "default", map[string]string{"app": "app-b"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(depA, depB).
		Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}
	priorContainers := []attunev1alpha1.ContainerRecommendation{{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    cpuRec,
			MemoryRequest: memRec,
		},
	}}
	policy.Status.Recommendations = []attunev1alpha1.WorkloadRecommendation{
		{Workload: "app-a", Kind: "Deployment", LastDataTime: &priorData, Stale: false, Containers: priorContainers},
		{Workload: "app-b", Kind: "Deployment", LastDataTime: &priorData, Stale: false, Containers: priorContainers},
	}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })

	result := r.processWorkloads(context.Background(), policy, []client.Object{depA, depB}, collector, nil, nil)

	require.Len(t, result.recommendations, 2)
	assert.Greater(t, result.totalQueryErrors, 0, "app-a query errors must be counted")

	var recA, recB *attunev1alpha1.WorkloadRecommendation
	for i := range result.recommendations {
		switch result.recommendations[i].Workload {
		case "app-a":
			recA = &result.recommendations[i]
		case "app-b":
			recB = &result.recommendations[i]
		}
	}
	require.NotNil(t, recA, "app-a must reuse the prior rec as stale")
	require.NotNil(t, recB, "app-b must produce a fresh rec")
	assert.True(t, recA.Stale)
	require.NotNil(t, recA.LastDataTime)
	assert.True(t, recA.LastDataTime.Equal(&priorData), "stale reuse must preserve LastDataTime")
	assert.False(t, recB.Stale, "fresh Prometheus data must not mark app-b stale")
	assert.Equal(t, int32(1), result.workloadsWithRecs, "only the fresh rec counts toward Ready")
}
