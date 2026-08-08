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
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/attune-io/attune/internal/throttle"
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

// cannedMultiVectorResponse returns a valid Prometheus API instant query
// response with multiple vector results.
func cannedMultiVectorResponse() string {
	return `{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": [
				{
					"metric": {"__name__": "memory_usage", "pod": "test-pod-a"},
					"value": [1700000000, "1073741824"]
				},
				{
					"metric": {"__name__": "memory_usage", "pod": "test-pod-b"},
					"value": [1700000000, "2147483648"]
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

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
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

func TestQueryRangeGrouped_Success(t *testing.T) {
	response := `{
		"status": "success",
		"data": {
			"resultType": "matrix",
			"result": [
				{
					"metric": {"__name__": "cpu_usage", "pod": "test-pod", "container": "app"},
					"values": [
						[1700000000, "0.25"],
						[1700000060, "0.50"]
					]
				},
				{
					"metric": {"__name__": "cpu_usage", "pod": "test-pod", "container": "sidecar"},
					"values": [
						[1700000000, "0.05"]
					]
				}
			]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	grouped, err := collector.QueryRangeGrouped(context.Background(), "cpu_usage", start, end, step)
	require.NoError(t, err)
	require.Len(t, grouped, 2)
	require.Len(t, grouped["app"], 2)
	require.Len(t, grouped["sidecar"], 1)
	assert.InDelta(t, 0.25, grouped["app"][0].Value, 0.001)
	assert.InDelta(t, 0.50, grouped["app"][1].Value, 0.001)
	assert.InDelta(t, 0.05, grouped["sidecar"][0].Value, 0.001)
}

func TestQueryRangeGrouped_NaNInfFiltered(t *testing.T) {
	response := `{
		"status": "success",
		"data": {
			"resultType": "matrix",
			"result": [
				{
					"metric": {"__name__": "cpu_usage", "container": "app"},
					"values": [
						[1700000000, "0.25"],
						[1700000060, "NaN"],
						[1700000120, "Inf"],
						[1700000180, "-Inf"],
						[1700000240, "0.75"]
					]
				}
			]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000300, 0)
	step := 60 * time.Second

	grouped, err := collector.QueryRangeGrouped(context.Background(), "cpu_usage", start, end, step)
	require.NoError(t, err)
	require.Len(t, grouped["app"], 2, "NaN, +Inf, and -Inf samples should be filtered out")
	assert.InDelta(t, 0.25, grouped["app"][0].Value, 0.001)
	assert.InDelta(t, 0.75, grouped["app"][1].Value, 0.001)
}

func TestQuery_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
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

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "missing_metric", time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty result")
}

func TestQuery_MultipleVectorSamples(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedMultiVectorResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "memory_usage", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected exactly one sample")
}

func TestQueryRange_PrometheusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(cannedErrorResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	_, err = collector.QueryRange(context.Background(), "bad{query", start, end, step)
	assert.Error(t, err)
}

func TestQuery_ConnectionRefused(t *testing.T) {
	// Use a URL that will not be listening.
	collector, err := NewPrometheusCollector("http://127.0.0.1:19999", logr.Discard())
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

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
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

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "bad{query", time.Now())
	assert.Error(t, err)
}

func TestNewPrometheusCollector_InvalidAddress(t *testing.T) {
	_, err := NewPrometheusCollector("://bad-url", logr.Discard())
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

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000120, 0)
	step := 60 * time.Second

	samples, err := collector.QueryRange(context.Background(), "cpu_usage", start, end, step)
	require.NoError(t, err)
	assert.Empty(t, samples)
}

func TestEscapePromQL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{`with"quote`, `with\"quote`},
		{`with\backslash`, `with\\backslash`},
		{`both\"chars`, `both\\\"chars`},
		{"with\nnewline", `with\nnewline`},
		{"with\rcarriage", `with\rcarriage`},
		{"with\ttab", `with\ttab`},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := EscapePromQL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetThrottleRatio_EscapesInput(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("failed to parse form: %v", err)
		}
		receivedQuery = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	// Use names with special characters that need escaping.
	_, err = collector.GetThrottleRatio(context.Background(), `ns"with"quotes`, `pod\with\backslash`, `container"both`, time.Now())
	require.NoError(t, err)

	// Verify the query has escaped values.
	assert.Contains(t, receivedQuery, `ns\"with\"quotes`)
	assert.Contains(t, receivedQuery, `pod\\with\\backslash`)
	assert.Contains(t, receivedQuery, `container\"both`)
}

func TestGetThrottleRatio_EmptyResultReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedEmptyResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	ratio, err := collector.GetThrottleRatio(context.Background(), "default", "pod-1", "app", time.Now())
	require.NoError(t, err)
	assert.Zero(t, ratio)
}

func TestGetThrottleRatio_NaNReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Prometheus returns NaN for 0/0 (both rates zero).
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"NaN"]}]}}`))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	ratio, err := collector.GetThrottleRatio(context.Background(), "default", "pod-1", "app", time.Now())
	require.NoError(t, err)
	assert.Zero(t, ratio)
}

func TestGetThrottleRatio_InfReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Prometheus can return +Inf for extreme rate ratios.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"+Inf"]}]}}`))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	ratio, err := collector.GetThrottleRatio(context.Background(), "default", "pod-1", "app", time.Now())
	require.NoError(t, err)
	assert.Zero(t, ratio)
}

func TestEscapePromQLRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{`with.dot`, `with\\.dot`},
		{`a+b*c?d`, `a\\+b\\*c\\?d`},
		{`(group)[class]{brace}`, `\\(group\\)\\[class\\]\\{brace\\}`},
		{`pipe|or`, `pipe\\|or`},
		{`^start$end`, `\\^start\\$end`},
		{`quote"and\backslash`, `quote\"and\\\\backslash`},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, EscapePromQLRegex(tt.input))
		})
	}
}

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"link-local v4", "169.254.169.254", true},
		{"link-local v6", "fe80::1", true},
		{"unspecified", "0.0.0.0", true},
		{"private 10.x", "10.0.0.1", false},
		{"private 172.x", "172.16.0.1", false},
		{"public IP", "8.8.8.8", false},
		{"cluster IP", "10.96.0.1", false},
		{"AWS IMDSv2 IPv6", "fd00:ec2::254", true},
		{"other ULA", "fd00::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			assert.Equal(t, tt.blocked, isBlockedIP(ip))
		})
	}
}

func TestHeaderTransport_InjectsHeadersAndBearer(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	opts := &CollectorOptions{
		Headers:     map[string]string{"X-Scope-OrgID": "tenant-1", "X-Custom": "value"},
		BearerToken: "my-secret-token",
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)

	assert.Equal(t, "tenant-1", gotHeaders.Get("X-Scope-OrgID"))
	assert.Equal(t, "value", gotHeaders.Get("X-Custom"))
	assert.Equal(t, "Bearer my-secret-token", gotHeaders.Get("Authorization"))
}

func TestHeaderTransport_NoBearer(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	opts := &CollectorOptions{
		Headers: map[string]string{"X-Scope-OrgID": "tenant-2"},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)

	assert.Equal(t, "tenant-2", gotHeaders.Get("X-Scope-OrgID"))
	assert.Empty(t, gotHeaders.Get("Authorization"))
}

func TestHeaderTransport_SkipsHeadersOnCrossOriginRedirect(t *testing.T) {
	var redirectedHeaders http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/query?query=up", http.StatusFound)
	}))
	defer source.Close()

	opts := &CollectorOptions{
		Headers:     map[string]string{"X-Scope-OrgID": "tenant-1"},
		BearerToken: "my-secret-token",
	}
	collector, err := NewPrometheusCollectorWithOptions(source.URL, logr.Discard(), opts, http.DefaultTransport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	require.NotNil(t, redirectedHeaders)
	assert.Empty(t, redirectedHeaders.Get("X-Scope-OrgID"))
	assert.Empty(t, redirectedHeaders.Get("Authorization"))
}

func TestHeaderTransport_PreservesHeadersOnSameOriginRedirect(t *testing.T) {
	var finalHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/api/v1/query?query=up", http.StatusFound)
			return
		}
		finalHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	opts := &CollectorOptions{
		Headers:     map[string]string{"X-Scope-OrgID": "tenant-1"},
		BearerToken: "my-secret-token",
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL+"/redirect", logr.Discard(), opts, http.DefaultTransport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	require.NotNil(t, finalHeaders)
	assert.Equal(t, "tenant-1", finalHeaders.Get("X-Scope-OrgID"))
	assert.Equal(t, "Bearer my-secret-token", finalHeaders.Get("Authorization"))
}

func TestQueryParamTransport_AppendsParams(t *testing.T) {
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rt := &queryParamTransport{
		base:   http.DefaultTransport,
		params: map[string]string{"dedup": "true", "partial_response": "true"},
	}

	req, _ := http.NewRequest("GET", server.URL+"/api/v1/query?query=up", nil)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Contains(t, gotURL, "dedup=true")
	assert.Contains(t, gotURL, "partial_response=true")
	assert.Contains(t, gotURL, "query=up", "original query param should be preserved")
}

