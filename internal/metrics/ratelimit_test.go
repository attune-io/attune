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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

func (m *mockThrottleCollector) GetThrottleRatio(_ context.Context, _, _, _ string, _ time.Time) (float64, error) {
	m.throttleCalls++
	return m.throttleRatio, nil
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
