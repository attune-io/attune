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

func TestChangeFilter_ZeroCurrent(t *testing.T) {
	rec, expl := recommendCPUThroughEngine(t, "0", "500m", 50)
	assert.Empty(t, expl.ChangeFilterApplied)
	assert.Equal(t, int64(500), rec.MilliValue(),
		"zero current should pass through inner recommendation")
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
