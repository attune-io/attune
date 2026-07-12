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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
					Pointlist: [][2]float64{
						{1700000000000, 500000000},  // 500M nanocores = 0.5 cores
						{1700000300000, 1000000000}, // 1B nanocores = 1.0 cores
					},
				},
				{
					Metric: "kubernetes.cpu.usage.total",
					TagSet: []string{"kube_container_name:sidecar", "kube_namespace:default"},
					Pointlist: [][2]float64{
						{1700000000000, 100000000}, // 0.1 cores
					},
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
					Pointlist: [][2]float64{{1700000000000, 536870912}}, // 512 MiB
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
					Pointlist: [][2]float64{{1700000000000, 42.5}},
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
					Pointlist: [][2]float64{{1700000000000, 100}},
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
					Pointlist: [][2]float64{
						{1700000000000, 1.0}, // earlier
						{1700000300000, 5.0}, // latest
					},
				},
				{
					Metric: "custom.metric",
					TagSet: []string{"kube_container_name:b"},
					Pointlist: [][2]float64{
						{1700000100000, 3.0}, // middle
					},
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

func TestDatadogCollector_Close(t *testing.T) {
	c := &DatadogCollector{}
	require.NoError(t, c.Close(), "Close is a no-op and must succeed")
	require.NoError(t, c.Close(), "Close must be idempotent")
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

// Verify DatadogCollector implements MetricsCollector.
var _ MetricsCollector = &DatadogCollector{}
