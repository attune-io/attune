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
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestChangeFilter(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		innerValue  string
		maxPct      float64
		wantMillis  int64
		wantApplied string
	}{
		{
			name:        "small change below threshold returns current",
			current:     "1000m",
			innerValue:  "1050m",
			maxPct:      50,
			wantMillis:  1000,
			wantApplied: "min_change_filtered",
		},
		{
			name:        "large increase above max caps at max percent",
			current:     "1000m",
			innerValue:  "1800m",
			maxPct:      50,
			wantMillis:  1500,
			wantApplied: "max_change_capped",
		},
		{
			name:       "change within range passes through",
			current:    "1000m",
			innerValue: "1200m",
			maxPct:     50,
			wantMillis: 1200,
		},
		{
			name:       "decrease within range passes through",
			current:    "1000m",
			innerValue: "800m",
			maxPct:     50,
			wantMillis: 800,
		},
		{
			name:        "large decrease above max caps at max percent",
			current:     "1000m",
			innerValue:  "300m",
			maxPct:      50,
			wantMillis:  500,
			wantApplied: "max_change_capped",
		},
		{
			name:        "small decrease below threshold returns current",
			current:     "1000m",
			innerValue:  "960m",
			maxPct:      50,
			wantMillis:  1000,
			wantApplied: "min_change_filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, expl := recommendCPUThroughEngine(t, tt.current, tt.innerValue, tt.maxPct)
			assert.Equal(t, tt.wantMillis, rec.MilliValue())
			assert.Equal(t, tt.wantApplied, expl.ChangeFilterApplied)
		})
	}
}

func TestChangeFilter_BinarySIMemoryCapping(t *testing.T) {
	rec, expl := recommendMemoryThroughEngine(t, "512Mi", "1024Mi", 50)
	assert.Equal(t, "max_change_capped", expl.ChangeFilterApplied)
	assert.Equal(t, resource.BinarySI, rec.Format,
		"capped memory result should preserve BinarySI format")
	assert.Equal(t, int64(805306368), rec.Value(),
		"50% increase from 512Mi should produce 768Mi")
}

func TestChangeFilter_BinarySIMemoryDecreaseCapping(t *testing.T) {
	rec, expl := recommendMemoryThroughEngine(t, "1Gi", "256Mi", 50)
	assert.Equal(t, "max_change_capped", expl.ChangeFilterApplied)
	assert.Equal(t, resource.BinarySI, rec.Format)
	assert.Equal(t, int64(536870912), rec.Value(),
		"50% decrease from 1Gi should produce 512Mi")
}

