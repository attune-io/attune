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

func TestConfidenceEstimator(t *testing.T) {
	baseValue := resource.MustParse("100m")

	tests := []struct {
		name       string
		confidence float64
		multiplier float64
		exponent   float64
		wantCheck  func(t *testing.T, millis int64)
	}{
		{
			name:       "high confidence barely changes result",
			confidence: 0.95,
			multiplier: 1.0,
			exponent:   2.0,
			wantCheck: func(t *testing.T, millis int64) {
				// 1 + (1-0.95)^2 = 1.0025; ceil(100m * 1.0025) = 101m
				assert.Equal(t, int64(101), millis)
			},
		},
		{
			name:       "low confidence widens by less than 2x",
			confidence: 0.1,
			multiplier: 1.0,
			exponent:   2.0,
			wantCheck: func(t *testing.T, millis int64) {
				// 1 + 0.9^2 = 1.81; ceil(100m * 1.81) = 181m
				assert.Equal(t, int64(181), millis)
			},
		},
		{
			name:       "zero confidence doubles",
			confidence: 0.0,
			multiplier: 1.0,
			exponent:   2.0,
			wantCheck: func(t *testing.T, millis int64) {
				// 1 + 1^2 = 2; 100m * 2 = 200m
				assert.Equal(t, int64(200), millis)
			},
		},
		{
			name:       "default multiplier and exponent",
			confidence: 0.5,
			multiplier: 0, // triggers default of 1.0
			exponent:   0, // triggers default of 2.0
			wantCheck: func(t *testing.T, millis int64) {
				// 1 + (1-0.5)^2 = 1.25; ceil(100m * 1.25) = 125m
				assert.Equal(t, int64(125), millis)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &confidenceEstimator{
				multiplier: tt.multiplier,
				exponent:   tt.exponent,
				inner:      &stubEstimator{value: baseValue},
			}
			profile := metrics.UsageProfile{Confidence: tt.confidence}
			result := e.Estimate(profile, resource.MustParse("500m"))
			tt.wantCheck(t, result.MilliValue())
		})
	}
}