func TestQueryParamTransport_DoesNotOverrideExistingParams(t *testing.T) {
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rt := &queryParamTransport{
		base:   http.DefaultTransport,
		params: map[string]string{"query": "rate(up[5m])", "step": "30s", "dedup": "true"},
	}

	req, _ := http.NewRequest("GET", server.URL+"/api/v1/query_range?query=up&step=60s", nil)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Contains(t, gotURL, "query=up")
	assert.Contains(t, gotURL, "step=60s")
	assert.NotContains(t, gotURL, "query=rate%28up%5B5m%5D%29")
	assert.NotContains(t, gotURL, "step=30s")
	assert.Contains(t, gotURL, "dedup=true")
}

func TestNewPrometheusCollectorWithOptions_InsecureSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	// Without InsecureSkipVerify, TLS verification would fail against the
	// self-signed cert from httptest.NewTLSServer. We pass the test server's
	// transport to bypass SSRF checks (localhost), but the InsecureSkipVerify
	// flag is exercised in the option-parsing branch.
	opts := &CollectorOptions{InsecureSkipVerify: true}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
}

func TestNewPrometheusCollectorWithOptions_NilOpts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), nil, server.Client().Transport)
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
}

func TestSSRFSafeTransport_HasProxyFromEnvironment(t *testing.T) {
	rt := ssrfSafeTransport()
	tr, ok := rt.(*http.Transport)
	require.True(t, ok, "ssrfSafeTransport must return *http.Transport")
	assert.NotNil(t, tr.Proxy, "transport must have Proxy set for proxy-aware support")
}

func TestNewPrometheusCollectorWithOptions_AppliesTLSMinVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	opts := &CollectorOptions{TLSMinVersion: tls.VersionTLS13}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts)
	require.NoError(t, err)

	tr := collector.transport
	require.NotNil(t, tr)
	require.NotNil(t, tr.TLSClientConfig, "TLS config must be set when TLSMinVersion is specified")
	assert.Equal(t, uint16(tls.VersionTLS13), tr.TLSClientConfig.MinVersion)
}

func TestNewPrometheusCollectorWithOptions_DefaultTLSMinVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), nil)
	require.NoError(t, err)

	tr := collector.transport
	require.NotNil(t, tr)
	// With no options, TLS config should not be set (Go defaults apply).
	assert.Nil(t, tr.TLSClientConfig)
}

func TestSSRFSafeTransport_BlocksLoopback(t *testing.T) {
	// A server on localhost should be blocked by the SSRF-safe transport.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard())
	require.NoError(t, err)

	_, err = collector.Query(context.Background(), "up", time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
}

func TestClose_NilTransport(t *testing.T) {
	c := &PrometheusCollector{logger: logr.Discard()}
	assert.NoError(t, c.Close())
}

func TestClose_WithTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	// Make a query to establish an idle connection.
	_, _ = collector.Query(context.Background(), "up", time.Now())

	assert.NoError(t, collector.Close())
}

func TestSameOrigin_BothNil(t *testing.T) {
	assert.False(t, sameOrigin(nil, nil))
}

func TestSameOrigin_OneNil(t *testing.T) {
	u, _ := url.Parse("http://example.com")
	assert.False(t, sameOrigin(u, nil))
	assert.False(t, sameOrigin(nil, u))
}

func TestSameOrigin_SameSchemeAndHost(t *testing.T) {
	a, _ := url.Parse("http://example.com/path1")
	b, _ := url.Parse("http://example.com/path2")
	assert.True(t, sameOrigin(a, b))
}

func TestSameOrigin_DifferentScheme(t *testing.T) {
	a, _ := url.Parse("http://example.com")
	b, _ := url.Parse("https://example.com")
	assert.False(t, sameOrigin(a, b))
}

func TestSameOrigin_DifferentHost(t *testing.T) {
	a, _ := url.Parse("http://a.example.com")
	b, _ := url.Parse("http://b.example.com")
	assert.False(t, sameOrigin(a, b))
}

