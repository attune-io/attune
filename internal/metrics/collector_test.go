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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cannedRangeResponse returns a valid Prometheus API range query response
// with two data points.
func cannedRangeResponse() string {
	return `{
		"status": "success",
		"data": {
			"resultType": "matrix",
			"result": [
				{
					"metric": {"__name__": "cpu_usage", "pod": "test-pod"},
					"values": [
						[1700000000, "0.25"],
						[1700000060, "0.50"]
					]
				}
			]
		}
	}`
}

// cannedInstantResponse returns a valid Prometheus API instant query response
// with a single vector result.
func cannedInstantResponse() string {
	return `{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": [
				{
					"metric": {"__name__": "memory_usage", "pod": "test-pod"},
					"value": [1700000000, "1073741824"]
				}
			]
		}
	}`
}

// cannedEmptyResponse returns a valid Prometheus API response with no results.
func cannedEmptyResponse() string {
	return `{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": []
		}
	}`
}

// cannedErrorResponse returns a Prometheus API error response.
func cannedErrorResponse() string {
	return `{
		"status": "error",
		"errorType": "bad_data",
		"error": "invalid query"
	}`
}

func TestQueryRange_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedRangeResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	samples, err := collector.QueryRange(context.Background(), "cpu_usage", start, end, step)
	require.NoError(t, err)
	assert.Len(t, samples, 2)
	assert.InDelta(t, 0.25, samples[0].Value, 0.001)
	assert.InDelta(t, 0.50, samples[1].Value, 0.001)
	assert.False(t, samples[0].Timestamp.IsZero())
	assert.False(t, samples[1].Timestamp.IsZero())
}

func TestQuery_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	val, err := collector.Query(context.Background(), "memory_usage", time.Unix(1700000000, 0))
	require.NoError(t, err)
	assert.InDelta(t, 1073741824.0, val, 0.001)
}

func TestQuery_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedEmptyResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "missing_metric", time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty result")
}

func TestQueryRange_PrometheusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(cannedErrorResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	_, err = collector.QueryRange(context.Background(), "bad{query", start, end, step)
	assert.Error(t, err)
}

func TestQuery_ConnectionRefused(t *testing.T) {
	// Use a URL that will not be listening.
	collector, err := NewPrometheusCollector("http://127.0.0.1:19999", nil)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "cpu_usage", time.Now())
	assert.Error(t, err)
}

func TestQuery_ScalarResult(t *testing.T) {
	scalarResp := `{
		"status": "success",
		"data": {
			"resultType": "scalar",
			"result": [1700000000, "42.5"]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(scalarResp))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	val, err := collector.Query(context.Background(), "scalar_metric", time.Unix(1700000000, 0))
	require.NoError(t, err)
	assert.InDelta(t, 42.5, val, 0.001)
}

func TestQuery_PrometheusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(cannedErrorResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "bad{query", time.Now())
	assert.Error(t, err)
}

func TestNewPrometheusCollector_InvalidAddress(t *testing.T) {
	_, err := NewPrometheusCollector("://bad-url", nil)
	assert.Error(t, err)
}

func TestQueryRange_EmptyMatrix(t *testing.T) {
	emptyMatrix := `{
		"status": "success",
		"data": {
			"resultType": "matrix",
			"result": []
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(emptyMatrix))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, nil)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	samples, err := collector.QueryRange(context.Background(), "cpu_usage", start, end, step)
	require.NoError(t, err)
	assert.Empty(t, samples)
}
