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
	"pgregory.net/rapid"

	"github.com/attune-io/attune/internal/metrics"
)

func genCRDPercentile(t *rapid.T) int {
	return rapid.SampledFrom([]int{50, 90, 95, 99}).Draw(t, "percentile")
}

func genMonotoneProfile(t *rapid.T, usage float64) metrics.UsageProfile {
	p50 := usage * 0.5
	p90 := usage * 0.9
	p95 := usage * 0.95
	p99 := usage * 0.99
	ps := metrics.PercentileSet{P50: p50, P90: p90, P95: p95, P99: p99, Max: usage}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         1000,
		TimeSpanDays:       7,
		Confidence:         1.0,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}

func TestProperty_RecommendMatchesExplanation(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		usage := rapid.Float64Range(0.05, 4).Draw(rt, "usage")
		currentCores := rapid.Float64Range(0.05, 4).Draw(rt, "current")
		overhead := rapid.Float64Range(0, 50).Draw(rt, "overhead")
		pct := genCRDPercentile(rt)
		eng := NewEngine(pct, overhead,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			100, 100, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		profile := genMonotoneProfile(rt, usage)
		current := *resource.NewMilliQuantity(int64(currentCores*1000), resource.DecimalSI)
		rec, _ := eng.Recommend(profile, current)
		_, expl, _ := eng.RecommendWithExplanation(profile, current)
		if rec.Cmp(expl.Final) != 0 {
			rt.Fatalf("Recommend %s != explanation.Final %s", rec.String(), expl.Final.String())
		}
	})
}

func TestProperty_HigherUsageDoesNotLowerRecommendation(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		low := rapid.Float64Range(0.05, 2).Draw(rt, "lowUsage")
		high := rapid.Float64Range(low, 4).Draw(rt, "highUsage")
		currentCores := rapid.Float64Range(0.2, 4).Draw(rt, "current")
		overhead := rapid.Float64Range(0, 30).Draw(rt, "overhead")
		pct := genCRDPercentile(rt)
		eng := NewEngine(pct, overhead,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			500, 500, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		current := *resource.NewMilliQuantity(int64(currentCores*1000), resource.DecimalSI)
		lowRec, _ := eng.Recommend(genMonotoneProfile(rt, low), current)
		highRec, _ := eng.Recommend(genMonotoneProfile(rt, high), current)
		if highRec.Cmp(lowRec) < 0 {
			rt.Fatalf("usage %v→%s then %v→%s decreased", low, lowRec.String(), high, highRec.String())
		}
	})
}

func TestProperty_HigherOverheadDoesNotLowerRecommendation(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		usage := rapid.Float64Range(0.1, 2).Draw(rt, "usage")
		lowOH := rapid.Float64Range(0, 20).Draw(rt, "lowOH")
		highOH := rapid.Float64Range(lowOH, 80).Draw(rt, "highOH")
		current := resource.MustParse("500m")
		pct := genCRDPercentile(rt)
		lowEng := NewEngine(pct, lowOH,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			500, 500, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		highEng := NewEngine(pct, highOH,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			500, 500, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		profile := genMonotoneProfile(rt, usage)
		lowRec, _ := lowEng.Recommend(profile, current)
		highRec, _ := highEng.Recommend(profile, current)
		if highRec.Cmp(lowRec) < 0 {
			rt.Fatalf("overhead %v→%s then %v→%s decreased", lowOH, lowRec.String(), highOH, highRec.String())
		}
	})
}

func TestProperty_HigherPercentileDoesNotLowerRecommendation(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		usage := rapid.Float64Range(0.2, 3).Draw(rt, "usage")
		current := resource.MustParse("500m")
		overhead := rapid.Float64Range(0, 20).Draw(rt, "overhead")
		ordered := []int{50, 90, 95, 99}
		i := rapid.IntRange(0, 2).Draw(rt, "lowIdx")
		j := rapid.IntRange(i, 3).Draw(rt, "highIdx")
		lowEng := NewEngine(ordered[i], overhead,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			500, 500, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		highEng := NewEngine(ordered[j], overhead,
			resource.MustParse("10m"), resource.MustParse("8000m"),
			500, 500, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
		profile := genMonotoneProfile(rt, usage)
		lowRec, _ := lowEng.Recommend(profile, current)
		highRec, _ := highEng.Recommend(profile, current)
		if highRec.Cmp(lowRec) < 0 {
			rt.Fatalf("percentile %d→%s then %d→%s decreased", ordered[i], lowRec.String(), ordered[j], highRec.String())
		}
	})
}

func TestProperty_BoundsAreFixpoint(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		minMilli := rapid.Int64Range(1, 2000).Draw(rt, "min")
		maxMilli := rapid.Int64Range(minMilli, 8000).Draw(rt, "max")
		qMilli := rapid.Int64Range(1, 10000).Draw(rt, "q")
		min := *resource.NewMilliQuantity(minMilli, resource.DecimalSI)
		max := *resource.NewMilliQuantity(maxMilli, resource.DecimalSI)
		q := *resource.NewMilliQuantity(qMilli, resource.DecimalSI)
		once, _ := applyBounds(q, min, max)
		twice, _ := applyBounds(once, min, max)
		if once.Cmp(twice) != 0 {
			rt.Fatalf("applyBounds not fixpoint: %s then %s", once.String(), twice.String())
		}
		if once.Cmp(min) < 0 || once.Cmp(max) > 0 {
			rt.Fatalf("clamped %s outside [%s, %s]", once.String(), min.String(), max.String())
		}
	})
}
