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

package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromQLQueryBuilder_CPU(t *testing.T) {
	qb := &PromQLQueryBuilder{}
	got := qb.BuildQuery("production", "api-server-[a-z0-9]+-[a-z0-9]+", "", "cpu", 5*time.Minute)
	assert.Contains(t, got, `rate(container_cpu_usage_seconds_total`)
	assert.Contains(t, got, `namespace="production"`)
	assert.Contains(t, got, `pod=~"api-server-[a-z0-9]+-[a-z0-9]+"`)
	assert.Contains(t, got, `[5m]`)
}

func TestPromQLQueryBuilder_Memory(t *testing.T) {
	qb := &PromQLQueryBuilder{}
	got := qb.BuildQuery("default", "web-.*", "", "memory", 5*time.Minute)
	assert.Contains(t, got, `container_memory_working_set_bytes`)
	assert.Contains(t, got, `namespace="default"`)
	assert.NotContains(t, got, "rate(")
}

func TestPromQLQueryBuilder_WithContainer(t *testing.T) {
	qb := &PromQLQueryBuilder{}
	got := qb.BuildQuery("ns", "pod-.*", "main", "cpu", 5*time.Minute)
	assert.Contains(t, got, `container="main"`)
}

func TestPromQLQueryBuilder_UnknownMetric(t *testing.T) {
	qb := &PromQLQueryBuilder{}
	got := qb.BuildQuery("ns", "pod-.*", "", "disk", 5*time.Minute)
	assert.Equal(t, "", got)
}

func TestDatadogQueryBuilder_CPU(t *testing.T) {
	qb := &DatadogQueryBuilder{}
	got := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "cpu", 5*time.Minute)
	assert.Contains(t, got, "avg:kubernetes.cpu.usage.total")
	assert.Contains(t, got, "kube_namespace:production")
	assert.Contains(t, got, "pod_name:api-server-*")
	assert.Contains(t, got, "by {kube_container_name,pod_name}")
	assert.Contains(t, got, datadogRegexMarker+"api-server-[a-z0-9]+")
	assert.Contains(t, got, ".rollup(avg,300)")
}

func TestDatadogQueryBuilder_Memory(t *testing.T) {
	qb := &DatadogQueryBuilder{}
	got := qb.BuildQuery("default", "web-[a-z]+", "", "memory", 5*time.Minute)
	assert.Contains(t, got, "avg:kubernetes.memory.working_set")
	assert.Contains(t, got, "kube_namespace:default")
}

func TestDatadogQueryBuilder_WithContainer(t *testing.T) {
	qb := &DatadogQueryBuilder{}
	got := qb.BuildQuery("ns", "pod-.*", "sidecar", "cpu", 5*time.Minute)
	assert.Contains(t, got, "kube_container_name:sidecar")
}

func TestDatadogQueryBuilder_MinRollup(t *testing.T) {
	qb := &DatadogQueryBuilder{}
	got := qb.BuildQuery("ns", "pod-.*", "", "cpu", 10*time.Second)
	// Rollup should be clamped to 60 seconds minimum.
	assert.Contains(t, got, ".rollup(avg,60)")
}

func TestCloudWatchQueryBuilder_CPU(t *testing.T) {
	qb := &CloudWatchQueryBuilder{ClusterName: "my-cluster"}
	got := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "cpu", 5*time.Minute)

	var spec CloudWatchQuerySpec
	require.NoError(t, json.Unmarshal([]byte(got), &spec))
	assert.Equal(t, "container_cpu_usage_total", spec.Metric)
	assert.Equal(t, "my-cluster", spec.ClusterName)
	assert.Equal(t, "production", spec.Namespace)
	assert.Equal(t, "api-server-[a-z0-9]+", spec.PodRegex)
	assert.Empty(t, spec.PodPrefix)
	assert.Equal(t, 300, spec.Period)
	assert.Equal(t, "Average", spec.Stat)
}

func TestCloudWatchQueryBuilder_Memory(t *testing.T) {
	qb := &CloudWatchQueryBuilder{ClusterName: "prod"}
	got := qb.BuildQuery("default", "web-[a-z]+", "", "memory", 5*time.Minute)

	var spec CloudWatchQuerySpec
	require.NoError(t, json.Unmarshal([]byte(got), &spec))
	assert.Equal(t, "container_memory_working_set", spec.Metric)
}

func TestCloudWatchQueryBuilder_PeriodRounding(t *testing.T) {
	qb := &CloudWatchQueryBuilder{ClusterName: "c"}
	got := qb.BuildQuery("ns", "p-.*", "", "cpu", 90*time.Second)

	var spec CloudWatchQuerySpec
	require.NoError(t, json.Unmarshal([]byte(got), &spec))
	// 90s rounds up to 120s (multiple of 60).
	assert.Equal(t, 120, spec.Period)
}

