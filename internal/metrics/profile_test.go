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
			name:    "empty samples returns zero profile",
			samples: nil,
			wantBurst: false,
			wantConfidence: func(c float64) bool {
				return c == 0
			},
			wantDataPoints: 0,
		},
		{
			name:    "steady usage has no bursts and high confidence",
			samples: generateSteadySamples(0.1, 7), // 100m CPU for 7 days
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
			name:    "seven days of data approaches confidence 1.0",
			samples: generateSteadySamples(0.5, 7),
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
