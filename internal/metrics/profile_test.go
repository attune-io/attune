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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// makeTimestamp creates a time at the given hour on a base date, offset by
// the given number of days.
func makeTimestamp(day, hour, minute int) time.Time {
	return time.Date(2026, 1, 1+day, hour, minute, 0, 0, time.UTC)
}

// generateSteadySamples creates samples with a steady value across multiple
// days and hours.
func generateSteadySamples(value float64, days int) []Sample {
	var samples []Sample
	for d := 0; d < days; d++ {
		for h := 0; h < 24; h++ {
			for m := 0; m < 60; m += 5 {
				samples = append(samples, Sample{
					Timestamp: makeTimestamp(d, h, m),
					Value:     value,
				})
			}
		}
	}
	return samples
}

func TestBuildProfile(t *testing.T) {
	tests := []struct {
		name           string
		samples        []Sample
		wantBurst      bool
		wantConfidence func(float64) bool
		wantDataPoints int
		checkHourly    bool
	}{
		{
			name:      "empty samples returns zero profile",
			samples:   nil,
			wantBurst: false,
			wantConfidence: func(c float64) bool {
				return c == 0
			},
			wantDataPoints: 0,
		},
		{
			name:      "steady usage has no bursts and high confidence",
			samples:   generateSteadySamples(0.1, 7), // 100m CPU for 7 days
			wantBurst: false,
			wantConfidence: func(c float64) bool {
				return c > 0.8
			},
			wantDataPoints: 7 * 24 * 12,
			checkHourly:    true,
		},
		{
			name: "bursty usage is detected",
			samples: func() []Sample {
				samples := generateSteadySamples(0.1, 3)
				// Add spike samples at hour 14.
				for i := 0; i < 5; i++ {
					samples = append(samples, Sample{
						Timestamp: makeTimestamp(1, 14, i),
						Value:     2.0, // 2000m, which is 20x the normal 100m
					})
				}
				return samples
			}(),
			wantBurst: true,
			wantConfidence: func(c float64) bool {
				return c > 0.3
			},
			wantDataPoints: 3*24*12 + 5,
		},
		{
			name: "insufficient data has low confidence",
			samples: []Sample{
				{Timestamp: makeTimestamp(0, 10, 0), Value: 0.1},
				{Timestamp: makeTimestamp(0, 10, 5), Value: 0.12},
				{Timestamp: makeTimestamp(0, 10, 10), Value: 0.11},
				{Timestamp: makeTimestamp(0, 10, 15), Value: 0.13},
				{Timestamp: makeTimestamp(0, 10, 20), Value: 0.1},
			},
			wantBurst: false,
			wantConfidence: func(c float64) bool {
				return c < 0.1
			},
			wantDataPoints: 5,
		},
		{
			name:      "seven days of data approaches confidence 1.0",
			samples:   generateSteadySamples(0.5, 7),
			wantBurst: false,
			wantConfidence: func(c float64) bool {
				return c >= 0.85
			},
			wantDataPoints: 7 * 24 * 12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := BuildProfile(tt.samples)

			assert.Equal(t, tt.wantDataPoints, profile.DataPoints, "data points mismatch")
			assert.Equal(t, tt.wantBurst, profile.BurstDetected, "burst detection mismatch")
			assert.True(t, tt.wantConfidence(profile.Confidence),
				"confidence %f did not meet expectation", profile.Confidence)

			if tt.checkHourly {
				// Verify all 24 hourly buckets are populated.
				for h := 0; h < 24; h++ {
					assert.Greater(t, profile.HourlyPercentiles[h].P50, 0.0,
						"hour %d P50 should be populated", h)
				}
			}

			if tt.wantDataPoints == 0 {
				assert.Equal(t, float64(0), profile.OverallPercentiles.P50)
				assert.Equal(t, float64(0), profile.TimeSpanDays)
			}
		})
	}
}

