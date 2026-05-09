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

	"github.com/SebTardif/kube-rightsize/internal/metrics"
	"k8s.io/apimachinery/pkg/api/resource"
)

// buildTestProfile creates a UsageProfile with the given usage value
// spread uniformly across all 24 hourly buckets. Percentile values are
// derived as fractions of the usage value.
func buildTestProfile(usage float64) metrics.UsageProfile {
	ps := metrics.PercentileSet{
		P50: usage * 0.5,
		P90: usage * 0.9,
		P95: usage * 0.95,
		P99: usage * 0.99,
		Max: usage,
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         1000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}

func FuzzPercentileEstimator(f *testing.F) {
	// Seed corpus with reasonable values.
	f.Add(0.1, 0.5, 95)

	f.Fuzz(func(t *testing.T, p50, p95 float64, percentile int) {
		if percentile < 50 || percentile > 99 {
			t.Skip()
		}
		if p50 < 0 || p95 < 0 || p50 > 1000 || p95 > 1000 {
			t.Skip()
		}

		// Build a profile from the fuzzed inputs.
		profile := metrics.UsageProfile{
			OverallPercentiles: metrics.PercentileSet{
				P50: p50,
				P90: (p50 + p95) / 2,
				P95: p95,
				P99: p95 * 1.1,
				Max: p95 * 1.5,
			},
			DataPoints:   1000,
			TimeSpanDays: 7,
			Confidence:   0.95,
		}
		for h := 0; h < 24; h++ {
			profile.HourlyPercentiles[h] = profile.OverallPercentiles
		}

		est := &PercentileEstimator{Percentile: percentile}
		current := resource.MustParse("500m")
		result := est.Estimate(profile, current)

		// The result must never be negative.
		if result.Cmp(resource.MustParse("0")) < 0 {
			t.Errorf("negative result: %v", result)
		}
	})
}

func FuzzRecommendationEngine(f *testing.F) {
	// Seed corpus: (usage, current, margin, percentile).
	f.Add(0.1, 0.5, 1.2, 95)

	f.Fuzz(func(t *testing.T, usage, current, margin float64, percentile int) {
		// Validate inputs.
		if percentile < 50 || percentile > 99 || margin < 1.0 || margin > 5.0 {
			t.Skip()
		}
		if usage < 0 || current <= 0 || usage > 100 || current > 100 {
			t.Skip()
		}

		engine := NewEngine(percentile, margin,
			resource.MustParse("10m"), resource.MustParse("100000m"), 100)

		profile := buildTestProfile(usage)
		currentQ := resource.NewMilliQuantity(int64(current*1000), resource.DecimalSI)

		// Ensure current is at or above the minimum bound so the change
		// filter returning current unchanged does not violate bounds.
		if currentQ.Cmp(resource.MustParse("10m")) < 0 {
			t.Skip()
		}

		rec, _ := engine.Recommend(profile, *currentQ)

		// Result must be within the configured bounds.
		if rec.Cmp(resource.MustParse("10m")) < 0 {
			t.Errorf("result %v below minimum bound 10m", rec)
		}
		if rec.Cmp(resource.MustParse("100000m")) > 0 {
			t.Errorf("result %v above maximum bound 100000m", rec)
		}
	})
}
