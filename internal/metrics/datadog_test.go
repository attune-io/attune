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
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/attune-io/attune/internal/operatormetrics"
)

func ddPoints(pairs ...[2]float64) [][2]*float64 {
	out := make([][2]*float64, len(pairs))
	for i, p := range pairs {
		ts, v := p[0], p[1]
		out[i] = [2]*float64{&ts, &v}
	}
	return out
}

func TestDatadogCollector_QueryRangeGrouped_NaNInfFiltered(t *testing.T) {
	// encoding/json cannot transport NaN/Inf; QueryRangeGrouped uses this helper after unmarshal.
	ts1 := 1700000000000.0
	points := ddPoints(
		[2]float64{ts1, 250000000},          // 0.25 cores after nanocore conversion
		[2]float64{ts1 + 60000, math.NaN()}, // dropped
		[2]float64{ts1 + 120000, math.Inf(1)},
		[2]float64{ts1 + 180000, math.Inf(-1)},
		[2]float64{ts1 + 240000, 750000000}, // 0.75 cores
	)

	ctx := WithNanInfLabels(context.Background(), "dd-ns", "dd-policy", "cpu")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	grouped := map[string][]Sample{
		"main": appendDatadogSamples(ctx, nil, points, true),
	}
	require.Len(t, grouped["main"], 2, "NaN, +Inf, and -Inf samples should be filtered out")
	assert.InDelta(t, 0.25, grouped["main"][0].Value, 0.001)
	assert.InDelta(t, 0.75, grouped["main"][1].Value, 0.001)
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	assert.Equal(t, before, after, "mixed finite+NaN series must not increment the unusable-series counter")
}

func TestDatadogCollector_QueryRangeGrouped_AllNonFiniteIncrementsOnce(t *testing.T) {
	ts1 := 1700000000000.0
	points := ddPoints(
		[2]float64{ts1, math.NaN()},
		[2]float64{ts1 + 60000, math.Inf(1)},
		[2]float64{ts1 + 120000, math.Inf(-1)},
	)

	ctx := WithNanInfLabels(context.Background(), "dd-ns", "dd-policy", "cpu")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	got := appendDatadogSamples(ctx, nil, points, true)
	assert.Empty(t, got, "all-non-finite series keeps no samples")
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	assert.Equal(t, before+1, after, "all-non-finite series increments the counter once")
}

func TestDatadogCollector_PreservePodName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric: "kubernetes.cpu.usage.total",
					TagSet: []string{"kube_container_name:web", "pod_name:steady"},
					Pointlist: ddPoints(
						[2]float64{1700000000000, 0},
						[2]float64{1700000300000, 500000000},
					),
				},
				{
					Metric:    "kubernetes.cpu.usage.total",
					TagSet:    []string{"kube_container_name:web", "pod_name:young"},
					Pointlist: ddPoints([2]float64{1700000000000, 1000000000}),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	query := `avg:kubernetes.cpu.usage.total{kube_namespace:default} by {kube_container_name,pod_name}`
	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000600, 0)

	plain, err := c.QueryRangeGrouped(context.Background(), query, start, end, time.Minute)
	require.NoError(t, err)
	require.Len(t, plain["web"], 3)
	for _, sample := range plain["web"] {
		assert.Empty(t, sample.Pod)
	}
	assert.InDelta(t, 0, plain["web"][0].Value, 0.001, "numeric 0 stays")

	grouped, err := c.QueryRangeGrouped(WithPreservePodSeries(context.Background()), query, start, end, time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["web"], 3)
	pods := map[string]int{}
	for _, sample := range grouped["web"] {
		pods[sample.Pod]++
	}
	assert.Equal(t, 2, pods["steady"])
	assert.Equal(t, 1, pods["young"])
}

