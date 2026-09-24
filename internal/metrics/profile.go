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

// DefaultMaxProfileSamples is the default cap on samples passed to BuildProfile.
// Beyond this, DownsampleSamples keeps temporal coverage while bounding CPU.
const DefaultMaxProfileSamples = 10000

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

// DownsampleSamples returns at most maxN samples. Each output point is the
// midpoint of one time-ordered window, so percentiles stay near the original
// distribution. The global maximum replaces the midpoint of its own window,
// so a short spike still reaches burst detection. When maxN <= 0 or
// len(samples) <= maxN, the original slice is returned unchanged.
// Already-sorted input is not copied.
func DownsampleSamples(samples []Sample, maxN int) []Sample {
	if maxN <= 0 || len(samples) <= maxN {
		return samples
	}
	sorted := samples
	if !samplesTimeSorted(samples) {
		sorted = make([]Sample, len(samples))
		copy(sorted, samples)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Timestamp.Before(sorted[j].Timestamp)
		})
	}
	if maxN == 1 {
		return []Sample{maxSample(sorted)}
	}
	n := len(sorted)
	maxIdx := 0
	for i := 1; i < n; i++ {
		if sorted[i].Value > sorted[maxIdx].Value {
			maxIdx = i
		}
	}
	out := make([]Sample, 0, maxN)
	maxWindow := -1
	for i := 0; i < maxN; i++ {
		start := i * n / maxN
		end := (i + 1) * n / maxN
		if end <= start {
			end = start + 1
		}
		if end > n {
			end = n
		}
		mid := start + (end-start)/2
		out = append(out, sorted[mid])
		if maxIdx >= start && maxIdx < end {
			maxWindow = len(out) - 1
		}
	}
	if maxWindow >= 0 {
		out[maxWindow] = sorted[maxIdx]
	}
	return out
}

func samplesTimeSorted(samples []Sample) bool {
	for i := 1; i < len(samples); i++ {
		if samples[i].Timestamp.Before(samples[i-1].Timestamp) {
			return false
		}
	}
	return true
}

func maxSample(samples []Sample) Sample {
	best := samples[0]
	for _, s := range samples[1:] {
		if s.Value > best.Value {
			best = s
		}
	}
	return best
}

// BuildProfile constructs a UsageProfile from the provided samples.
// Samples are bucketed by hour of day (0-23) for hourly percentiles,
// and also aggregated for overall percentiles. Burst detection flags
// cases where the max value exceeds 3x the p95.
// Callers with large N should DownsampleSamples first.
func BuildProfile(samples []Sample) UsageProfile {
	if len(samples) == 0 {
		return UsageProfile{}
	}

	var hourCounts [24]int
	minTime, maxTime := samples[0].Timestamp, samples[0].Timestamp
	validCount := 0
	for _, s := range samples {
		// Filter out NaN/Inf samples that can corrupt percentile computation.
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		validCount++
		hourCounts[s.Timestamp.Hour()]++

		if s.Timestamp.Before(minTime) {
			minTime = s.Timestamp
		}
		if s.Timestamp.After(maxTime) {
			maxTime = s.Timestamp
		}
	}
	if validCount == 0 {
		return UsageProfile{}
	}

	// Pre-size buckets exactly once to avoid repeated slice growth while
	// building the profile for recommendation hot paths.
	hourBuckets := [24][]float64{}
	for h, count := range hourCounts {
		if count > 0 {
			hourBuckets[h] = make([]float64, count)
		}
	}
	allValues := make([]float64, validCount)
	var hourOffsets [24]int
	allOffset := 0
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		hour := s.Timestamp.Hour()
		hourBuckets[hour][hourOffsets[hour]] = s.Value
		hourOffsets[hour]++
		allValues[allOffset] = s.Value
		allOffset++
	}

	profile := UsageProfile{
		DataPoints: validCount,
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

// computePercentiles sorts values in place and computes p50, p90, p95, p99,
// and max. Callers must not use the slice after this call.
func computePercentiles(values []float64) PercentileSet {
	if len(values) == 0 {
		return PercentileSet{}
	}

	sort.Float64s(values)

	return PercentileSet{
		P50: percentile(values, 0.50),
		P90: percentile(values, 0.90),
		P95: percentile(values, 0.95),
		P99: percentile(values, 0.99),
		Max: values[len(values)-1],
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
