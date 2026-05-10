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
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	// Query executes an instant query against Prometheus at the given
	// timestamp and returns a single scalar value.
	Query(ctx context.Context, query string, ts time.Time) (float64, error)
}

// PrometheusCollector implements MetricsCollector using the Prometheus HTTP API.
type PrometheusCollector struct {
	api    promv1.API
	logger *slog.Logger
}

// NewPrometheusCollector creates a new PrometheusCollector that queries the
// Prometheus instance at the given address (e.g. "http://prometheus:9090").
func NewPrometheusCollector(address string, logger *slog.Logger) (*PrometheusCollector, error) {
	client, err := promapi.NewClient(promapi.Config{
		Address: address,
	})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PrometheusCollector{
		api:    promv1.NewAPI(client),
		logger: logger,
	}, nil
}

// QueryRange executes a Prometheus range query and returns the parsed samples.
// It expects the result to be a matrix type containing at least one series.
func (c *PrometheusCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	result, warnings, err := c.api.QueryRange(ctx, query, promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus range query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.Warn("prometheus range query returned warnings",
			"warnings", strings.Join(warnings, "; "))
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected result type %T, expected matrix", result)
	}

	var samples []Sample
	for _, series := range matrix {
		for _, sp := range series.Values {
			samples = append(samples, Sample{
				Timestamp: sp.Timestamp.Time(),
				Value:     float64(sp.Value),
			})
		}
	}

	return samples, nil
}

// Query executes a Prometheus instant query and returns a single float64 value.
// It expects the result to be a vector type containing exactly one sample.
func (c *PrometheusCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	result, warnings, err := c.api.Query(ctx, query, ts)
	if err != nil {
		return 0, fmt.Errorf("prometheus instant query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.Warn("prometheus instant query returned warnings",
			"warnings", strings.Join(warnings, "; "))
	}

	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, fmt.Errorf("empty result from instant query")
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected result type %T, expected vector or scalar", result)
	}
}
