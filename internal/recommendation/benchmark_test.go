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

package recommendation

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/attune-io/attune/internal/metrics"
)

func BenchmarkPercentileEstimator(b *testing.B) {
	est := &PercentileEstimator{Percentile: 95}
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		est.Estimate(profile, current)
	}
}

func BenchmarkFullChain(b *testing.B) {
	engine := NewEngine(95, 20.0,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50, 50,
	)
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		engine.Recommend(profile, current)
	}
}

func BenchmarkMarginEstimator(b *testing.B) {
	inner := &PercentileEstimator{Percentile: 95}
	est := &marginEstimator{factor: 1.2, inner: inner}
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		est.Estimate(profile, current)
	}
}

func BenchmarkConfidenceEstimator(b *testing.B) {
	inner := &PercentileEstimator{Percentile: 95}
	margin := &marginEstimator{factor: 1.2, inner: inner}
	est := &confidenceEstimator{multiplier: 1.0, exponent: 2.0, inner: margin}
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		est.Estimate(profile, current)
	}
}

// BenchmarkFullChain_HistoryWindows benchmarks the full recommendation chain
// with profiles matching real history windows. The chain cost is dominated
// by the estimator math, not profile building, so this tests whether
// confidence scaling and bounds checks degrade with larger histories.
func BenchmarkFullChain_HistoryWindows(b *testing.B) {
	cases := []struct {
		name       string
		dataPoints int
		days       int
	}{
		{"1d_288dp", 288, 1},
		{"7d_2016dp", 2016, 7},
		{"14d_4032dp", 4032, 14},
		{"30d_8640dp", 8640, 30},
	}
	engine := NewEngine(95, 20.0,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50, 50,
	)
	current := resource.MustParse("500m")

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			profile := buildBenchProfileWithSize(tc.dataPoints, tc.days)
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				engine.Recommend(profile, current)
			}
		})
	}
}

// BenchmarkFullChain_Percentiles benchmarks the chain across all supported
// percentile values to verify no percentile has disproportionate cost.
func BenchmarkFullChain_Percentiles(b *testing.B) {
	for _, p := range []int{50, 90, 95, 99} {
		b.Run(fmt.Sprintf("P%d", p), func(b *testing.B) {
			engine := NewEngine(p, 20.0,
				resource.MustParse("50m"),
				resource.MustParse("4000m"),
				50, 50,
			)
			profile := buildBenchProfile()
			current := resource.MustParse("500m")
			b.ResetTimer()
			for b.Loop() {
				engine.Recommend(profile, current)
			}
		})
	}
}

func buildBenchProfile() metrics.UsageProfile {
	return buildBenchProfileWithSize(5000, 7)
}

func buildBenchProfileWithSize(dataPoints, days int) metrics.UsageProfile {
	ps := metrics.PercentileSet{
		P50: 0.100,
		P90: 0.180,
		P95: 0.200,
		P99: 0.250,
		Max: 0.400,
	}
	confidence := float64(dataPoints) / 5000.0
	if confidence > 1.0 {
		confidence = 1.0
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         dataPoints,
		TimeSpanDays:       float64(days),
		Confidence:         confidence,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}
