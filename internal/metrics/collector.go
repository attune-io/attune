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

// Package metrics provides a Prometheus query client for collecting
// container resource usage metrics.
package metrics

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/attune-io/attune/internal/throttle"
)

// Sample represents a single metric data point with a timestamp and value.
type Sample struct {
	Timestamp time.Time
	Value     float64
}

// MetricsCollector defines the interface for querying Prometheus metrics.
// Implementations can be swapped for testing.
type MetricsCollector interface {
	// QueryRange executes a range query against Prometheus and returns
	// the resulting samples. The query is evaluated from start to end
	// with the given step interval.
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error)

	// QueryRangeGrouped executes a range query against Prometheus and returns
	// samples grouped by the Prometheus `container` label. Series without a
	// container label are returned under the empty-string key.
	QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]Sample, error)

	// Query executes an instant query against Prometheus at the given
	// timestamp and returns a single scalar value.
	Query(ctx context.Context, query string, ts time.Time) (float64, error)
}

// PrometheusCollector implements MetricsCollector using the Prometheus HTTP API.
type PrometheusCollector struct {
	api       promv1.API
	logger    logr.Logger
	transport *http.Transport
	// maxSeries caps matrix series per range query. 0 = DefaultMaxPrometheusSeries;
	// negative = unlimited.
	maxSeries int
}

// Close releases resources held by the collector. It closes idle HTTP
// connections on the underlying transport to prevent connection leaks
// when the collector is evicted from the cache.
func (c *PrometheusCollector) Close() error {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}

// ssrfSafeTransport returns an http.RoundTripper that resolves hostnames
// and validates the resolved IP against SSRF blocklists before connecting.
// This defeats DNS rebinding attacks where a hostname initially resolves
// to a legitimate IP but switches to a metadata endpoint during the TTL gap.
func ssrfSafeTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("SSRF dial: invalid address %q: %w", addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("SSRF dial: DNS resolution failed for %q: %w", host, err)
			}
			for _, ip := range ips {
				if isBlockedIP(ip.IP) {
					return nil, fmt.Errorf("SSRF blocked: %s resolved to blocked address %s", host, ip.IP)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   10,
	}
}

// awsIMDSv6 is the AWS EC2 Instance Metadata Service v2 IPv6 endpoint.
// It lives in fd00::/8 (Unique Local Address), which is NOT link-local.
var awsIMDSv6 = net.ParseIP("fd00:ec2::254")

// isBlockedIP returns true for IPs that should never be contacted by the
// operator: loopback, link-local, unspecified, and the AWS IMDSv2 IPv6 endpoint.
// Private IPs (10.x, 172.16.x, 192.168.x) are intentionally allowed because
// Prometheus typically runs on a ClusterIP service inside the cluster.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.Equal(awsIMDSv6)
}

var errEmptyInstantQuery = errors.New("empty result from instant query")
var errNonFiniteInstantQuery = errors.New("instant query returned NaN or Inf (often 0/0 in PromQL); rewrite the query so it always returns a finite scalar")

// ErrSeriesCapped is returned when a range query returned more series than
// MaxSeries and the result was truncated. Callers may still use the partial
// map and should surface a status condition.
var ErrSeriesCapped = errors.New("prometheus series capped")

// DefaultMaxPrometheusSeries is the default per-query series cap (0 = unlimited
// when not configured on the collector).
const DefaultMaxPrometheusSeries = 5000

// CollectorOptions configures optional HTTP settings for Prometheus-compatible
// backends (Thanos, VictoriaMetrics, Grafana Mimir, managed services).
type CollectorOptions struct {
	// Headers are added to every HTTP request (e.g. "X-Scope-OrgID" for Mimir).
	Headers map[string]string
	// QueryParameters are appended to every query request URL
	// (e.g. "dedup=true" for Thanos Query).
	QueryParameters map[string]string
	// BearerToken is sent as "Authorization: Bearer <token>".
	BearerToken string
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
	// TLSMinVersion is the minimum TLS version to accept (e.g. tls.VersionTLS12).
	// Zero means use Go defaults (TLS 1.2).
	TLSMinVersion uint16
	// MaxSeries caps the number of series kept from a range query matrix.
	// Zero means use DefaultMaxPrometheusSeries. Negative means unlimited.
	MaxSeries int
}

