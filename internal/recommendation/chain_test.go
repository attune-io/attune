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

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SebTardif/kube-rightsize/internal/metrics"
)

// buildRealisticCPUProfile creates a profile simulating realistic CPU usage
// with the given overall P95 value and confidence level.
func buildRealisticCPUProfile(p95 float64, confidence float64) metrics.UsageProfile {
	ps := metrics.PercentileSet{
		P50: p95 * 0.5,
		P90: p95 * 0.9,
		P95: p95,
		P99: p95 * 1.1,
		Max: p95 * 1.2,
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         confidence,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}

func TestRecommendationEngine_RealisticCPU(t *testing.T) {
	engine := NewEngine(
		95,                          // percentile
		1.2,                         // safety margin
		resource.MustParse("50m"),   // min bound
		resource.MustParse("4000m"), // max bound
		50,                          // max change percent
	)

	// Profile with P95 = 0.200 (200m CPU), high confidence.
	profile := buildRealisticCPUProfile(0.200, 0.95)
	current := resource.MustParse("500m")

	recommended, changed := engine.Recommend(profile, current)
	assert.True(t, changed)
	// Chain: P95=200m → margin(1.2)=240m → confidence(~4.2x)=~1011m
	//   → bounds OK → change filter: 1011m vs 500m = 102% > 50%
	//   → capped at 500m + 250m = 750m.
	assert.Equal(t, int64(750), recommended.MilliValue(),
		"50%% max increase from 500m should cap at 750m")
}

func TestRecommendationEngine_RealisticMemory(t *testing.T) {
	engine := NewEngine(
		99,                         // percentile
		1.2,                        // safety margin
		resource.MustParse("64Mi"), // min bound
		resource.MustParse("8Gi"),  // max bound
		50,                         // max change percent
	)

	// Memory profile: P99 = 256Mi = 268435456 bytes.
	memP99 := 268435456.0
	ps := metrics.PercentileSet{
		P50: memP99 * 0.5,
		P90: memP99 * 0.9,
		P95: memP99 * 0.95,
		P99: memP99,
		Max: memP99 * 1.1,
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

	current := resource.MustParse("512Mi")

	recommended, changed := engine.Recommend(profile, current)
	assert.True(t, changed)
	assert.Greater(t, recommended.MilliValue(), int64(0))
	t.Logf("Memory recommendation: %s (from current %s)", recommended.String(), current.String())
}

func TestRecommendationEngine_SmallChangeFiltered(t *testing.T) {
	engine := NewEngine(
		95,
		1.0, // no margin
		resource.MustParse("10m"),
		resource.MustParse("10000m"),
		50,
	)

	// Design a profile where the full chain (percentile -> margin ->
	// confidence -> bounds) produces a value within 10% of current,
	// so the ChangeFilter's MinChangePercent (10%) rejects it.
	// With confidence=1.0: factor = (1+1/1)^2 = 4.
	// P95=0.1275 → 128m → margin(1.0)=128m → confidence(4x)=512m → bounds OK.
	// Change from current 500m: (512-500)/500 = 2.4% < 10% → filtered.
	ps := metrics.PercentileSet{
		P50: 0.060,
		P90: 0.100,
		P95: 0.1275,
		P99: 0.135,
		Max: 0.150,
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         10000,
		TimeSpanDays:       14,
		Confidence:         1.0,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}

	current := resource.MustParse("500m")
	recommended, changed := engine.Recommend(profile, current)

	assert.False(t, changed, "change <10%% should be filtered out")
	assert.Equal(t, current.MilliValue(), recommended.MilliValue(),
		"filtered recommendation should equal current")
}

func TestRecommendationEngine_LargeChangeCapped(t *testing.T) {
	engine := NewEngine(
		95,
		1.2,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50, // max 50% change
	)

	// Profile where recommended would be much higher than current.
	// Chain: P95=2000m → margin(1.2)=2400m → confidence(~4.2x)=~10112m
	//   → bounds(4000m) → change filter: 4000m vs 200m = 1900% > 50%
	//   → capped at 200m + 100m = 300m.
	profile := buildRealisticCPUProfile(2.0, 0.95)
	current := resource.MustParse("200m")

	recommended, changed := engine.Recommend(profile, current)
	assert.True(t, changed)
	assert.Equal(t, int64(300), recommended.MilliValue(),
		"50%% max increase from 200m should cap at 300m")
}

func TestRecommendationEngine_HighVsLowConfidence(t *testing.T) {
	makeEngine := func() *RecommendationEngine {
		return NewEngine(
			95,
			1.2,
			resource.MustParse("50m"),
			resource.MustParse("100000m"),
			100, // allow full range of change so filter doesn't mask differences
		)
	}

	// Use current close to expected recommendation so change filter doesn't interfere.
	current := resource.MustParse("250m")

	highConfProfile := buildRealisticCPUProfile(0.200, 0.95)
	lowConfProfile := buildRealisticCPUProfile(0.200, 0.15)

	highRec, _ := makeEngine().Recommend(highConfProfile, current)
	lowRec, _ := makeEngine().Recommend(lowConfProfile, current)

	// Low confidence should produce a higher (more conservative) recommendation.
	assert.GreaterOrEqual(t, lowRec.MilliValue(), highRec.MilliValue(),
		"low confidence should produce >= recommendation than high confidence")
	t.Logf("High confidence: %s, Low confidence: %s", highRec.String(), lowRec.String())
}

func TestRecommendationEngine_BurstyProfile(t *testing.T) {
	engine := NewEngine(
		95,
		1.2,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50,
	)

	// Bursty profile: P95 is low but max is very high.
	ps := metrics.PercentileSet{
		P50: 0.050,
		P90: 0.080,
		P95: 0.100,
		P99: 0.200,
		Max: 1.000, // 10x P95, clear burst
	}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		BurstDetected:      true,
		BurstMagnitude:     10.0,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}

	// Set current near the expected recommendation range so the change filter
	// doesn't reject it as too small.
	current := resource.MustParse("50m")
	recommended, _ := engine.Recommend(profile, current)

	// The recommendation should be positive and at least the min bound.
	assert.GreaterOrEqual(t, recommended.MilliValue(), int64(50))
	t.Logf("Bursty profile: recommended=%s, current=%s", recommended.String(), current.String())
}
