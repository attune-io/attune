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
				// (1 + 1.0/0.95)^2 = (1 + 1.0526)^2 = 2.0526^2 ~= 4.21
				// 100m * 4.21 ~= 421m; still a multiplier but much less than low confidence.
				assert.Less(t, millis, int64(500))
				assert.Greater(t, millis, int64(100))
			},
		},
		{
			name:       "low confidence significantly increases result",
			confidence: 0.1,
			multiplier: 1.0,
			exponent:   2.0,
			wantCheck: func(t *testing.T, millis int64) {
				// (1 + 1.0/0.1)^2 = (11)^2 = 121
				// 100m * 121 = 12100m
				assert.Greater(t, millis, int64(10000))
			},
		},
		{
			name:       "zero confidence uses floor of 0.1",
			confidence: 0.0,
			multiplier: 1.0,
			exponent:   2.0,
			wantCheck: func(t *testing.T, millis int64) {
				// Same as confidence 0.1: (1 + 1.0/0.1)^2 = 121
				// 100m * 121 = 12100m
				assert.Greater(t, millis, int64(10000))
			},
		},
		{
			name:       "default multiplier and exponent",
			confidence: 0.5,
			multiplier: 0, // triggers default of 1.0
			exponent:   0, // triggers default of 2.0
			wantCheck: func(t *testing.T, millis int64) {
				// (1 + 1.0/0.5)^2 = 3^2 = 9
				// 100m * 9 = 900m
				assert.InDelta(t, 900, millis, 10)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &ConfidenceEstimator{
				Multiplier: tt.multiplier,
				Exponent:   tt.exponent,
				Inner:      &stubEstimator{value: baseValue},
			}
			profile := metrics.UsageProfile{Confidence: tt.confidence}
			result := e.Estimate(profile, resource.MustParse("500m"))
			tt.wantCheck(t, result.MilliValue())
		})
	}
}