// headerTransport wraps an http.RoundTripper and injects custom headers
// and/or a bearer token into every request.
type headerTransport struct {
	base        http.RoundTripper
	headers     map[string]string
	bearerToken string
	baseURL     *url.URL
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Scheme == b.Scheme && a.Host == b.Host
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.baseURL != nil && !sameOrigin(t.baseURL, req.URL) {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	if t.bearerToken != "" {
		clone.Header.Set("Authorization", "Bearer "+t.bearerToken)
	}
	return t.base.RoundTrip(clone)
}

// queryParamTransport wraps an http.RoundTripper and appends extra URL query
// parameters to every request. Used for backend-specific settings like Thanos
// deduplication ("dedup=true") or partial response ("partial_response=true").
type queryParamTransport struct {
	base   http.RoundTripper
	params map[string]string
}

func (t *queryParamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	q := clone.URL.Query()
	for k, v := range t.params {
		if q.Has(k) {
			continue
		}
		q.Set(k, v)
	}
	clone.URL.RawQuery = q.Encode()
	return t.base.RoundTrip(clone)
}

// NewPrometheusCollector creates a new PrometheusCollector that queries the
// Prometheus instance at the given address (e.g. "http://prometheus-server.monitoring:80").
// It uses an SSRF-safe HTTP transport that validates resolved IPs.
// An optional http.RoundTripper can be passed to override the default SSRF-safe
// transport (used in tests with httptest.NewServer on localhost).
func NewPrometheusCollector(address string, logger logr.Logger, transport ...http.RoundTripper) (*PrometheusCollector, error) {
	return NewPrometheusCollectorWithOptions(address, logger, nil, transport...)
}

// NewPrometheusCollectorWithOptions creates a collector with custom headers,
// bearer token auth, and TLS settings for Prometheus-compatible backends.
func NewPrometheusCollectorWithOptions(address string, logger logr.Logger, opts *CollectorOptions, transport ...http.RoundTripper) (*PrometheusCollector, error) {
	var rt http.RoundTripper
	var httpTransport *http.Transport
	if len(transport) > 0 && transport[0] != nil {
		rt = transport[0]
		httpTransport, _ = rt.(*http.Transport)
	} else {
		base := ssrfSafeTransport()
		httpTransport, _ = base.(*http.Transport)
		if opts != nil && httpTransport != nil {
			if opts.InsecureSkipVerify || opts.TLSMinVersion != 0 {
				if httpTransport.TLSClientConfig == nil {
					httpTransport.TLSClientConfig = &tls.Config{} //nolint:gosec // user-configured
				}
				if opts.InsecureSkipVerify {
					httpTransport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // user-configured
				}
				if opts.TLSMinVersion != 0 {
					httpTransport.TLSClientConfig.MinVersion = opts.TLSMinVersion
				}
			}
		}
		rt = base
	}

	// Wrap with header/token injection if needed.
	if opts != nil && (len(opts.Headers) > 0 || opts.BearerToken != "") {
		parsedAddress, err := url.Parse(address)
		if err != nil {
			return nil, fmt.Errorf("parsing prometheus address: %w", err)
		}
		rt = &headerTransport{base: rt, headers: opts.Headers, bearerToken: opts.BearerToken, baseURL: parsedAddress}
	}

	// Wrap with query parameter injection if needed (Thanos, VictoriaMetrics).
	if opts != nil && len(opts.QueryParameters) > 0 {
		rt = &queryParamTransport{base: rt, params: opts.QueryParameters}
	}

	client, err := promapi.NewClient(promapi.Config{
		Address:      address,
		RoundTripper: rt,
	})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	maxSeries := 0
	if opts != nil {
		maxSeries = opts.MaxSeries
	}
	return &PrometheusCollector{
		api:       promv1.NewAPI(client),
		logger:    logger,
		transport: httpTransport,
		maxSeries: maxSeries,
	}, nil
}

// QueryRange executes a Prometheus range query and returns the parsed samples.
// It expects the result to be a matrix type containing at least one series.
// ErrSeriesCapped may be returned with partial samples.
func (c *PrometheusCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	grouped, err := c.QueryRangeGrouped(ctx, query, start, end, step)
	var samples []Sample
	for _, groupedSamples := range grouped {
		samples = append(samples, groupedSamples...)
	}
	return samples, err
}

// effectiveMaxSeries returns the series cap for this collector.
func (c *PrometheusCollector) effectiveMaxSeries() int {
	if c.maxSeries < 0 {
		return 0 // unlimited
	}
	if c.maxSeries == 0 {
		return DefaultMaxPrometheusSeries
	}
	return c.maxSeries
}

// QueryRangeGrouped executes a Prometheus range query and preserves the
// `container` label from each returned series. When more series are returned
// than MaxSeries allows, the map is truncated and ErrSeriesCapped is returned
// (partial data is still usable).
func (c *PrometheusCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]Sample, error) {
	result, warnings, err := c.api.QueryRange(ctx, query, promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus range query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.Info("Prometheus range query returned warnings",
			"warnings", strings.Join(warnings, "; "))
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected result type %T, expected matrix", result)
	}

	limit := c.effectiveMaxSeries()
	capped := limit > 0 && len(matrix) > limit
	if capped {
		c.logger.Info("Prometheus range query series capped",
			"limit", limit, "got", len(matrix))
		c.logger.V(2).Info("Prometheus range query series capped",
			"limit", limit, "got", len(matrix), "query", query)
		// Prefer one series per container before filling remaining budget so
		// high-cardinality pods do not starve entire containers under None aggregation.
		matrix = capMatrixByContainer(matrix, limit)
	}

	grouped := make(map[string][]Sample, len(matrix))
	for _, series := range matrix {
		container := string(series.Metric[model.LabelName("container")])
		before := len(grouped[container])
		for _, sp := range series.Values {
			v := float64(sp.Value)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			grouped[container] = append(grouped[container], Sample{
				Timestamp: sp.Timestamp.Time(),
				Value:     v,
			})
		}
		if len(series.Values) > 0 && len(grouped[container]) == before {
			recordDroppedNonFinite(ctx, fallbackMetricType(query))
		}
	}

	if capped {
		return grouped, fmt.Errorf("%w: kept %d series", ErrSeriesCapped, limit)
	}
	return grouped, nil
}