func TestBuildProfile_HourlyBucketing(t *testing.T) {
	// Create samples only at hours 0, 6, 12, 18 with distinct values.
	samples := []Sample{
		{Timestamp: makeTimestamp(0, 0, 0), Value: 10.0},
		{Timestamp: makeTimestamp(0, 0, 5), Value: 12.0},
		{Timestamp: makeTimestamp(0, 6, 0), Value: 20.0},
		{Timestamp: makeTimestamp(0, 6, 5), Value: 22.0},
		{Timestamp: makeTimestamp(0, 12, 0), Value: 30.0},
		{Timestamp: makeTimestamp(0, 12, 5), Value: 32.0},
		{Timestamp: makeTimestamp(0, 18, 0), Value: 40.0},
		{Timestamp: makeTimestamp(0, 18, 5), Value: 42.0},
	}

	profile := BuildProfile(samples)

	// Hour 0 should have values around 10-12.
	assert.InDelta(t, 11.0, profile.HourlyPercentiles[0].P50, 1.0)
	// Hour 12 should have values around 30-32.
	assert.InDelta(t, 31.0, profile.HourlyPercentiles[12].P50, 1.0)
	// Hour 18 should have values around 40-42.
	assert.InDelta(t, 41.0, profile.HourlyPercentiles[18].P50, 1.0)
	// Hours without data should be zero.
	assert.Equal(t, float64(0), profile.HourlyPercentiles[3].P50)
}

func TestBuildProfile_BurstMagnitude(t *testing.T) {
	// All values are 1.0 except one spike at 10.0.
	// With enough data, p95 should be near 1.0, and max = 10.0.
	// Ratio = 10.0 / 1.0 = 10, which is > 3.
	samples := make([]Sample, 0, 101)
	for i := 0; i < 100; i++ {
		samples = append(samples, Sample{
			Timestamp: makeTimestamp(0, i%24, (i/24)*5),
			Value:     1.0,
		})
	}
	samples = append(samples, Sample{
		Timestamp: makeTimestamp(0, 12, 30),
		Value:     10.0,
	})

	profile := BuildProfile(samples)

	assert.True(t, profile.BurstDetected)
	assert.Greater(t, profile.BurstMagnitude, 3.0)
}

func TestBuildProfile_AllNaNInfReturnsZeroProfile(t *testing.T) {
	// When every sample is NaN or Inf, validCount hits 0 after the
	// filtering loop (distinct from empty input which returns early
	// before the loop). The profile must be zero-valued and safe.
	samples := []Sample{
		{Timestamp: makeTimestamp(0, 0, 0), Value: math.NaN()},
		{Timestamp: makeTimestamp(0, 1, 0), Value: math.Inf(1)},
		{Timestamp: makeTimestamp(0, 2, 0), Value: math.Inf(-1)},
		{Timestamp: makeTimestamp(0, 3, 0), Value: math.NaN()},
	}

	profile := BuildProfile(samples)

	assert.Equal(t, 0, profile.DataPoints)
	assert.Equal(t, float64(0), profile.Confidence)
	assert.Equal(t, float64(0), profile.OverallPercentiles.P50)
	assert.Equal(t, float64(0), profile.OverallPercentiles.Max)
	assert.False(t, profile.BurstDetected)
}

func TestBuildProfile_NaNInfFiltered(t *testing.T) {
	// Mix valid samples with NaN and Inf. The profile should only
	// contain valid values; NaN/Inf must not corrupt percentiles.
	samples := []Sample{
		{Timestamp: makeTimestamp(0, 0, 0), Value: 100},
		{Timestamp: makeTimestamp(0, 1, 0), Value: math.NaN()},
		{Timestamp: makeTimestamp(0, 2, 0), Value: 200},
		{Timestamp: makeTimestamp(0, 3, 0), Value: math.Inf(1)},
		{Timestamp: makeTimestamp(0, 4, 0), Value: 300},
		{Timestamp: makeTimestamp(0, 5, 0), Value: math.Inf(-1)},
		{Timestamp: makeTimestamp(0, 6, 0), Value: 400},
	}

	profile := BuildProfile(samples)

	// Only 4 valid samples should be counted.
	assert.Equal(t, 4, profile.DataPoints)
	assert.False(t, math.IsNaN(profile.OverallPercentiles.P99))
	assert.False(t, math.IsInf(profile.OverallPercentiles.P99, 0))
}

