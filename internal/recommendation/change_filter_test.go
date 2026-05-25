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

	"github.com/attune-io/attune/internal/metrics"
)

func TestChangeFilter(t *testing.T) {
	tests := []struct {
		name       string
		current    string
		innerValue string
		minPct     float64
		maxPct     float64
		wantMillis int64
	}{
		{
			name:       "small change below threshold returns current",
			current:    "1000m",
			innerValue: "1050m", // 5% change, below 10% min
			minPct:     10,
			maxPct:     50,
			wantMillis: 1000,
		},
		{
			name:       "large increase above max caps at max percent",
			current:    "1000m",
			innerValue: "1800m", // 80% change, above 50% max
			minPct:     10,
			maxPct:     50,
			wantMillis: 1500, // 1000 + 50% = 1500
		},
		{
			name:       "change within range passes through",
			current:    "1000m",
			innerValue: "1200m", // 20% change, within 10-50% range
			minPct:     10,
			maxPct:     50,
			wantMillis: 1200,
		},
		{
			name:       "decrease within range passes through",
			current:    "1000m",
			innerValue: "800m", // 20% decrease, within range
			minPct:     10,
			maxPct:     50,
			wantMillis: 800,
		},
		{
			name:       "large decrease above max caps at max percent",
			current:    "1000m",
			innerValue: "300m", // 70% decrease, above 50% max
			minPct:     10,
			maxPct:     50,
			wantMillis: 500, // 1000 - 50% = 500
		},
		{
			name:       "small decrease below threshold returns current",
			current:    "1000m",
			innerValue: "960m", // 4% decrease, below 10% min
			minPct:     10,
			maxPct:     50,
			wantMillis: 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &changeFilter{
				minChangePercent: tt.minPct,
				maxChangePercent: tt.maxPct,
				inner:            &stubEstimator{value: resource.MustParse(tt.innerValue)},
			}
			profile := metrics.UsageProfile{Confidence: 0.95}
			current := resource.MustParse(tt.current)
			result := e.Estimate(profile, current)
			assert.Equal(t, tt.wantMillis, result.MilliValue())
		})
	}
}

func TestChangeFilter_BinarySIMemoryCapping(t *testing.T) {
	// Current: 512Mi, inner recommends 1024Mi (100% increase), max 50%.
	// Expected: 512Mi + 50% = 768Mi.
	innerValue := resource.MustParse("1024Mi")
	e := &changeFilter{
		minChangePercent: 10,
		maxChangePercent: 50,
		inner:            &stubEstimator{value: innerValue},
	}
	current := resource.MustParse("512Mi")
	result := e.Estimate(metrics.UsageProfile{Confidence: 0.95}, current)

	assert.Equal(t, resource.BinarySI, result.Format,
		"capped memory result should preserve BinarySI format")
	// 512Mi = 536870912 bytes. 50% increase = 805306368 bytes.
	// The code computes in millis: current=536870912000, delta=268435456000,
	// capped=805306368000, then divides by 1000 and ceils: 805306368.
	assert.Equal(t, int64(805306368), result.Value(),
		"50%% increase from 512Mi should produce 768Mi")
}

func TestChangeFilter_BinarySIMemoryDecreaseCapping(t *testing.T) {
	// Current: 1Gi, inner recommends 256Mi (75% decrease), max 50%.
	// Expected: 1Gi - 50% = 512Mi.
	innerValue := resource.MustParse("256Mi")
	e := &changeFilter{
		minChangePercent: 10,
		maxChangePercent: 50,
		inner:            &stubEstimator{value: innerValue},
	}
	current := resource.MustParse("1Gi")
	result := e.Estimate(metrics.UsageProfile{Confidence: 0.95}, current)

	assert.Equal(t, resource.BinarySI, result.Format)
	// 1Gi = 1073741824 bytes. 50% decrease = 536870912.
	assert.Equal(t, int64(536870912), result.Value(),
		"50%% decrease from 1Gi should produce 512Mi")
}

func TestChangeFilter_ZeroCurrent(t *testing.T) {
	innerValue := resource.MustParse("500m")
	e := &changeFilter{
		minChangePercent: 10,
		maxChangePercent: 50,
		inner:            &stubEstimator{value: innerValue},
	}
	current := resource.MustParse("0")
	result := e.Estimate(metrics.UsageProfile{Confidence: 0.95}, current)

	assert.Equal(t, int64(500), result.MilliValue(),
		"zero current should pass through inner recommendation")
}