func TestQueryRangeGrouped_SeriesCapByContainer(t *testing.T) {
	// Build a fake matrix via a small custom collector is hard without API;
	// unit-test capMatrixByContainer directly.
	m1 := &model.SampleStream{Metric: model.Metric{"container": "a", "pod": "p1"}}
	m2 := &model.SampleStream{Metric: model.Metric{"container": "a", "pod": "p2"}}
	m3 := &model.SampleStream{Metric: model.Metric{"container": "b", "pod": "p1"}}
	m4 := &model.SampleStream{Metric: model.Metric{"container": "c", "pod": "p1"}}
	matrix := model.Matrix{m1, m2, m3, m4}

	// Limit 2: should keep first series of a and first of b (pass1) then stop?
	// Pass1: a, b then c would make 3 - with limit 2 we get a,b only.
	out := capMatrixByContainer(matrix, 2)
	require.Len(t, out, 2)
	containers := []string{string(out[0].Metric["container"]), string(out[1].Metric["container"])}
	assert.Equal(t, []string{"a", "b"}, containers)

	// Limit 3: a, b, c from pass1 (one each); no room for second a.
	out3 := capMatrixByContainer(matrix, 3)
	require.Len(t, out3, 3)
	got := map[string]bool{}
	for _, s := range out3 {
		got[string(s.Metric["container"])] = true
	}
	assert.True(t, got["a"] && got["b"] && got["c"])
}

func TestValidRecordingMetricName(t *testing.T) {
	assert.True(t, ValidRecordingMetricName("attune:container_cpu:rate5m"))
	assert.True(t, ValidRecordingMetricName("container_memory_working_set_bytes"))
	assert.False(t, ValidRecordingMetricName(""))
	assert.False(t, ValidRecordingMetricName("x} or on()"))
	assert.False(t, ValidRecordingMetricName("has space"))
}

func TestGetThrottleRatios_EmptyKeys(t *testing.T) {
	c := &PrometheusCollector{logger: logr.Discard()}
	out, err := c.GetThrottleRatios(context.Background(), "ns", nil, time.Now())
	require.NoError(t, err)
	assert.Empty(t, out)
	out, err = c.GetThrottleRatios(context.Background(), "ns", []throttle.Key{}, time.Now())
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestGetThrottleRatios_SingleKeyDelegates(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		receivedQuery = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Single-key path uses GetThrottleRatio / instant Query shape.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0.25"]}]}}`))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	k := throttle.Key{Pod: "pod-a", Container: "app"}
	out, err := collector.GetThrottleRatios(context.Background(), "default", []throttle.Key{k}, time.Now())
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.InDelta(t, 0.25, out[k], 1e-9)
	// Single-key path uses exact matchers, not regex batch.
	assert.Contains(t, receivedQuery, `pod="pod-a"`)
	assert.Contains(t, receivedQuery, `container="app"`)
	assert.NotContains(t, receivedQuery, `pod=~`)
}

func TestGetThrottleRatios_MultiKeyBatch(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		receivedQuery = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Two wanted series + one extra (should be ignored) + NaN for partial.
		body := `{
			"status":"success",
			"data":{
				"resultType":"vector",
				"result":[
					{"metric":{"pod":"pod-a","container":"app"},"value":[1700000000,"0.1"]},
					{"metric":{"pod":"pod-b","container":"app"},"value":[1700000000,"0.5"]},
					{"metric":{"pod":"pod-a","container":"sidecar"},"value":[1700000000,"NaN"]},
					{"metric":{"pod":"other","container":"app"},"value":[1700000000,"0.9"]}
				]
			}
		}`
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	keys := []throttle.Key{
		{Pod: "pod-a", Container: "app"},
		{Pod: "pod-b", Container: "app"},
		{Pod: "pod-a", Container: "sidecar"},
		// Duplicate pod/container intentionally omitted from second series only once in regex.
		{Pod: "pod-c", Container: "worker"}, // no series returned → absent from map
	}
	out, err := collector.GetThrottleRatios(context.Background(), "prod", keys, time.Now())
	require.NoError(t, err)

	assert.InDelta(t, 0.1, out[throttle.Key{Pod: "pod-a", Container: "app"}], 1e-9)
	assert.InDelta(t, 0.5, out[throttle.Key{Pod: "pod-b", Container: "app"}], 1e-9)
	assert.Zero(t, out[throttle.Key{Pod: "pod-a", Container: "sidecar"}], "NaN must map to 0")
	_, hasC := out[throttle.Key{Pod: "pod-c", Container: "worker"}]
	assert.False(t, hasC, "missing series stays absent (caller treats as 0)")
	_, hasOther := out[throttle.Key{Pod: "other", Container: "app"}]
	assert.False(t, hasOther, "unwanted series must be filtered")

	// Batch path uses regex matchers with sorted unique pod/container sets.
	assert.Contains(t, receivedQuery, `namespace="prod"`)
	assert.Contains(t, receivedQuery, `pod=~"`)
	assert.Contains(t, receivedQuery, `container=~"`)
	assert.Contains(t, receivedQuery, "pod-a")
	assert.Contains(t, receivedQuery, "pod-b")
	assert.Contains(t, receivedQuery, "pod-c")
	assert.Contains(t, receivedQuery, "app")
	assert.Contains(t, receivedQuery, "sidecar")
	assert.Contains(t, receivedQuery, "worker")
}

func TestGetThrottleRatios_EscapesRegexInput(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		receivedQuery = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedEmptyResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	// Special characters must be escaped for =~ and for namespace exact match.
	keys := []throttle.Key{
		{Pod: `pod.with+meta`, Container: `app|x`},
		{Pod: `pod"quote`, Container: `c`},
	}
	_, err = collector.GetThrottleRatios(context.Background(), `ns"x`, keys, time.Now())
	require.NoError(t, err)

	assert.Contains(t, receivedQuery, `namespace="ns\"x"`)
	// Regex metacharacters go through EscapePromQLRegex (regex + PromQL double-escape).
	assert.Contains(t, receivedQuery, EscapePromQLRegex(`pod.with+meta`))
	assert.Contains(t, receivedQuery, EscapePromQLRegex(`app|x`))
	assert.Contains(t, receivedQuery, EscapePromQLRegex(`pod"quote`))
	// Batch uses =~ not exact pod=" for multi-key.
	assert.Contains(t, receivedQuery, `pod=~"`)
	assert.Contains(t, receivedQuery, `container=~"`)
}

func TestGetThrottleRatios_InfMapsToZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status":"success",
			"data":{"resultType":"vector","result":[
				{"metric":{"pod":"p","container":"c"},"value":[1700000000,"+Inf"]}
			]}
		}`))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	k := throttle.Key{Pod: "p", Container: "c"}
	// Two keys force batch path (not single-key delegate).
	out, err := collector.GetThrottleRatios(context.Background(), "ns", []throttle.Key{k, {Pod: "p2", Container: "c2"}}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, out[k])
}

func TestGetThrottleRatios_QueryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedErrorResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	keys := []throttle.Key{
		{Pod: "a", Container: "c"},
		{Pod: "b", Container: "c"},
	}
	_, err = collector.GetThrottleRatios(context.Background(), "ns", keys, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch throttle query failed")
}

func TestGetThrottleRatios_NonVectorResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Scalar is accepted by the client library but not a vector.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"1"]}}`))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollector(server.URL, logr.Discard(), http.DefaultTransport)
	require.NoError(t, err)

	keys := []throttle.Key{
		{Pod: "a", Container: "c"},
		{Pod: "b", Container: "c"},
	}
	out, err := collector.GetThrottleRatios(context.Background(), "ns", keys, time.Now())
	require.NoError(t, err)
	assert.Empty(t, out, "non-vector success returns empty map without error")
}

