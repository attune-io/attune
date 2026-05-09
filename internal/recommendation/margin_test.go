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

// stubEstimator returns a fixed quantity for testing decorator estimators.
type stubEstimator struct {
	value resource.Quantity
}

func (s *stubEstimator) Estimate(_ metrics.UsageProfile, _ resource.Quantity) resource.Quantity {
	return s.value.DeepCopy()
}

func TestMarginEstimator(t *testing.T) {
	tests := []struct {
		name       string
		factor     float64
		innerValue string
		wantMillis int64
	}{
		{
			name:       "1.2 factor increases by 20%",
			factor:     1.2,
			innerValue: "100m",
			wantMillis: 120,
		},
		{
			name:       "1.0 factor returns same value",
			factor:     1.0,
			innerValue: "100m",
			wantMillis: 100,
		},
		{
			name:       "CPU millicore values",
			factor:     1.2,
			innerValue: "250m",
			wantMillis: 300,
		},
		{
			name:       "memory byte values via millivalue",
			factor:     1.2,
			innerValue: "1000",
			wantMillis: 1200000, // 1000 * 1000 millis * 1.2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &MarginEstimator{
				Factor: tt.factor,
				Inner:  &stubEstimator{value: resource.MustParse(tt.innerValue)},
			}
			profile := metrics.UsageProfile{Confidence: 0.95}
			result := e.Estimate(profile, resource.MustParse("500m"))
			assert.Equal(t, tt.wantMillis, result.MilliValue())
		})
	}
}
