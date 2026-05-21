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
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
)

func BenchmarkBuildPrometheusQuery_CPU(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("production", "api-server-7f8c9d", "web-container", "cpu", 5*time.Minute)
	}
}

func BenchmarkBuildPrometheusQuery_Memory(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("production", "api-server-7f8c9d", "web-container", "memory", 5*time.Minute)
	}
}

func BenchmarkBuildPrometheusQuery_SpecialChars(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("my-ns.test", "pod+name.v2[0]", "container(1)", "cpu", 5*time.Minute)
	}
}

func BenchmarkReconcile(b *testing.B) {
	policy := newTestPolicy("bench-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api"})
	pod := newTestPod("api-server-abc", "default", map[string]string{"app": "api"})

	samples := generateSamples(2016, 0.2)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{"main": samples}, nil
		},
		queryFunc: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			return 0.1, nil
		},
	}
	r, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "bench-policy", Namespace: "default"}}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = r.Reconcile(context.Background(), req)
	}
}

func BenchmarkComputeRecommendations(b *testing.B) {
	policy := newTestPolicy("bench-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	samples := generateSamples(2016, 0.2) // 7 days at 5-min step
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return samples, nil
		},
	}

	b.ResetTimer()
	for b.Loop() {
		_, _, _, _, _ = reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil)
	}
}