// capMatrixByContainer keeps at most limit series, preferring first-seen series
// per container label, then filling remaining slots in matrix order.
func capMatrixByContainer(matrix model.Matrix, limit int) model.Matrix {
	if limit <= 0 || len(matrix) <= limit {
		return matrix
	}
	seenContainer := make(map[string]bool, limit)
	out := make(model.Matrix, 0, limit)
	// Pass 1: one series per distinct container.
	for _, series := range matrix {
		if len(out) >= limit {
			break
		}
		c := string(series.Metric[model.LabelName("container")])
		if seenContainer[c] {
			continue
		}
		seenContainer[c] = true
		out = append(out, series)
	}
	// Pass 2: fill remaining budget with additional series (other pods).
	if len(out) < limit {
		kept := make(map[*model.SampleStream]bool, len(out))
		for _, s := range out {
			kept[s] = true
		}
		for _, series := range matrix {
			if len(out) >= limit {
				break
			}
			if kept[series] {
				continue
			}
			out = append(out, series)
		}
	}
	return out
}

// Query executes a Prometheus instant query and returns a single float64 value.
// It accepts either a scalar result or a vector containing exactly one sample.
func (c *PrometheusCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	result, warnings, err := c.api.Query(ctx, query, ts)
	if err != nil {
		return 0, fmt.Errorf("prometheus instant query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.Info("Prometheus instant query returned warnings",
			"warnings", strings.Join(warnings, "; "))
	}

	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, errEmptyInstantQuery
		}
		if len(v) != 1 {
			return 0, fmt.Errorf("expected exactly one sample from instant query, got %d", len(v))
		}
		return finiteInstantValue(float64(v[0].Value))
	case *model.Scalar:
		return finiteInstantValue(float64(v.Value))
	default:
		return 0, fmt.Errorf("unexpected result type %T, expected vector or scalar", result)
	}
}

