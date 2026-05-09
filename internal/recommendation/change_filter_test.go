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
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
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
			e := &ChangeFilter{
				MinChangePercent: tt.minPct,
				MaxChangePercent: tt.maxPct,
				Inner:            &stubEstimator{value: resource.MustParse(tt.innerValue)},
			}
			profile := metrics.UsageProfile{Confidence: 0.95}
			current := resource.MustParse(tt.current)
			result := e.Estimate(profile, current)
			assert.Equal(t, tt.wantMillis, result.MilliValue())
		})
	}
}
