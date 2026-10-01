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

func TestBoundsEstimator(t *testing.T) {
	minBound := resource.MustParse("50m")
	maxBound := resource.MustParse("2000m")

	tests := []struct {
		name       string
		innerValue string
		wantMillis int64
	}{
		{
			name:       "value within bounds returns unchanged",
			innerValue: "500m",
			wantMillis: 500,
		},
		{
			name:       "value below min returns min",
			innerValue: "10m",
			wantMillis: 50,
		},
		{
			name:       "value above max returns max",
			innerValue: "5000m",
			wantMillis: 2000,
		},
		{
			name:       "value equal to min returns unchanged",
			innerValue: "50m",
			wantMillis: 50,
		},
		{
			name:       "value equal to max returns unchanged",
			innerValue: "2000m",
			wantMillis: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &boundsEstimator{
				min:   minBound,
				max:   maxBound,
				inner: &stubEstimator{value: resource.MustParse(tt.innerValue)},
			}
			profile := metrics.UsageProfile{Confidence: 0.95}
			result := e.Estimate(profile, resource.MustParse("500m"))
			assert.Equal(t, tt.wantMillis, result.MilliValue())
		})
	}
}

func TestApplyBounds_NilMaxSkipsCeiling(t *testing.T) {
	q := resource.MustParse("10")
	min := resource.MustParse("1m")
	got, which := applyBounds(q, min, nil)
	assert.True(t, q.Equal(got), got.String())
	assert.Empty(t, which)
}

func TestApplyBounds_ZeroMaxIsACap(t *testing.T) {
	q := resource.MustParse("10")
	min := resource.MustParse("1m")
	zero := resource.MustParse("0")
	got, which := applyBounds(q, min, &zero)
	assert.True(t, zero.Equal(got), got.String())
	assert.Equal(t, "max", which)
}

func TestApplyBounds_MaxAppliesAfterMin(t *testing.T) {
	q, err := resource.ParseQuantity("100m")
	assert.NoError(t, err)
	min, err := resource.ParseQuantity("500m")
	assert.NoError(t, err)
	max, err := resource.ParseQuantity("200m")
	assert.NoError(t, err)
	got, which := applyBounds(q, min, &max)
	assert.Equal(t, "max", which)
	assert.True(t, max.Equal(got), got.String())
}

func TestApplyBounds_ZeroMaxKeepsFloorWhenRaised(t *testing.T) {
	q, err := resource.ParseQuantity("0")
	assert.NoError(t, err)
	min, err := resource.ParseQuantity("1m")
	assert.NoError(t, err)
	max, err := resource.ParseQuantity("0")
	assert.NoError(t, err)
	got, which := applyBounds(q, min, &max)
	assert.Equal(t, "min", which)
	assert.True(t, min.Equal(got), got.String())
}