func finiteInstantValue(v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errNonFiniteInstantQuery
	}
	return v, nil
}

// GetThrottleRatio queries Prometheus for the CPU throttle ratio of a container.
// It computes: rate(container_cpu_cfs_throttled_periods_total[5m]) /
// rate(container_cpu_cfs_periods_total[5m]).
// Returns 0.0 if no data is available. Implements safety.ThrottleChecker.
func (c *PrometheusCollector) GetThrottleRatio(ctx context.Context, namespace, pod, container string, ts time.Time) (float64, error) {
	// Escape all parameters to prevent PromQL injection.
	ns := EscapePromQL(namespace)
	p := EscapePromQL(pod)
	cont := EscapePromQL(container)
	query := fmt.Sprintf(
		`rate(container_cpu_cfs_throttled_periods_total{namespace="%s",pod="%s",container="%s"}[5m])`+
			` / rate(container_cpu_cfs_periods_total{namespace="%s",pod="%s",container="%s"}[5m])`,
		ns, p, cont, ns, p, cont,
	)
	val, err := c.Query(ctx, query, ts)
	if err != nil {
		if errors.Is(err, errEmptyInstantQuery) || errors.Is(err, errNonFiniteInstantQuery) {
			return 0, nil
		}
		return 0, err
	}
	return val, nil
}

// maxThrottleBatchKeys limits pod/container pairs per PromQL batch so the
// pod=~|...| regex stays bounded for large safety observation sets.
const maxThrottleBatchKeys = 64

// GetThrottleRatios batch-queries throttle ratios for many pod/container pairs
// in one PromQL vector (pod=~ and container=~). Implements throttle.BatchChecker.
// Large key sets are split into chunks of maxThrottleBatchKeys.
func (c *PrometheusCollector) GetThrottleRatios(ctx context.Context, namespace string, keys []throttle.Key, ts time.Time) (map[throttle.Key]float64, error) {
	out := make(map[throttle.Key]float64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	if len(keys) == 1 {
		v, err := c.GetThrottleRatio(ctx, namespace, keys[0].Pod, keys[0].Container, ts)
		if err != nil {
			return nil, err
		}
		out[keys[0]] = v
		return out, nil
	}
	if len(keys) > maxThrottleBatchKeys {
		c.logger.V(1).Info("Chunking batch throttle query",
			"keys", len(keys), "chunkSize", maxThrottleBatchKeys)
	}
	for i := 0; i < len(keys); i += maxThrottleBatchKeys {
		end := i + maxThrottleBatchKeys
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		part, err := c.getThrottleRatiosChunk(ctx, namespace, chunk, ts)
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			out[k] = v
		}
	}
	return out, nil
}

