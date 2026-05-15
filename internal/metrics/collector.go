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
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
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
	api    promv1.API
	logger logr.Logger
}

// ssrfSafeTransport returns an http.RoundTripper that resolves hostnames
// and validates the resolved IP against SSRF blocklists before connecting.
// This defeats DNS rebinding attacks where a hostname initially resolves
// to a legitimate IP but switches to a metadata endpoint during the TTL gap.
func ssrfSafeTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
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

// CollectorOptions configures optional HTTP settings for Prometheus-compatible
// backends (Thanos, VictoriaMetrics, Grafana Mimir, managed services).
type CollectorOptions struct {
	// Headers are added to every HTTP request (e.g. "X-Scope-OrgID" for Mimir).
	Headers map[string]string
	// BearerToken is sent as "Authorization: Bearer <token>".
	BearerToken string
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
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
	clone.Header = req.Header.Clone()
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	if t.bearerToken != "" {
		clone.Header.Set("Authorization", "Bearer "+t.bearerToken)
	}
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
	if len(transport) > 0 && transport[0] != nil {
		rt = transport[0]
	} else {
		base := ssrfSafeTransport()
		if opts != nil && opts.InsecureSkipVerify {
			if httpTransport, ok := base.(*http.Transport); ok {
				if httpTransport.TLSClientConfig == nil {
					httpTransport.TLSClientConfig = &tls.Config{} //nolint:gosec // user-configured
				}
				httpTransport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // user-configured
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

	client, err := promapi.NewClient(promapi.Config{
		Address:      address,
		RoundTripper: rt,
	})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &PrometheusCollector{
		api:    promv1.NewAPI(client),
		logger: logger,
	}, nil
}

// QueryRange executes a Prometheus range query and returns the parsed samples.
// It expects the result to be a matrix type containing at least one series.
func (c *PrometheusCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	grouped, err := c.QueryRangeGrouped(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}

	var samples []Sample
	for _, groupedSamples := range grouped {
		samples = append(samples, groupedSamples...)
	}
	return samples, nil
}

// QueryRangeGrouped executes a Prometheus range query and preserves the
// `container` label from each returned series.
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

	grouped := make(map[string][]Sample, len(matrix))
	for _, series := range matrix {
		container := string(series.Metric[model.LabelName("container")])
		for _, sp := range series.Values {
			grouped[container] = append(grouped[container], Sample{
				Timestamp: sp.Timestamp.Time(),
				Value:     float64(sp.Value),
			})
		}
	}

	return grouped, nil
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
			return 0, fmt.Errorf("empty result from instant query")
		}
		if len(v) != 1 {
			return 0, fmt.Errorf("expected exactly one sample from instant query, got %d", len(v))
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected result type %T, expected vector or scalar", result)
	}
}

// GetThrottleRatio queries Prometheus for the CPU throttle ratio of a container.
// It computes: rate(container_cpu_cfs_throttled_periods_total[5m]) /
// rate(container_cpu_cfs_periods_total[5m]).
// Returns 0.0 if no data is available. Implements safety.ThrottleChecker.
func (c *PrometheusCollector) GetThrottleRatio(ctx context.Context, namespace, pod, container string) (float64, error) {
	// Escape all parameters to prevent PromQL injection.
	ns := EscapePromQL(namespace)
	p := EscapePromQL(pod)
	cont := EscapePromQL(container)
	query := fmt.Sprintf(
		`rate(container_cpu_cfs_throttled_periods_total{namespace="%s",pod="%s",container="%s"}[5m])`+
			` / rate(container_cpu_cfs_periods_total{namespace="%s",pod="%s",container="%s"}[5m])`,
		ns, p, cont, ns, p, cont,
	)
	val, err := c.Query(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}
	return val, nil
}

// EscapePromQL escapes backslashes and quotes for safe interpolation into PromQL strings.
func EscapePromQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// EscapePromQLRegex escapes regex metacharacters in addition to PromQL
// escaping. Used for values interpolated into =~ regex matchers to prevent
// unintended pattern matching (e.g., "." matching any character).
func EscapePromQLRegex(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
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
	return s
}