func TestDatadogCollector_QueryRangeGrouped(t *testing.T) {
	// Simulate Datadog /api/v1/query response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-api-key", r.Header.Get("DD-API-KEY"))
		assert.Equal(t, "test-app-key", r.Header.Get("DD-APPLICATION-KEY"))
		assert.Contains(t, r.URL.Path, "/api/v1/query")

		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric: "kubernetes.cpu.usage.total",
					TagSet: []string{"kube_container_name:web", "kube_namespace:default"},
					Pointlist: ddPoints(
						[2]float64{1700000000000, 500000000},  // 500M nanocores = 0.5 cores
						[2]float64{1700000300000, 1000000000}, // 1B nanocores = 1.0 cores
					),
				},
				{
					Metric:    "kubernetes.cpu.usage.total",
					TagSet:    []string{"kube_container_name:sidecar", "kube_namespace:default"},
					Pointlist: ddPoints([2]float64{1700000000000, 100000000}), // 0.1 cores
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "test-api-key",
		appKey:        "test-app-key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000600, 0)
	query := `avg:kubernetes.cpu.usage.total{kube_namespace:default} by {kube_container_name}`

	grouped, err := c.QueryRangeGrouped(context.Background(), query, start, end, 5*time.Minute)
	require.NoError(t, err)

	// Verify grouping by container.
	assert.Len(t, grouped, 2)
	assert.Len(t, grouped["web"], 2)
	assert.Len(t, grouped["sidecar"], 1)

	// Verify nanocores -> cores conversion.
	assert.InDelta(t, 0.5, grouped["web"][0].Value, 0.001)
	assert.InDelta(t, 1.0, grouped["web"][1].Value, 0.001)
	assert.InDelta(t, 0.1, grouped["sidecar"][0].Value, 0.001)
}

func TestDatadogCollector_MemoryNoConversion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric:    "kubernetes.memory.working_set",
					TagSet:    []string{"kube_container_name:web"},
					Pointlist: ddPoints([2]float64{1700000000000, 536870912}), // 512 MiB
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	query := `avg:kubernetes.memory.working_set{kube_namespace:default}`
	grouped, err := c.QueryRangeGrouped(context.Background(), query, time.Now().Add(-time.Hour), time.Now(), 5*time.Minute)
	require.NoError(t, err)

	// Memory should NOT be converted (no nanocores conversion).
	assert.InDelta(t, 536870912, grouped["web"][0].Value, 1)
}