func TestBoundsNotUndoneByChangeFilter(t *testing.T) {
	// Flat 2-core usage clamps to maxAllowed 1000m. 1100m is only 9.1%
	// above that, under the 10% minimum-change filter. The published
	// value must still land inside the bounds.
	eng := NewEngine(95, 0, resource.MustParse("100m"), resource.MustParse("1000m"),
		50, 50, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
	rec, _, changed := eng.RecommendWithExplanation(buildRealisticCPUProfile(2.0, 1.0), resource.MustParse("1100m"))
	assert.True(t, changed, "current above maxAllowed must move")
	assert.LessOrEqual(t, rec.MilliValue(), int64(1000), "recommendation must respect maxAllowed")
	assert.GreaterOrEqual(t, rec.MilliValue(), int64(100), "recommendation must respect minAllowed")

	// 95m -> 100m is a 5.3% step, also under the minimum-change filter.
	low := NewEngine(95, 0, resource.MustParse("100m"), resource.MustParse("4000m"),
		50, 50, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
	recLow, _, changedLow := low.RecommendWithExplanation(buildRealisticCPUProfile(0.001, 1.0), resource.MustParse("95m"))
	assert.True(t, changedLow, "current below minAllowed must move")
	assert.GreaterOrEqual(t, recLow.MilliValue(), int64(100), "recommendation must respect minAllowed")
	assert.LessOrEqual(t, recLow.MilliValue(), int64(4000))

	// A 10% decrease cap from 2000m stops at 1800m, which is still above maxAllowed.
	capped := NewEngine(95, 0, resource.MustParse("100m"), resource.MustParse("1000m"),
		50, 10, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
	recCap, _, changedCap := capped.RecommendWithExplanation(buildRealisticCPUProfile(2.0, 1.0), resource.MustParse("2000m"))
	assert.True(t, changedCap)
	assert.LessOrEqual(t, recCap.MilliValue(), int64(1000), "max decrease cap must not leave the value above maxAllowed")
}

func TestChangeFilter_ZeroCurrent(t *testing.T) {
	rec, expl := recommendCPUThroughEngine(t, "0", "500m", 50)
	assert.Empty(t, expl.ChangeFilterApplied)
	assert.Equal(t, int64(500), rec.MilliValue(),
		"zero current should pass through inner recommendation")
}

func TestApplyChangeFilter_RoundsByResource(t *testing.T) {
	parse := func(s string) resource.Quantity {
		t.Helper()
		q, err := resource.ParseQuantity(s)
		require.NoError(t, err)
		return q
	}
	cpuCurrent := *resource.NewQuantity(1, resource.BinarySI)
	cpuRecommended := *resource.NewQuantity(2, resource.BinarySI)

	tests := []struct {
		name        string
		current     resource.Quantity
		recommended resource.Quantity
		minChange   float64
		maxPct      float64
		isCPU       bool
		wantMilli   int64
		wantFormat  resource.Format
		wantReason  string
		wholeByte   bool
	}{
		{
			name:        "decimal memory increase rounds up to a whole byte",
			current:     parse("1Gi"),
			recommended: parse("2G"),
			maxPct:      30,
			wantMilli:   1395864372 * 1000,
			wantFormat:  resource.DecimalSI,
			wantReason:  "max_change_capped",
			wholeByte:   true,
		},
		{
			name:        "decimal memory decrease rounds up to a whole byte",
			current:     parse("1Gi"),
			recommended: parse("500M"),
			maxPct:      30,
			wantMilli:   751619277 * 1000,
			wantFormat:  resource.DecimalSI,
			wantReason:  "max_change_capped",
			wholeByte:   true,
		},
		{
			name:        "plain byte memory does not stay in millibytes",
			current:     parse("100000001"),
			recommended: parse("2000000000"),
			maxPct:      30,
			wantMilli:   130000002 * 1000,
			wantFormat:  resource.DecimalSI,
			wantReason:  "max_change_capped",
			wholeByte:   true,
		},
		{
			name:        "binary memory with a whole product keeps that value",
			current:     parse("512Mi"),
			recommended: parse("1024Mi"),
			maxPct:      50,
			wantMilli:   805306368 * 1000,
			wantFormat:  resource.BinarySI,
			wantReason:  "max_change_capped",
			wholeByte:   true,
		},
		{
			name:        "cpu decimal cap stays in millicores",
			current:     parse("200m"),
			recommended: parse("400m"),
			maxPct:      50,
			isCPU:       true,
			wantMilli:   300,
			wantFormat:  resource.DecimalSI,
			wantReason:  "max_change_capped",
		},
		{
			name:        "cpu binary cap stays in millicores",
			current:     cpuCurrent,
			recommended: cpuRecommended,
			maxPct:      50,
			isCPU:       true,
			wantMilli:   1500,
			wantFormat:  resource.DecimalSI,
			wantReason:  "max_change_capped",
		},
		{
			name:        "zero current returns the recommendation",
			current:     parse("0"),
			recommended: parse("500m"),
			maxPct:      50,
			isCPU:       true,
			wantMilli:   500,
			wantFormat:  resource.DecimalSI,
		},
		{
			name:        "change under the minimum returns current",
			current:     parse("1000m"),
			recommended: parse("1050m"),
			minChange:   10,
			maxPct:      50,
			isCPU:       true,
			wantMilli:   1000,
			wantFormat:  resource.DecimalSI,
			wantReason:  "min_change_filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := applyChangeFilter(tt.current, tt.recommended, tt.minChange, tt.maxPct, tt.maxPct, tt.isCPU)
			assert.Equal(t, tt.wantMilli, got.MilliValue())
			assert.Equal(t, tt.wantFormat, got.Format)
			assert.Equal(t, tt.wantReason, reason)
			if tt.wholeByte {
				assert.Zero(t, got.MilliValue()%1000, "capped memory must be a whole byte")
			}
		})
	}
}

func TestRecommend_DecimalMemoryBoundCapsToWholeBytes(t *testing.T) {
	maxBound, err := resource.ParseQuantity("2G")
	require.NoError(t, err)
	minBound, err := resource.ParseQuantity("4Mi")
	require.NoError(t, err)
	current, err := resource.ParseQuantity("1Gi")
	require.NoError(t, err)

	eng := NewEngine(95, 0, minBound, maxBound, 30, 30, EngineOpts{BurstSensitivity: ptrFloat(0)})
	rec, expl, changed := eng.RecommendWithExplanation(buildRealisticCPUProfile(3_000_000_000, 1.0), current)
	require.True(t, changed, "current 1Gi under a 2G cap must move")
	assert.Equal(t, "max_change_capped", expl.ChangeFilterApplied)
	assert.Equal(t, int64(1395864372), rec.Value())
	assert.Equal(t, int64(1395864372)*1000, rec.MilliValue())
	assert.Zero(t, rec.MilliValue()%1000)
	assert.Equal(t, resource.DecimalSI, rec.Format)
	assert.Equal(t, expl.Final.MilliValue(), rec.MilliValue())

	direct, _ := eng.Recommend(buildRealisticCPUProfile(3_000_000_000, 1.0), current)
	assert.Equal(t, rec.MilliValue(), direct.MilliValue())
}

func recommendCPUThroughEngine(t *testing.T, current, inner string, maxPct float64) (resource.Quantity, RecommendationExplanation) {
	t.Helper()
	eng := NewEngine(95, 0, resource.MustParse("1m"), resource.MustParse("100000m"),
		maxPct, maxPct, EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
	cur := resource.MustParse(current)
	profile := buildRealisticCPUProfile(coresOf(t, inner), 1.0)
	rec, expl, _ := eng.RecommendWithExplanation(profile, cur)
	direct, _ := eng.Recommend(profile, cur)
	assert.Equal(t, rec.MilliValue(), direct.MilliValue(),
		"Recommend must match RecommendWithExplanation")
	return expl.Final, expl
}

func recommendMemoryThroughEngine(t *testing.T, current, inner string, maxPct float64) (resource.Quantity, RecommendationExplanation) {
	t.Helper()
	innerQ := resource.MustParse(inner)
	eng := NewEngine(95, 0, resource.MustParse("1Mi"), resource.MustParse("1024Gi"),
		maxPct, maxPct, EngineOpts{BurstSensitivity: ptrFloat(0)})
	_, expl, _ := eng.RecommendWithExplanation(
		buildRealisticCPUProfile(float64(innerQ.Value()), 1.0), resource.MustParse(current))
	return expl.Final, expl
}

func coresOf(t *testing.T, q string) float64 {
	t.Helper()
	parsed := resource.MustParse(q)
	return float64(parsed.MilliValue()) / 1000
}

func ptrFloat(v float64) *float64 { return &v }