func TestDatadogPodFilter(t *testing.T) {
	tests := []struct {
		regex string
		want  string
	}{
		{"api-server-[a-z0-9]+-[a-z0-9]+", "pod_name:api-server-*"},
		{"web-.*", "pod_name:web-*"},
		{"exact-name", "pod_name:exact-name"},
		{"[starts-with-bracket", "pod_name:*"},
		{"web-[a-z0-9]+-[a-z0-9]{5}", "pod_name:web-*"},
		{`my\.app-[a-z0-9]+`, "pod_name:my.app-*"},
		{`my\\.app-[a-z0-9]+-[a-z0-9]{5}`, "pod_name:my.app-*"},
		{"web-aaa|web-bbb", "(pod_name:web-aaa OR pod_name:web-bbb)"},
	}
	for _, tt := range tests {
		t.Run(tt.regex, func(t *testing.T) {
			assert.Equal(t, tt.want, datadogPodFilter(tt.regex))
		})
	}
}

func TestPodNameMatchesPromQLEscapedDot(t *testing.T) {
	re := `my\\.app-[a-z0-9]+-[a-z0-9]{5}`
	assert.True(t, podNameMatches(re, "my.app-abcde-fghij"))
	assert.False(t, podNameMatches(re, "my.app-api-abcde-fghij"))
	assert.False(t, podNameMatches(re, "myXapp-abcde-fghij"))
	assert.True(t, podNameMatches("web-a|web-b", "web-b"))
	assert.False(t, podNameMatches("web-a|web-b", "web-api"))
}

func TestCloudWatchPodPrefix(t *testing.T) {
	tests := []struct {
		regex string
		want  string
	}{
		{"api-server-[a-z0-9]+", "api-server-"},
		{"web-.*", "web-"},
		{"exact-name", "exact-name"},
		{"[bracket", ""},
	}
	for _, tt := range tests {
		t.Run(tt.regex, func(t *testing.T) {
			assert.Equal(t, tt.want, cloudWatchPodPrefix(tt.regex))
		})
	}
}

func TestFormatPromDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{1 * time.Hour, "1h"},
		{30 * time.Second, "30s"},
		{0, "5m"},
		{-1, "5m"},
		{90 * time.Second, "90s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatPromDuration(tt.d))
		})
	}
}

// Verify all three builders implement the interface at compile time.
var (
	_ QueryBuilder = &PromQLQueryBuilder{}
	_ QueryBuilder = &DatadogQueryBuilder{}
	_ QueryBuilder = &CloudWatchQueryBuilder{}
)

func TestPromQLQueryBuilder_BackwardCompatibility(t *testing.T) {
	// Default aggregation wraps raw metrics with max by (container).
	qb := &PromQLQueryBuilder{}

	cpu := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "cpu", 5*time.Minute)
	assert.True(t, strings.HasPrefix(cpu, "max by (container) (rate(container_cpu_usage_seconds_total{"))
	assert.Contains(t, cpu, `[5m]`)

	mem := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "memory", 5*time.Minute)
	assert.True(t, strings.HasPrefix(mem, "max by (container) (container_memory_working_set_bytes{"))

	// None restores the unaggregated shape used before scale work.
	qbNone := &PromQLQueryBuilder{Aggregation: PodAggregationNone}
	cpuNone := qbNone.BuildQuery("production", "api-server-[a-z0-9]+", "", "cpu", 5*time.Minute)
	assert.True(t, strings.HasPrefix(cpuNone, "rate(container_cpu_usage_seconds_total{"))
}

func TestPromQLQueryBuilder_RecordingMetrics(t *testing.T) {
	qb := &PromQLQueryBuilder{
		Aggregation:  PodAggregationMax,
		CPUMetric:    "attune:container_cpu:rate5m",
		MemoryMetric: "attune:container_memory:working_set",
	}
	cpu := qb.BuildQuery("ns", "pod-.*", "", "cpu", 5*time.Minute)
	assert.Contains(t, cpu, "attune:container_cpu:rate5m")
	assert.NotContains(t, cpu, "rate(container_cpu")
	mem := qb.BuildQuery("ns", "pod-.*", "", "memory", 5*time.Minute)
	assert.Contains(t, mem, "attune:container_memory:working_set")
}

func TestDownsampleSamples(t *testing.T) {
	samples := make([]Sample, 100)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range samples {
		samples[i] = Sample{Timestamp: base.Add(time.Duration(i) * time.Minute), Value: float64(i)}
	}
	out := DownsampleSamples(samples, 10)
	assert.Len(t, out, 10)
	// Each window keeps its midpoint. A rising series no longer reports the window max.
	assert.Equal(t, samples[5].Value, out[0].Value)
	assert.Equal(t, samples[15].Value, out[1].Value)
	// The last window holds the global max, so that one point stays the spike.
	assert.Equal(t, samples[99].Value, out[9].Value)
	// No-op when under cap
	assert.Equal(t, samples, DownsampleSamples(samples, 200))
}