func TestDatadogCollector_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors": ["Forbidden"]}`))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "bad-key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	_, err := c.QueryRangeGrouped(context.Background(), "any", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestDatadogCollector_QueryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{
			Status: "error",
			Error:  "invalid query syntax",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	_, err := c.QueryRangeGrouped(context.Background(), "bad", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid query syntax")
}

func TestDatadogCollector_Query_Instant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric:    "custom.metric",
					TagSet:    []string{},
					Pointlist: ddPoints([2]float64{1700000000000, 42.5}),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	val, err := c.Query(context.Background(), "custom.metric{*}", time.Unix(1700000000, 0))
	require.NoError(t, err)
	assert.InDelta(t, 42.5, val, 0.001)
}

func TestDatadogCollector_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{Status: "ok", Series: nil}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	grouped, err := c.QueryRangeGrouped(context.Background(), "query", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.NoError(t, err)
	assert.Empty(t, grouped)
}

func TestExtractDatadogTag(t *testing.T) {
	tags := []string{"kube_container_name:web", "kube_namespace:default", "pod_name:api-abc"}
	assert.Equal(t, "web", extractDatadogTag(tags, "kube_container_name"))
	assert.Equal(t, "default", extractDatadogTag(tags, "kube_namespace"))
	assert.Equal(t, "", extractDatadogTag(tags, "missing_tag"))
}

func TestDatadogCollector_EmptyTagSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric:    "kubernetes.memory.working_set",
					TagSet:    []string{}, // no kube_container_name tag
					Pointlist: ddPoints([2]float64{1700000000000, 100}),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	grouped, err := c.QueryRangeGrouped(context.Background(), "memory", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.NoError(t, err)
	// Samples with no container tag should be grouped under "".
	assert.Len(t, grouped[""], 1)
	assert.InDelta(t, 100, grouped[""][0].Value, 0.001)
}

func TestDatadogCollector_Query_ReturnsLatestTimestamp(t *testing.T) {
	// Regression test: Query must return the latest sample by timestamp,
	// not by iteration order (which is non-deterministic from map flattening).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := datadogSeriesResponse{
			Status: "ok",
			Series: []datadogSeries{
				{
					Metric: "custom.metric",
					TagSet: []string{"kube_container_name:a"},
					Pointlist: ddPoints(
						[2]float64{1700000000000, 1.0}, // earlier
						[2]float64{1700000300000, 5.0}, // latest
					),
				},
				{
					Metric:    "custom.metric",
					TagSet:    []string{"kube_container_name:b"},
					Pointlist: ddPoints([2]float64{1700000100000, 3.0}), // middle
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}

	val, err := c.Query(context.Background(), "custom.metric{*}", time.Unix(1700000300, 0))
	require.NoError(t, err)
	assert.InDelta(t, 5.0, val, 0.001, "should return the sample with the latest timestamp")
}

func TestDatadogCollector_NullPointIsZeroNumericZeroKept(t *testing.T) {
	const body = `{
		"status": "ok",
		"series": [{
			"metric": "kubernetes.cpu.usage.total",
			"tag_set": ["kube_container_name:web"],
			"pointlist": [[1700000000000, 500000000], [1700000060000, null], [1700000120000, 0]]
		}]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	ctx := WithNanInfLabels(context.Background(), "dd-ns", "dd-policy", "cpu")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))

	grouped, err := c.QueryRangeGrouped(ctx, "avg:kubernetes.cpu.usage.total{*}", time.Unix(1700000000, 0), time.Unix(1700000200, 0), time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["web"], 3)
	assert.InDelta(t, 0.5, grouped["web"][0].Value, 0.001)
	assert.InDelta(t, 0, grouped["web"][1].Value, 0.001)
	assert.InDelta(t, 0, grouped["web"][2].Value, 0.001)

	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	assert.Equal(t, before, after, "a series that still has a usable point does not increment")
}

func TestDatadogCollector_MemoryNullPointIsZero(t *testing.T) {
	const body = `{
		"status": "ok",
		"series": [{
			"metric": "kubernetes.memory.working_set",
			"tag_set": ["kube_container_name:web"],
			"pointlist": [[1700000000000, null], [1700000060000, 1048576]]
		}]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	grouped, err := c.QueryRangeGrouped(context.Background(), "avg:kubernetes.memory.working_set{*}", time.Unix(1700000000, 0), time.Unix(1700000200, 0), time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["web"], 2)
	assert.InDelta(t, 0, grouped["web"][0].Value, 1)
	assert.InDelta(t, 1048576, grouped["web"][1].Value, 1)
}

func TestDatadogCollector_AllNullPointsStayZero(t *testing.T) {
	const body = `{
		"status": "ok",
		"series": [{
			"metric": "kubernetes.cpu.usage.total",
			"tag_set": ["kube_container_name:web"],
			"pointlist": [[1700000000000, null], [1700000060000, null]]
		}]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	ctx := WithNanInfLabels(context.Background(), "dd-ns", "dd-policy", "cpu")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	grouped, err := c.QueryRangeGrouped(ctx, "avg:kubernetes.cpu.usage.total{*}", time.Unix(1700000000, 0), time.Unix(1700000200, 0), time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["web"], 2)
	assert.InDelta(t, 0, grouped["web"][0].Value, 0.001)
	assert.InDelta(t, 0, grouped["web"][1].Value, 0.001)
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	assert.Equal(t, before, after)
}

func TestDatadogCollector_NullTimestampKeepsValue(t *testing.T) {
	const body = `{
		"status": "ok",
		"series": [{
			"metric": "kubernetes.cpu.usage.total",
			"tag_set": ["kube_container_name:web"],
			"pointlist": [[null, 500000000], [1700000060000]]
		}]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	ctx := WithNanInfLabels(context.Background(), "dd-ns", "dd-policy", "cpu")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))

	grouped, err := c.QueryRangeGrouped(ctx, "avg:kubernetes.cpu.usage.total{*}", time.Unix(1700000000, 0), time.Unix(1700000200, 0), time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["web"], 2)
	assert.True(t, grouped["web"][0].Timestamp.Equal(time.Unix(0, 0)))
	assert.InDelta(t, 0.5, grouped["web"][0].Value, 0.001)
	assert.InDelta(t, 0, grouped["web"][1].Value, 0.001)

	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("dd-ns", "dd-policy", "untracked", "cpu"))
	assert.Equal(t, before, after, "a kept value must not increment the counter")
}

func TestDatadogCollector_NonNumericPointFailsQuery(t *testing.T) {
	const body = `{
		"status": "ok",
		"series": [{
			"metric": "kubernetes.cpu.usage.total",
			"tag_set": ["kube_container_name:web"],
			"pointlist": [[1700000000000, "NaN"]]
		}]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := &DatadogCollector{
		httpClient:    server.Client(),
		baseURL:       server.URL,
		apiKey:        "key",
		logger:        logr.Discard(),
		cpuMetricName: "kubernetes.cpu.usage.total",
	}
	_, err := c.QueryRangeGrouped(context.Background(), "avg:kubernetes.cpu.usage.total{*}", time.Unix(1700000000, 0), time.Unix(1700000200, 0), time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing Datadog response")
}

func TestDatadogCollector_Close(t *testing.T) {
	c := &DatadogCollector{}
	require.NoError(t, c.Close(), "nil HTTP client must succeed")
	require.NoError(t, c.Close(), "Close must be idempotent")

	transport := &http.Transport{}
	c = &DatadogCollector{httpClient: &http.Client{Transport: transport}}
	require.NoError(t, c.Close())
	require.NoError(t, c.Close(), "CloseIdleConnections must be idempotent")
}

func TestLatestSampleValue(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		want    float64
		wantErr string
	}{
		{
			name:    "empty returns error",
			samples: nil,
			wantErr: "empty result from TestBackend instant query",
		},
		{
			name:    "single sample",
			samples: []Sample{{Timestamp: time.Unix(1000, 0), Value: 42.0}},
			want:    42.0,
		},
		{
			name: "returns latest by timestamp",
			samples: []Sample{
				{Timestamp: time.Unix(1000, 0), Value: 1.0},
				{Timestamp: time.Unix(3000, 0), Value: 3.0},
				{Timestamp: time.Unix(2000, 0), Value: 2.0},
			},
			want: 3.0,
		},
		{
			name:    "NaN value returns error",
			samples: []Sample{{Timestamp: time.Unix(1000, 0), Value: math.NaN()}},
			wantErr: "non-finite value from TestBackend instant query",
		},
		{
			name:    "Inf value returns error",
			samples: []Sample{{Timestamp: time.Unix(1000, 0), Value: math.Inf(1)}},
			wantErr: "non-finite value from TestBackend instant query",
		},
		{
			name:    "negative Inf value returns error",
			samples: []Sample{{Timestamp: time.Unix(1000, 0), Value: math.Inf(-1)}},
			wantErr: "non-finite value from TestBackend instant query",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := latestSampleValue(tt.samples, "TestBackend")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.InDelta(t, tt.want, val, 0.001)
			}
		})
	}
}

func TestNewDatadogCollector_Defaults(t *testing.T) {
	c, err := NewDatadogCollector("", "api", "app", logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "https://api.datadoghq.com", c.baseURL)
	assert.Equal(t, "api", c.apiKey)
	assert.Equal(t, "app", c.appKey)
	assert.Equal(t, "kubernetes.cpu.usage.total", c.cpuMetricName)
	require.NotNil(t, c.httpClient)
	assert.Equal(t, 30*time.Second, c.httpClient.Timeout)
	require.NotNil(t, c.httpClient.CheckRedirect)
}

func TestNewDatadogCollector_CustomSite(t *testing.T) {
	c, err := NewDatadogCollector("datadoghq.eu", "k", "a", logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "https://api.datadoghq.eu", c.baseURL)
}

func TestNewDatadogCollector_RejectsInvalidSite(t *testing.T) {
	c, err := NewDatadogCollector("evil.example", "k", "a", logr.Discard())
	require.Error(t, err)
	assert.Nil(t, c)
	assert.Contains(t, err.Error(), "not a recognized Datadog site")

	c, err = NewDatadogCollector("datadoghq.com.evil.example", "k", "a", logr.Discard())
	require.Error(t, err)
	assert.Nil(t, c)
}

func TestNewDatadogCollector_DoesNotFollowRedirects(t *testing.T) {
	var sawEvil atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawEvil.Store(true)
		assert.Empty(t, r.Header.Get("DD-API-KEY"), "API key must not be sent to the redirect target")
	}))
	defer evil.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer origin.Close()

	c, err := NewDatadogCollector("datadoghq.com", "secret-api-key", "app", logr.Discard())
	require.NoError(t, err)
	c.baseURL = origin.URL

	_, err = c.QueryRangeGrouped(context.Background(), "avg:system.cpu.user{*}", time.Unix(1700000000, 0), time.Unix(1700000600, 0), time.Minute)
	require.Error(t, err)
	assert.ErrorIs(t, err, errDatadogRedirect)
	assert.False(t, sawEvil.Load(), "must not follow redirect to attacker host")
}

// Verify DatadogCollector implements MetricsCollector.
var _ MetricsCollector = &DatadogCollector{}
