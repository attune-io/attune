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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/attune-io/attune/internal/throttle"
)

// mockCollector implements MetricsCollector for testing.
type mockCollector struct {
	queryRangeCalls        int
	queryRangeGroupedCalls int
	queryCalls             int
	queryRangeFunc         func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error)
	queryRangeGroupedFunc  func(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]Sample, error)
	queryFunc              func(ctx context.Context, query string, ts time.Time) (float64, error)
}

func (m *mockCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	m.queryRangeCalls++
	if m.queryRangeFunc != nil {
		return m.queryRangeFunc(ctx, query, start, end, step)
	}
	return []Sample{{Timestamp: start, Value: 0.5}}, nil
}

func (m *mockCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]Sample, error) {
	m.queryRangeGroupedCalls++
	if m.queryRangeGroupedFunc != nil {
		return m.queryRangeGroupedFunc(ctx, query, start, end, step)
	}
	return map[string][]Sample{"": {{Timestamp: start, Value: 0.5}}}, nil
}

func (m *mockCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	m.queryCalls++
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, ts)
	}
	return 42.0, nil
}

func TestRateLimitedCollector_PassesThrough(t *testing.T) {
	mock := &mockCollector{}
	rl := NewRateLimitedCollector(mock, 10, 20)

	ctx := context.Background()
	now := time.Now()

	// QueryRange passes through to inner collector.
	samples, err := rl.QueryRange(ctx, "cpu_usage", now.Add(-time.Hour), now, 5*time.Minute)
	require.NoError(t, err)
	assert.Len(t, samples, 1)
	assert.InDelta(t, 0.5, samples[0].Value, 0.001)
	assert.Equal(t, 1, mock.queryRangeCalls)

	// Query passes through to inner collector.
	val, err := rl.Query(ctx, "mem_usage", now)
	require.NoError(t, err)
	assert.InDelta(t, 42.0, val, 0.001)
	assert.Equal(t, 1, mock.queryCalls)
}

func TestRateLimitedCollector_QueryRangeGrouped(t *testing.T) {
	mock := &mockCollector{}
	rl := NewRateLimitedCollector(mock, 10, 20)

	ctx := context.Background()
	now := time.Now()

	grouped, err := rl.QueryRangeGrouped(ctx, "cpu_usage", now.Add(-time.Hour), now, 5*time.Minute)
	require.NoError(t, err)
	assert.Len(t, grouped, 1)
	assert.Contains(t, grouped, "")
	assert.Len(t, grouped[""], 1)
	assert.InDelta(t, 0.5, grouped[""][0].Value, 0.001)
	assert.Equal(t, 1, mock.queryRangeGroupedCalls)
}

