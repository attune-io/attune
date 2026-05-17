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
	"time"

	"golang.org/x/time/rate"

	"github.com/SebTardifLabs/kube-rightsize/internal/throttle"
)

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

// GetThrottleRatio delegates to the inner collector if it implements
// safety.ThrottleChecker. Returns 0.0 if the inner collector does not
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