// getThrottleRatiosChunk runs one PromQL vector query for a bounded key set.
func (c *PrometheusCollector) getThrottleRatiosChunk(ctx context.Context, namespace string, keys []throttle.Key, ts time.Time) (map[throttle.Key]float64, error) {
	out := make(map[throttle.Key]float64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	if len(keys) == 1 {
		v, err := c.GetThrottleRatio(ctx, namespace, keys[0].Pod, keys[0].Container, ts)
		if err != nil {
			return nil, err
		}
		out[keys[0]] = v
		return out, nil
	}
	pods := make([]string, 0, len(keys))
	ctrs := make([]string, 0, len(keys))
	seenP := map[string]bool{}
	seenC := map[string]bool{}
	for _, k := range keys {
		if !seenP[k.Pod] {
			seenP[k.Pod] = true
			pods = append(pods, EscapePromQLRegex(k.Pod))
		}
		if !seenC[k.Container] {
			seenC[k.Container] = true
			ctrs = append(ctrs, EscapePromQLRegex(k.Container))
		}
	}
	slices.Sort(pods)
	slices.Sort(ctrs)
	ns := EscapePromQL(namespace)
	podRE := strings.Join(pods, "|")
	ctrRE := strings.Join(ctrs, "|")
	query := fmt.Sprintf(
		`rate(container_cpu_cfs_throttled_periods_total{namespace="%s",pod=~"%s",container=~"%s"}[5m])`+
			` / rate(container_cpu_cfs_periods_total{namespace="%s",pod=~"%s",container=~"%s"}[5m])`,
		ns, podRE, ctrRE, ns, podRE, ctrRE,
	)
	result, warnings, err := c.api.Query(ctx, query, ts)
	if err != nil {
		return nil, fmt.Errorf("prometheus batch throttle query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.Info("Prometheus batch throttle query returned warnings",
			"warnings", strings.Join(warnings, "; "))
	}
	vec, ok := result.(model.Vector)
	if ok {
		wanted := make(map[throttle.Key]bool, len(keys))
		for _, k := range keys {
			wanted[k] = true
		}
		for _, sample := range vec {
			pod := string(sample.Metric[model.LabelName("pod")])
			ctr := string(sample.Metric[model.LabelName("container")])
			k := throttle.Key{Pod: pod, Container: ctr}
			if !wanted[k] {
				continue
			}
			v := float64(sample.Value)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				out[k] = 0
				continue
			}
			out[k] = v
		}
	}
	// Match GetThrottleRatio empty/no-data semantics: every requested key is
	// present so callers can install a complete throttleRatioCache without
	// falling back to N per-pod queries for silent pods (no CFS series).
	// Also applies when the result is not a vector (treat as no data).
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			out[k] = 0
		}
	}
	return out, nil
}

// EscapePromQL escapes backslashes, quotes, and control characters for safe
// interpolation into PromQL strings. PromQL uses Go-style string escaping.
func EscapePromQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// EscapePromQLRegex escapes regex metacharacters in addition to PromQL
// escaping. Used for values interpolated into =~ regex matchers to prevent
// unintended pattern matching (e.g., "." matching any character).
func EscapePromQLRegex(s string) string {
	// Step 1: Escape regex metacharacters with a single backslash.
	// This must happen BEFORE PromQL string escaping so the backslashes
	// introduced here get doubled in step 2.
	//
	// Order matters: escape existing backslashes first to avoid re-escaping
	// the backslashes we introduce for other metacharacters.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `.`, `\.`)
	s = strings.ReplaceAll(s, `+`, `\+`)
	s = strings.ReplaceAll(s, `*`, `\*`)
	s = strings.ReplaceAll(s, `?`, `\?`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	s = strings.ReplaceAll(s, `[`, `\[`)
	s = strings.ReplaceAll(s, `]`, `\]`)
	s = strings.ReplaceAll(s, `{`, `\{`)
	s = strings.ReplaceAll(s, `}`, `\}`)
	s = strings.ReplaceAll(s, `|`, `\|`)
	s = strings.ReplaceAll(s, `^`, `\^`)
	s = strings.ReplaceAll(s, `$`, `\$`)

	// Step 2: PromQL string escaping. Double all backslashes and escape
	// quotes so the result is valid inside a PromQL "..." string literal.
	// PromQL uses Go-style string escaping where only \\, \", \n, \t etc.
	// are recognized; bare \. or \+ would cause a parse error.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
