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

	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
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
		EngineOpts{IsCPU: true},
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
		EngineOpts{IsCPU: true},
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
		EngineOpts{IsCPU: true},
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
			EngineOpts{IsCPU: true},
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
		EngineOpts{IsCPU: true},
	)

	// Profile with moderate usage so the burst boost is visible after the
	// change filter's 10% minimum threshold.
	ps := metrics.PercentileSet{
		P50: 0.200,
		P90: 0.350,
		P95: 0.400,
		P99: 0.600,
		Max: 4.000, // 10x P95, clear burst
	}
	burstyProfile := metrics.UsageProfile{
		OverallPercentiles: ps,
		BurstDetected:      true,
		BurstMagnitude:     10.0,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		burstyProfile.HourlyPercentiles[h] = ps
	}

	calmProfile := burstyProfile
	calmProfile.BurstDetected = false
	calmProfile.BurstMagnitude = 0

	current := resource.MustParse("200m")
	_, burstyExplain, _ := engine.RecommendWithExplanation(burstyProfile, current)
	_, calmExplain, _ := engine.RecommendWithExplanation(calmProfile, current)

	// The burst factor should be > 1.0 for the bursty profile.
	assert.Greater(t, burstyExplain.BurstFactor, 1.0,
		"bursty profile should have burst factor > 1.0")
	assert.Equal(t, 1.0, calmExplain.BurstFactor,
		"calm profile should have burst factor == 1.0")

	// The post-burst value should be higher than the post-safety-margin value.
	assert.Greater(t, burstyExplain.AfterBurst.MilliValue(), burstyExplain.AfterSafetyMargin.MilliValue(),
		"burst should increase the value above safety margin")

	// The calm profile's post-burst should equal its post-safety-margin.
	assert.Equal(t, calmExplain.AfterSafetyMargin.MilliValue(), calmExplain.AfterBurst.MilliValue(),
		"no burst should leave the value unchanged after safety margin")

	t.Logf("Bursty afterBurst=%s (factor=%.3f), Calm afterBurst=%s (factor=%.3f)",
		burstyExplain.AfterBurst.String(), burstyExplain.BurstFactor,
		calmExplain.AfterBurst.String(), calmExplain.BurstFactor)
}

func TestRecommendationEngine_BurstExplanation(t *testing.T) {
	engine := NewEngine(
		95,
		1.2,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50,
		EngineOpts{IsCPU: true},
	)

	ps := metrics.PercentileSet{P50: 0.1, P90: 0.15, P95: 0.2, P99: 0.3, Max: 2.0}
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

	_, explanation, _ := engine.RecommendWithExplanation(profile, resource.MustParse("100m"))

	assert.Greater(t, explanation.BurstFactor, 1.0,
		"burst factor should be > 1.0 for bursty profile")
	assert.True(t, explanation.AfterBurst.Cmp(explanation.AfterSafetyMargin) > 0,
		"afterBurst should be greater than afterSafetyMargin when burst is active")
}

func TestRecommendationEngine_NoBurstExplanation(t *testing.T) {
	engine := NewEngine(95, 1.2, resource.MustParse("50m"), resource.MustParse("4000m"), 50, EngineOpts{IsCPU: true})

	ps := metrics.PercentileSet{P50: 0.1, P90: 0.15, P95: 0.2, P99: 0.3, Max: 0.5}
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		BurstDetected:      false,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}

	_, explanation, _ := engine.RecommendWithExplanation(profile, resource.MustParse("100m"))

	assert.Equal(t, 1.0, explanation.BurstFactor,
		"burst factor should be 1.0 when no burst detected")
	assert.Equal(t, explanation.AfterSafetyMargin.MilliValue(), explanation.AfterBurst.MilliValue(),
		"afterBurst should equal afterSafetyMargin when no burst")
}