func TestBuildProfile_TwoPassCountingCorrectness(t *testing.T) {
	// Verify the two-pass preallocation produces identical results to what
	// a naive grow-as-you-go implementation would produce. This guards the
	// optimization in BuildProfile that counts valid samples first, then
	// preallocates exact-sized slices.
	samples := []Sample{
		{Timestamp: makeTimestamp(0, 3, 0), Value: 10},
		{Timestamp: makeTimestamp(0, 3, 15), Value: 20},
		{Timestamp: makeTimestamp(0, 3, 30), Value: math.NaN()},
		{Timestamp: makeTimestamp(0, 9, 0), Value: 50},
		{Timestamp: makeTimestamp(0, 9, 30), Value: math.Inf(1)},
		{Timestamp: makeTimestamp(0, 15, 0), Value: 30},
		{Timestamp: makeTimestamp(0, 15, 15), Value: 40},
		{Timestamp: makeTimestamp(0, 15, 30), Value: 60},
	}

	profile := BuildProfile(samples)

	assert.Equal(t, 6, profile.DataPoints, "only valid (non-NaN/Inf) samples should be counted")

	// Hour 3 should have [10, 20].
	assert.InDelta(t, 15.0, profile.HourlyPercentiles[3].P50, 0.1)
	assert.InDelta(t, 20.0, profile.HourlyPercentiles[3].Max, 0.01)

	// Hour 9 should have [50] only (Inf filtered).
	assert.InDelta(t, 50.0, profile.HourlyPercentiles[9].P50, 0.01)

	// Hour 15 should have [30, 40, 60].
	assert.InDelta(t, 40.0, profile.HourlyPercentiles[15].P50, 0.1)
	assert.InDelta(t, 60.0, profile.HourlyPercentiles[15].Max, 0.01)

	// Overall percentiles should reflect all 6 valid values: [10,20,30,40,50,60].
	assert.InDelta(t, 60.0, profile.OverallPercentiles.Max, 0.01)
	assert.InDelta(t, 35.0, profile.OverallPercentiles.P50, 0.1) // median of [10,20,30,40,50,60]
}

func TestComputePercentiles(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		expect PercentileSet
	}{
		{
			name:   "empty input returns zero set",
			values: nil,
			expect: PercentileSet{},
		},
		{
			name:   "single value returns that value for all percentiles",
			values: []float64{42.0},
			expect: PercentileSet{P50: 42, P90: 42, P95: 42, P99: 42, Max: 42},
		},
		{
			name:   "two values interpolate correctly",
			values: []float64{10, 20},
			expect: PercentileSet{P50: 15, P90: 19, P95: 19.5, P99: 19.9, Max: 20},
		},
		{
			name:   "five values with known percentiles",
			values: []float64{1, 2, 3, 4, 5},
			// rank = p * (n-1): P50=2.0->3, P90=3.6->4.6, P95=3.8->4.8, P99=3.96->4.96
			expect: PercentileSet{P50: 3, P90: 4.6, P95: 4.8, P99: 4.96, Max: 5},
		},
		{
			name:   "unsorted input is sorted before computing",
			values: []float64{5, 1, 3, 2, 4},
			expect: PercentileSet{P50: 3, P90: 4.6, P95: 4.8, P99: 4.96, Max: 5},
		},
		{
			name:   "identical values return that value for all percentiles",
			values: []float64{7, 7, 7, 7},
			expect: PercentileSet{P50: 7, P90: 7, P95: 7, P99: 7, Max: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computePercentiles(tt.values)
			assert.InDelta(t, tt.expect.P50, result.P50, 0.01, "P50")
			assert.InDelta(t, tt.expect.P90, result.P90, 0.01, "P90")
			assert.InDelta(t, tt.expect.P95, result.P95, 0.01, "P95")
			assert.InDelta(t, tt.expect.P99, result.P99, 0.01, "P99")
			assert.InDelta(t, tt.expect.Max, result.Max, 0.01, "Max")
		})
	}
}

func TestPercentile_EmptySlice(t *testing.T) {
	assert.Equal(t, 0.0, percentile(nil, 0.5))
	assert.Equal(t, 0.0, percentile([]float64{}, 0.95))
}

func TestPercentile_ExactBoundary(t *testing.T) {
	// When rank falls exactly on an index (lower == upper), no interpolation
	// should occur. With 11 values [0..10], P50 rank = 0.5*10 = 5.0 (exact).
	sorted := make([]float64, 11)
	for i := range sorted {
		sorted[i] = float64(i)
	}
	assert.InDelta(t, 5.0, percentile(sorted, 0.50), 0.001)
	assert.InDelta(t, 10.0, percentile(sorted, 1.0), 0.001)
	assert.InDelta(t, 0.0, percentile(sorted, 0.0), 0.001)
}
