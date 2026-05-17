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
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
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

func TestEscapePromQLRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{`with.dot`, `with\.dot`},
		{`a+b*c?d`, `a\+b\*c\?d`},
		{`(group)[class]{brace}`, `\(group\)\[class\]\{brace\}`},
		{`pipe|or`, `pipe\|or`},
		{`^start$end`, `\^start\$end`},
		{`quote"and\backslash`, `quote\"and\\backslash`},
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