func TestDownsampleSamples_KeepsSpike(t *testing.T) {
	samples := make([]Sample, 20)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range samples {
		samples[i] = Sample{Timestamp: base.Add(time.Duration(i) * time.Minute), Value: 1}
	}
	// Index 7 sits between even strides of 4 output points (0, 6, 13, 19).
	samples[7].Value = 1000
	out := DownsampleSamples(samples, 4)
	max := 0.0
	for _, s := range out {
		if s.Value > max {
			max = s.Value
		}
	}
	assert.Equal(t, 1000.0, max, "a short spike must survive downsampling")
}

func TestDownsampleSamples_PercentileStaysNearRaw(t *testing.T) {
	samples := make([]Sample, 30000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range samples {
		samples[i] = Sample{Timestamp: base.Add(time.Duration(i) * time.Second), Value: float64(i)}
	}
	raw := BuildProfile(samples)
	down := BuildProfile(DownsampleSamples(samples, DefaultMaxProfileSamples))
	// Window maxima would lift P95 from ~0.95 to ~0.98 of the span.
	assert.InDelta(t, raw.OverallPercentiles.P95, down.OverallPercentiles.P95, raw.OverallPercentiles.P95*0.02)
	assert.Equal(t, raw.OverallPercentiles.Max, down.OverallPercentiles.Max)
}

func TestCloudWatchPodNameMatchesController(t *testing.T) {
	tests := []struct {
		name    string
		regex   string
		pod     string
		matches bool
	}{
		{"deployment full pod", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-7d8f9c6b5-xk2pq", true},
		{"deployment replicaset", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-7d8f9c6b5", true},
		{"deployment short name", `api-[a-z0-9]+-[a-z0-9]{5}`, "api", false},
		{"deployment sibling word", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-v2", false},
		{"deployment sibling worker", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-worker", false},
		{"deployment cron stamp", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-1700000000", false},
		{"deployment sibling replicaset", `api-[a-z0-9]+-[a-z0-9]{5}`, "api-v2-7d8f9c6b", false},
		{"daemonset controller", `web-[a-z0-9]{5}`, "web", true},
		{"daemonset full pod", `web-[a-z0-9]{5}`, "web-fghij", true},
		{"daemonset sibling", `web-[a-z0-9]{5}`, "web-api", false},
		{"statefulset controller", `db-[0-9]+`, "db", true},
		{"statefulset pod", `db-[0-9]+`, "db-0", true},
		{"job controller", `migrate-[a-z0-9]{5}`, "migrate", true},
		{"indexed job controller", `batch-[0-9]+-[a-z0-9]{5}`, "batch", true},
		{"indexed job pod", `batch-[0-9]+-[a-z0-9]{5}`, "batch-3-fghij", true},
		{"cronjob job name", `nightly-[0-9]{10}-[a-z0-9]{5}`, "nightly-1700000000", true},
		{"cronjob full pod", `nightly-[0-9]{10}-[a-z0-9]{5}`, "nightly-1700000000-fghij", true},
		{"indexed cron job name", `nightly-[0-9]{10}-[0-9]+-[a-z0-9]{5}`, "nightly-1700000000", true},
		{"indexed cron index", `nightly-[0-9]{10}-[0-9]+-[a-z0-9]{5}`, "nightly-1700000000-3", false},
		{"cronjob workload only", `nightly-[0-9]{10}-[a-z0-9]{5}`, "nightly", false},
		{"sampled deployment pod", `api-7d8f9c6b5-xk2pq`, "api-7d8f9c6b5", true},
		{"sampled numbered job", `migrate-2-fghij`, "migrate-2", true},
		{"sampled numbered job parent", `migrate-2-fghij`, "migrate", false},
		{"sampled statefulset pod", `db-0`, "db", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.matches, cloudWatchPodNameMatches(tt.regex, tt.pod))
		})
	}
}

func TestApplyPodAggregation(t *testing.T) {
	inner := `rate(container_cpu_usage_seconds_total{namespace="ns"}[5m])`
	assert.Equal(t, inner, applyPodAggregation(inner, PodAggregationNone))
	assert.Equal(t, `avg by (container) (`+inner+`)`, applyPodAggregation(inner, PodAggregationAvg))
	assert.Equal(t, `max by (container) (`+inner+`)`, applyPodAggregation(inner, PodAggregationMax))
	assert.Equal(t, `max by (container) (`+inner+`)`, applyPodAggregation(inner, ""))
	assert.Equal(t, `max by (container) (`+inner+`)`, applyPodAggregation(inner, PodAggregationMode("Weird")))
}