func TestEffectiveMaxSeries(t *testing.T) {
	c := &PrometheusCollector{}
	assert.Equal(t, DefaultMaxPrometheusSeries, c.effectiveMaxSeries())
	c.maxSeries = 42
	assert.Equal(t, 42, c.effectiveMaxSeries())
	c.maxSeries = -1
	assert.Equal(t, 0, c.effectiveMaxSeries(), "negative is unlimited")
	c.maxSeries = 0
	assert.Equal(t, DefaultMaxPrometheusSeries, c.effectiveMaxSeries())
}

func TestCapMatrixByContainer_EdgeCases(t *testing.T) {
	// limit <= 0 or under limit returns input unchanged.
	s := &model.SampleStream{Metric: model.Metric{"container": "c"}}
	m := model.Matrix{s}
	assert.Same(t, m[0], capMatrixByContainer(m, 0)[0])
	assert.Same(t, m[0], capMatrixByContainer(m, -1)[0])
	assert.Len(t, capMatrixByContainer(m, 5), 1)

	// Prefer one series per container then fill remaining.
	matrix := model.Matrix{
		{Metric: model.Metric{"container": "a", "pod": "p1"}},
		{Metric: model.Metric{"container": "a", "pod": "p2"}},
		{Metric: model.Metric{"container": "b", "pod": "p1"}},
		{Metric: model.Metric{"container": "c", "pod": "p1"}},
	}
	out := capMatrixByContainer(matrix, 3)
	require.Len(t, out, 3)
	containers := map[string]int{}
	for _, series := range out {
		containers[string(series.Metric["container"])]++
	}
	// Pass 1 takes a, b, c (3 distinct) — no room for second "a".
	assert.Equal(t, 1, containers["a"])
	assert.Equal(t, 1, containers["b"])
	assert.Equal(t, 1, containers["c"])

	// limit 2: only first two distinct containers.
	out2 := capMatrixByContainer(matrix, 2)
	require.Len(t, out2, 2)
}