func TestRecommendationEngine_BurstMagnitudeBoundary(t *testing.T) {
	engine := NewEngine(95, 1.2, resource.MustParse("50m"), resource.MustParse("4000m"), 50, EngineOpts{IsCPU: true})

	ps := metrics.PercentileSet{P50: 0.1, P90: 0.15, P95: 0.2, P99: 0.3, Max: 0.6}

	// BurstDetected=true but BurstMagnitude exactly 1.0 -- condition is > 1, so no boost.
	profile := metrics.UsageProfile{
		OverallPercentiles: ps,
		BurstDetected:      true,
		BurstMagnitude:     1.0,
		DataPoints:         5000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}

	_, explanation, _ := engine.RecommendWithExplanation(profile, resource.MustParse("100m"))

	assert.Equal(t, 1.0, explanation.BurstFactor,
		"burst factor should be 1.0 when magnitude is exactly 1.0")
	assert.Equal(t, explanation.AfterSafetyMargin.MilliValue(), explanation.AfterBurst.MilliValue(),
		"no boost when magnitude is at boundary")
}

func TestRecommendationEngine_BurstSensitivityZero(t *testing.T) {
	zero := 0.0
	engine := NewEngine(95, 1.2, resource.MustParse("50m"), resource.MustParse("4000m"), 50,
		EngineOpts{IsCPU: true, BurstSensitivity: &zero})

	ps := metrics.PercentileSet{P50: 0.1, P90: 0.15, P95: 0.2, P99: 0.3, Max: 2.0}
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

	_, explanation, _ := engine.RecommendWithExplanation(profile, resource.MustParse("100m"))

	assert.Equal(t, 1.0, explanation.BurstFactor,
		"sensitivity=0 should disable burst boost entirely")
	assert.Equal(t, explanation.AfterSafetyMargin.MilliValue(), explanation.AfterBurst.MilliValue(),
		"no boost when sensitivity is zero")
}

func TestRecommendationEngine_BurstSensitivityCustom(t *testing.T) {
	defaultSens := 0.1
	doubleSens := 0.2
	defaultEngine := NewEngine(95, 1.2, resource.MustParse("50m"), resource.MustParse("4000m"), 100,
		EngineOpts{IsCPU: true, BurstSensitivity: &defaultSens})
	doubleEngine := NewEngine(95, 1.2, resource.MustParse("50m"), resource.MustParse("4000m"), 100,
		EngineOpts{IsCPU: true, BurstSensitivity: &doubleSens})

	ps := metrics.PercentileSet{P50: 0.1, P90: 0.15, P95: 0.2, P99: 0.3, Max: 2.0}
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

	_, defExpl, _ := defaultEngine.RecommendWithExplanation(profile, resource.MustParse("100m"))
	_, dblExpl, _ := doubleEngine.RecommendWithExplanation(profile, resource.MustParse("100m"))

	// Double sensitivity should produce a higher burst factor.
	assert.Greater(t, dblExpl.BurstFactor, defExpl.BurstFactor,
		"sensitivity=0.2 should produce higher burst factor than 0.1")
	assert.Greater(t, dblExpl.AfterBurst.MilliValue(), defExpl.AfterBurst.MilliValue(),
		"higher sensitivity should increase the burst-adjusted value")
}

func TestRecommendationEngine_ExplainChain(t *testing.T) {
	engine := NewEngine(
		95,
		1.2,
		resource.MustParse("50m"),
		resource.MustParse("4000m"),
		50,
		EngineOpts{IsCPU: true},
	)
	profile := buildRealisticCPUProfile(0.200, 0.95)
	current := resource.MustParse("500m")

	recommended, explanation, changed := engine.RecommendWithExplanation(profile, current)
	assert.True(t, changed)
	assert.Equal(t, recommended.String(), explanation.Final.String())
	assert.Equal(t, int64(200), explanation.RawPercentile.MilliValue())
	assert.Equal(t, int64(240), explanation.AfterSafetyMargin.MilliValue())
	assert.InDelta(t, 4.2133, explanation.ConfidenceFactor, 0.0001)
	assert.Equal(t, "max_change_capped", explanation.ChangeFilterApplied)
	assert.Equal(t, int64(750), explanation.AfterChangeFilter.MilliValue())
}