func TestRateLimitedCollector_CancelledContext(t *testing.T) {
	mock := &mockCollector{}
	rl := NewRateLimitedCollector(mock, 10, 20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	now := time.Now()

	// QueryRange should return context error.
	_, err := rl.QueryRange(ctx, "cpu_usage", now.Add(-time.Hour), now, 5*time.Minute)
	assert.Error(t, err)
	assert.Equal(t, 0, mock.queryRangeCalls)

	// QueryRangeGrouped should return context error.
	_, err = rl.QueryRangeGrouped(ctx, "cpu_usage", now.Add(-time.Hour), now, 5*time.Minute)
	assert.Error(t, err)
	assert.Equal(t, 0, mock.queryRangeGroupedCalls)

	// Query should return context error.
	_, err = rl.Query(ctx, "mem_usage", now)
	assert.Error(t, err)
	assert.Equal(t, 0, mock.queryCalls)
}

// mockThrottleCollector implements both MetricsCollector and the throttle checker interface.
type mockThrottleCollector struct {
	mockCollector
	throttleCalls int
	throttleRatio float64
	batchCalls    int
	batchOut      map[throttle.Key]float64
	// batchFailOnCall, when >0, returns an error on that batchCalls count
	// (1-based) so multi-chunk wrappers can prove they do not return partial maps.
	batchFailOnCall int
}

func (m *mockThrottleCollector) GetThrottleRatio(_ context.Context, _, _, _ string, _ time.Time) (float64, error) {
	m.throttleCalls++
	return m.throttleRatio, nil
}

func (m *mockThrottleCollector) GetThrottleRatios(_ context.Context, _ string, keys []throttle.Key, _ time.Time) (map[throttle.Key]float64, error) {
	m.batchCalls++
	if m.batchFailOnCall > 0 && m.batchCalls == m.batchFailOnCall {
		return nil, fmt.Errorf("simulated batch failure on call %d", m.batchCalls)
	}
	if m.batchOut != nil {
		part := make(map[throttle.Key]float64, len(keys))
		for _, k := range keys {
			if v, ok := m.batchOut[k]; ok {
				part[k] = v
			}
		}
		return part, nil
	}
	out := make(map[throttle.Key]float64, len(keys))
	for _, k := range keys {
		out[k] = m.throttleRatio
	}
	return out, nil
}

func TestRateLimitedCollector_SupportsThrottle(t *testing.T) {
	t.Run("inner implements ThrottleChecker", func(t *testing.T) {
		inner := &mockThrottleCollector{}
		rl := NewRateLimitedCollector(inner, 10, 20)
		assert.True(t, rl.SupportsThrottle())
	})

	t.Run("inner does not implement ThrottleChecker", func(t *testing.T) {
		inner := &mockCollector{}
		rl := NewRateLimitedCollector(inner, 10, 20)
		assert.False(t, rl.SupportsThrottle())
	})
}

func TestRateLimitedCollector_GetThrottleRatio_Delegates(t *testing.T) {
	inner := &mockThrottleCollector{throttleRatio: 0.75}
	rl := NewRateLimitedCollector(inner, 10, 20)

	ratio, err := rl.GetThrottleRatio(context.Background(), "ns", "pod", "container", time.Now())
	require.NoError(t, err)
	assert.InDelta(t, 0.75, ratio, 0.001)
	assert.Equal(t, 1, inner.throttleCalls)
}

func TestRateLimitedCollector_GetThrottleRatio_InnerNotThrottleChecker(t *testing.T) {
	inner := &mockCollector{} // does NOT implement ThrottleChecker
	rl := NewRateLimitedCollector(inner, 10, 20)

	ratio, err := rl.GetThrottleRatio(context.Background(), "ns", "pod", "container", time.Now())
	require.NoError(t, err)
	assert.InDelta(t, 0.0, ratio, 0.001)
}

func TestRateLimitedCollector_GetThrottleRatio_CancelledContext(t *testing.T) {
	inner := &mockThrottleCollector{throttleRatio: 0.5}
	rl := NewRateLimitedCollector(inner, 10, 20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := rl.GetThrottleRatio(ctx, "ns", "pod", "container", time.Now())
	assert.Error(t, err)
	assert.Equal(t, 0, inner.throttleCalls)
}

func TestRateLimitedCollector_ImplementsBatchChecker(t *testing.T) {
	// Production wraps Prometheus in RateLimitedCollector; safety casts to
	// BatchChecker. Without GetThrottleRatios on the wrapper, batch is dead.
	var _ throttle.BatchChecker = NewRateLimitedCollector(&mockThrottleCollector{}, 10, 20)
}

func TestRateLimitedCollector_GetThrottleRatios_Delegates(t *testing.T) {
	k1 := throttle.Key{Pod: "p1", Container: "c"}
	k2 := throttle.Key{Pod: "p2", Container: "c"}
	inner := &mockThrottleCollector{
		batchOut: map[throttle.Key]float64{k1: 0.2, k2: 0.4},
	}
	rl := NewRateLimitedCollector(inner, 10, 20)

	out, err := rl.GetThrottleRatios(context.Background(), "ns", []throttle.Key{k1, k2}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.batchCalls, "one batch call, not per-key")
	assert.Equal(t, 0, inner.throttleCalls)
	assert.InDelta(t, 0.2, out[k1], 1e-9)
	assert.InDelta(t, 0.4, out[k2], 1e-9)
}

func TestRateLimitedCollector_GetThrottleRatios_ChunksLargeSets(t *testing.T) {
	// RateLimitedCollector must issue one Wait+batch call per chunk so
	// large fleets do not bypass the limiter with a single token.
	n := maxThrottleBatchKeys*2 + 5
	keys := make([]throttle.Key, n)
	batchOut := make(map[throttle.Key]float64, n)
	for i := range keys {
		keys[i] = throttle.Key{Pod: fmt.Sprintf("pod-%d", i), Container: "app"}
		batchOut[keys[i]] = 0.1
	}
	inner := &mockThrottleCollector{batchOut: batchOut}
	rl := NewRateLimitedCollector(inner, 1000, 1000) // high QPS so Wait never blocks

	out, err := rl.GetThrottleRatios(context.Background(), "ns", keys, time.Now())
	require.NoError(t, err)
	require.Len(t, out, n)
	assert.Equal(t, 3, inner.batchCalls, "expect one batch call per chunk")
}

func TestRateLimitedCollector_GetThrottleRatios_SecondChunkError(t *testing.T) {
	n := maxThrottleBatchKeys + 3
	keys := make([]throttle.Key, n)
	batchOut := make(map[throttle.Key]float64, n)
	for i := range keys {
		keys[i] = throttle.Key{Pod: fmt.Sprintf("pod-%d", i), Container: "app"}
		batchOut[keys[i]] = 0.1
	}
	inner := &mockThrottleCollector{batchOut: batchOut, batchFailOnCall: 2}
	rl := NewRateLimitedCollector(inner, 1000, 1000)

	out, err := rl.GetThrottleRatios(context.Background(), "ns", keys, time.Now())
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "simulated batch failure")
	assert.Equal(t, 2, inner.batchCalls)
}

func TestRateLimitedCollector_GetThrottleRatios_FallbackPerKey(t *testing.T) {
	// Checker only (no BatchChecker on inner): wrapper still implements
	// BatchChecker by falling back to rate-limited GetThrottleRatio.
	inner := &mockCheckerOnly{ratio: 0.3}
	rl := NewRateLimitedCollector(inner, 10, 20)

	keys := []throttle.Key{
		{Pod: "a", Container: "c"},
		{Pod: "b", Container: "c"},
	}
	out, err := rl.GetThrottleRatios(context.Background(), "ns", keys, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, inner.calls)
	assert.InDelta(t, 0.3, out[keys[0]], 1e-9)
	assert.InDelta(t, 0.3, out[keys[1]], 1e-9)
}

// mockCheckerOnly implements MetricsCollector + throttle.Checker (not BatchChecker).
type mockCheckerOnly struct {
	mockCollector
	calls int
	ratio float64
}

func (m *mockCheckerOnly) GetThrottleRatio(_ context.Context, _, _, _ string, _ time.Time) (float64, error) {
	m.calls++
	return m.ratio, nil
}
