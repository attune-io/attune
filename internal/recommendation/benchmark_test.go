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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
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
	engine := NewEngine(95, 1.2,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50,
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
	est := &MarginEstimator{Factor: 1.2, Inner: inner}
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		est.Estimate(profile, current)
	}
}

func BenchmarkConfidenceEstimator(b *testing.B) {
	inner := &PercentileEstimator{Percentile: 95}
	margin := &MarginEstimator{Factor: 1.2, Inner: inner}
	est := &ConfidenceEstimator{Multiplier: 1.0, Exponent: 2.0, Inner: margin}
	profile := buildBenchProfile()
	current := resource.MustParse("500m")

	b.ResetTimer()
	for b.Loop() {
		est.Estimate(profile, current)
	}
}

func buildBenchProfile() metrics.UsageProfile {
	ps := metrics.PercentileSet{
		P50: 0.100,
		P90: 0.180,
		P95: 0.200,
		P99: 0.250,
		Max: 0.400,
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}
