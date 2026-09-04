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
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/attune-io/attune/internal/validation"
)

var errDatadogRedirect = errors.New("datadog redirects are not allowed")

// datadogSeriesResponse models the JSON response from /api/v1/query.
type datadogSeriesResponse struct {
	Status string          `json:"status"`
	Series []datadogSeries `json:"series"`
	Error  string          `json:"error,omitempty"`
}

type datadogSeries struct {
	Metric    string       `json:"metric"`
	Scope     string       `json:"scope"`
	TagSet    []string     `json:"tag_set"`
	Pointlist [][2]float64 `json:"pointlist"`
}

// DatadogCollector implements MetricsCollector by querying the Datadog
// Metrics Query API (/api/v1/query).
type DatadogCollector struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	appKey     string
	logger     logr.Logger
	// isCPUMetric is used to detect whether unit conversion (nanocores->cores)
	// should be applied. Set per-query based on the metric name in the query.
	cpuMetricName string
}

// NewDatadogCollector creates a collector that queries the Datadog API.
// site is the Datadog site (e.g. "datadoghq.com"), apiKey and appKey are
// authentication credentials read from a Kubernetes Secret.
func NewDatadogCollector(site, apiKey, appKey string, logger logr.Logger) (*DatadogCollector, error) {
	if site == "" {
		site = "datadoghq.com"
	}
	if err := validation.DatadogSite(site); err != nil {
		return nil, fmt.Errorf("datadog site: %w", err)
	}
	return &DatadogCollector{
		httpClient: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment},
			CheckRedirect: datadogCheckRedirect,
		},
		baseURL:       fmt.Sprintf("https://api.%s", site),
		apiKey:        apiKey,
		appKey:        appKey,
		logger:        logger,
		cpuMetricName: "kubernetes.cpu.usage.total",
	}, nil
}

func datadogCheckRedirect(*http.Request, []*http.Request) error {
	return errDatadogRedirect
}

// QueryRange executes a Datadog metric query and returns flattened samples.
func (c *DatadogCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	return flattenGrouped(c.QueryRangeGrouped(ctx, query, start, end, step))
}

// QueryRangeGrouped queries the Datadog /api/v1/query endpoint and groups
// results by the kube_container_name tag.
func (c *DatadogCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, _ time.Duration) (map[string][]Sample, error) {
	params := url.Values{
		"from":  {fmt.Sprintf("%d", start.Unix())},
		"to":    {fmt.Sprintf("%d", end.Unix())},
		"query": {query},
	}

	reqURL := fmt.Sprintf("%s/api/v1/query?%s", c.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating Datadog request: %w", err)
	}

	req.Header.Set("DD-API-KEY", c.apiKey)
	if c.appKey != "" {
		req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("datadog API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB limit
	if err != nil {
		return nil, fmt.Errorf("reading Datadog response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("datadog API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var ddResp datadogSeriesResponse
	if err := json.Unmarshal(body, &ddResp); err != nil {
		return nil, fmt.Errorf("parsing Datadog response: %w", err)
	}

	if ddResp.Status == "error" {
		return nil, fmt.Errorf("datadog query error: %s", ddResp.Error)
	}

	isCPU := strings.Contains(query, c.cpuMetricName)

	grouped := make(map[string][]Sample, len(ddResp.Series))
	for _, series := range ddResp.Series {
		container := extractDatadogTag(series.TagSet, "kube_container_name")
		grouped[container] = appendDatadogSamples(ctx, grouped[container], series.Pointlist, isCPU)
	}

	c.logger.V(1).Info("Datadog query completed",
		"query", truncate(query, 120),
		"seriesCount", len(ddResp.Series),
		"totalSamples", countSamples(grouped))

	return grouped, nil
}

// Query executes a Datadog instant query at the given timestamp.
// It queries a small window around ts and returns the latest value.
func (c *DatadogCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	// Datadog doesn't have true instant queries; use a 5-minute window.
	start := ts.Add(-5 * time.Minute)
	samples, err := c.QueryRange(ctx, query, start, ts, time.Minute)
	if err != nil {
		return 0, err
	}
	return latestSampleValue(samples, "Datadog")
}

// Close releases idle HTTP connections on the collector transport.
func (c *DatadogCollector) Close() error {
	if c == nil || c.httpClient == nil {
		return nil
	}
	if t, ok := c.httpClient.Transport.(*http.Transport); ok && t != nil {
		t.CloseIdleConnections()
	}
	return nil
}

// appendFiniteScaled appends a sample after dropping NaN/Inf. CPU values
// are converted from nanocores to cores.
func appendFiniteScaled(ctx context.Context, dst []Sample, ts time.Time, value float64, isCPU bool) []Sample {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		recordDroppedNonFinite(ctx, metricTypeFromCPU(isCPU))
		return dst
	}
	if isCPU {
		value /= 1e9
	}
	return append(dst, Sample{Timestamp: ts, Value: value})
}

// appendDatadogSamples converts Datadog [timestamp_ms, value] points to
// Samples, dropping NaN/Inf. CPU values are converted from nanocores to cores.
func appendDatadogSamples(ctx context.Context, dst []Sample, points [][2]float64, isCPU bool) []Sample {
	for _, point := range points {
		ts := time.Unix(int64(point[0])/1000, 0)
		dst = appendFiniteScaled(ctx, dst, ts, point[1], isCPU)
	}
	return dst
}

// extractDatadogTag extracts a tag value from a Datadog tag set.
// Tags are in "key:value" format.
func extractDatadogTag(tags []string, key string) string {
	prefix := key + ":"
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix)
		}
	}
	return ""
}

// flattenGrouped flattens a grouped sample map into a single slice.
// Used by both DatadogCollector.QueryRange and CloudWatchCollector.QueryRange.
func flattenGrouped(grouped map[string][]Sample, err error) ([]Sample, error) {
	if err != nil {
		return nil, err
	}
	n := 0
	for _, s := range grouped {
		n += len(s)
	}
	samples := make([]Sample, 0, n)
	for _, s := range grouped {
		samples = append(samples, s...)
	}
	return samples, nil
}

// latestSampleValue returns the value of the sample with the latest timestamp.
// It returns an error with the given backend name if the sample slice is empty.
// Used by Datadog and CloudWatch instant queries to avoid duplicating the
// "find latest" logic.
func latestSampleValue(samples []Sample, backend string) (float64, error) {
	if len(samples) == 0 {
		return 0, fmt.Errorf("empty result from %s instant query", backend)
	}
	latest := samples[0]
	for _, s := range samples[1:] {
		if s.Timestamp.After(latest.Timestamp) {
			latest = s
		}
	}
	if math.IsNaN(latest.Value) || math.IsInf(latest.Value, 0) {
		return 0, fmt.Errorf("non-finite value from %s instant query: %v", backend, latest.Value)
	}
	return latest.Value, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func countSamples(grouped map[string][]Sample) int {
	n := 0
	for _, s := range grouped {
		n += len(s)
	}
	return n
}
