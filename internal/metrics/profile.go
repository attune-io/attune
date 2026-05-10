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
	"sort"
)

// PercentileSet holds a standard set of percentile values computed from
// a collection of samples.
type PercentileSet struct {
	P50 float64
	P90 float64
	P95 float64
	P99 float64
	Max float64
}

// UsageProfile summarizes resource usage over a time period, providing
// per-hour and overall percentile breakdowns along with burst detection
// and a confidence score.
type UsageProfile struct {
	HourlyPercentiles  [24]PercentileSet
	OverallPercentiles PercentileSet
	BurstDetected      bool
	BurstMagnitude     float64
	DataPoints         int
	TimeSpanDays       float64
	Confidence         float64
}

// BuildProfile constructs a UsageProfile from the provided samples.
// Samples are bucketed by hour of day (0-23) for hourly percentiles,
// and also aggregated for overall percentiles. Burst detection flags
// cases where the max value exceeds 3x the p95.
func BuildProfile(samples []Sample) UsageProfile {
	if len(samples) == 0 {
		return UsageProfile{}
	}

	// Bucket samples by hour of day.
	hourBuckets := [24][]float64{}
	allValues := make([]float64, 0, len(samples))

	minTime, maxTime := samples[0].Timestamp, samples[0].Timestamp
	for _, s := range samples {
		hour := s.Timestamp.Hour()
		hourBuckets[hour] = append(hourBuckets[hour], s.Value)
		allValues = append(allValues, s.Value)

		if s.Timestamp.Before(minTime) {
			minTime = s.Timestamp
		}
		if s.Timestamp.After(maxTime) {
			maxTime = s.Timestamp
		}
	}

	profile := UsageProfile{
		DataPoints: len(samples),
	}

	// Calculate time span in days.
	profile.TimeSpanDays = maxTime.Sub(minTime).Hours() / 24.0

	// Calculate hourly percentiles.
	for h := 0; h < 24; h++ {
		if len(hourBuckets[h]) > 0 {
			profile.HourlyPercentiles[h] = computePercentiles(hourBuckets[h])
		}
	}

	// Calculate overall percentiles.
	profile.OverallPercentiles = computePercentiles(allValues)

	// Detect bursts: max > 3x p95.
	if profile.OverallPercentiles.P95 > 0 {
		ratio := profile.OverallPercentiles.Max / profile.OverallPercentiles.P95
		if ratio > 3.0 {
			profile.BurstDetected = true
			profile.BurstMagnitude = ratio
		}
	}

	// Compute confidence: min(timeSpanDays, sqrt(dataPoints/24)) / 7, clamped to [0, 1].
	timeComponent := profile.TimeSpanDays
	dataComponent := math.Sqrt(float64(profile.DataPoints) / 24.0)
	raw := math.Min(timeComponent, dataComponent) / 7.0
	profile.Confidence = math.Max(0, math.Min(1, raw))

	return profile
}

// computePercentiles sorts the values and computes p50, p90, p95, p99, and max.
func computePercentiles(values []float64) PercentileSet {
	if len(values) == 0 {
		return PercentileSet{}
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	return PercentileSet{
		P50: percentile(sorted, 0.50),
		P90: percentile(sorted, 0.90),
		P95: percentile(sorted, 0.95),
		P99: percentile(sorted, 0.99),
		Max: sorted[len(sorted)-1],
	}
}

// percentile returns the value at the given percentile (0.0-1.0) from a
// sorted slice using linear interpolation.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}

	rank := p * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))

	if lower == upper {
		return sorted[lower]
	}

	fraction := rank - float64(lower)
	return sorted[lower]*(1-fraction) + sorted[upper]*fraction
}
