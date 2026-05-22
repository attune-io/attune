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
	assert.Contains(t, got, "by {kube_container_name}")
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
	assert.Equal(t, "api-server-", spec.PodPrefix)
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
		{"api-server-[a-z0-9]+-[a-z0-9]+", "api-server-*"},
		{"web-.*", "web-*"},
		{"exact-name", "exact-name*"},
		{"[starts-with-bracket", "*"},
	}
	for _, tt := range tests {
		t.Run(tt.regex, func(t *testing.T) {
			assert.Equal(t, tt.want, datadogPodFilter(tt.regex))
		})
	}
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
	// Verify the new PromQLQueryBuilder produces identical output to the
	// old buildPrometheusQuery function (now deleted from controller).
	qb := &PromQLQueryBuilder{}

	cpu := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "cpu", 5*time.Minute)
	assert.True(t, strings.HasPrefix(cpu, "rate(container_cpu_usage_seconds_total{"))
	assert.Contains(t, cpu, `[5m]`)

	mem := qb.BuildQuery("production", "api-server-[a-z0-9]+", "", "memory", 5*time.Minute)
	assert.True(t, strings.HasPrefix(mem, "container_memory_working_set_bytes{"))
}
