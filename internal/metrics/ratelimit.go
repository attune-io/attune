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
	"io"
	"time"

	"golang.org/x/time/rate"

	"github.com/attune-io/attune/internal/throttle"
)

var _ io.Closer = (*RateLimitedCollector)(nil)

// RateLimitedCollector wraps a MetricsCollector with rate limiting.
type RateLimitedCollector struct {
	inner   MetricsCollector
	limiter *rate.Limiter
}

// NewRateLimitedCollector creates a rate-limited wrapper.
// qps is queries per second (e.g., 10), burst is max burst size (e.g., 20).
func NewRateLimitedCollector(inner MetricsCollector, qps float64, burst int) *RateLimitedCollector {
	return &RateLimitedCollector{
		inner:   inner,
		limiter: rate.NewLimiter(rate.Limit(qps), burst),
	}
}

func (c *RateLimitedCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.QueryRange(ctx, query, start, end, step)
}

func (c *RateLimitedCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]Sample, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.QueryRangeGrouped(ctx, query, start, end, step)
}

func (c *RateLimitedCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	return c.inner.Query(ctx, query, ts)
}

// SupportsThrottle reports whether the inner collector supports CPU
// throttle ratio queries. Callers should check this before registering
// the RateLimitedCollector as a ThrottleChecker, since GetThrottleRatio
// returns 0 (no throttling) when the inner does not support it.
func (c *RateLimitedCollector) SupportsThrottle() bool {
	_, ok := c.inner.(throttle.Checker)
	return ok
}

// GetThrottleRatio delegates to the inner collector if it implements
// throttle.Checker. Returns 0.0 if the inner collector does not
// support throttle queries.
func (c *RateLimitedCollector) GetThrottleRatio(ctx context.Context, namespace, pod, container string, ts time.Time) (float64, error) {
	if tc, ok := c.inner.(throttle.Checker); ok {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, err
		}
		return tc.GetThrottleRatio(ctx, namespace, pod, container, ts)
	}
	return 0, nil
}

// GetThrottleRatios delegates to the inner collector if it implements
// throttle.BatchChecker. One rate-limit token covers each PromQL chunk
// (maxThrottleBatchKeys keys), not one token per key. Without this method,
// the production RateLimitedCollector wrapper does not satisfy BatchChecker
// and the safety path falls back to N per-pod GetThrottleRatio calls.
func (c *RateLimitedCollector) GetThrottleRatios(ctx context.Context, namespace string, keys []throttle.Key, ts time.Time) (map[throttle.Key]float64, error) {
	if bc, ok := c.inner.(throttle.BatchChecker); ok {
		if len(keys) <= maxThrottleBatchKeys {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
			return bc.GetThrottleRatios(ctx, namespace, keys, ts)
		}
		out := make(map[throttle.Key]float64, len(keys))
		for i := 0; i < len(keys); i += maxThrottleBatchKeys {
			end := i + maxThrottleBatchKeys
			if end > len(keys) {
				end = len(keys)
			}
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
			part, err := bc.GetThrottleRatios(ctx, namespace, keys[i:end], ts)
			if err != nil {
				return nil, err
			}
			for k, v := range part {
				out[k] = v
			}
		}
		return out, nil
	}
	// Inner is Checker-only: fall back to per-key (still rate-limited).
	out := make(map[throttle.Key]float64, len(keys))
	for _, k := range keys {
		v, err := c.GetThrottleRatio(ctx, namespace, k.Pod, k.Container, ts)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// Close forwards to the inner collector when it implements io.Closer.
// Production caches *RateLimitedCollector, so eviction must reach
// PrometheusCollector.Close / DatadogCollector.Close through this wrapper.
func (c *RateLimitedCollector) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
