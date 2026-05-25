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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/attune-io/attune/internal/metrics"
)

func makeProfile(overall metrics.PercentileSet, hourlyOverrides map[int]metrics.PercentileSet) metrics.UsageProfile {
	p := metrics.UsageProfile{
		OverallPercentiles: overall,
		DataPoints:         1000,
		TimeSpanDays:       7,
		Confidence:         0.95,
	}
	// Set all hours to overall by default.
	for h := 0; h < 24; h++ {
		p.HourlyPercentiles[h] = overall
	}
	// Apply overrides.
	for h, ps := range hourlyOverrides {
		p.HourlyPercentiles[h] = ps
	}
	return p
}

func TestPercentileEstimator(t *testing.T) {
	tests := []struct {
		name       string
		percentile int
		profile    metrics.UsageProfile
		current    resource.Quantity
		wantMillis int64
	}{
		{
			name:       "P95 selection from known values",
			percentile: 95,
			profile: makeProfile(metrics.PercentileSet{
				P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200,
			}, nil),
			current:    resource.MustParse("500m"),
			wantMillis: 100,
		},
		{
			name:       "P99 selection from known values",
			percentile: 99,
			profile: makeProfile(metrics.PercentileSet{
				P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200,
			}, nil),
			current:    resource.MustParse("500m"),
			wantMillis: 150,
		},
		{
			name:       "hourly variation picks max across hours",
			percentile: 95,
			profile: makeProfile(
				metrics.PercentileSet{P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200},
				map[int]metrics.PercentileSet{
					14: {P50: 0.100, P90: 0.200, P95: 0.300, P99: 0.400, Max: 0.500},
				},
			),
			current:    resource.MustParse("500m"),
			wantMillis: 300, // hour 14 has P95=0.300
		},
		{
			name:       "P50 selection",
			percentile: 50,
			profile: makeProfile(metrics.PercentileSet{
				P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200,
			}, nil),
			current:    resource.MustParse("500m"),
			wantMillis: 50,
		},
		{
			name:       "P90 selection",
			percentile: 90,
			profile: makeProfile(metrics.PercentileSet{
				P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200,
			}, nil),
			current:    resource.MustParse("500m"),
			wantMillis: 80,
		},
		{
			name:       "unknown percentile defaults to P95",
			percentile: 75,
			profile: makeProfile(metrics.PercentileSet{
				P50: 0.050, P90: 0.080, P95: 0.100, P99: 0.150, Max: 0.200,
			}, nil),
			current:    resource.MustParse("500m"),
			wantMillis: 100,
		},
		{
			name:       "zero profile returns current",
			percentile: 95,
			profile:    metrics.UsageProfile{},
			current:    resource.MustParse("200m"),
			wantMillis: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &PercentileEstimator{Percentile: tt.percentile, IsCPU: true}
			result := e.Estimate(tt.profile, tt.current)
			assert.Equal(t, tt.wantMillis, result.MilliValue())
		})
	}
}

func TestPercentileEstimator_MemoryConversion(t *testing.T) {
	// 256Mi = 268435456 bytes.
	memP95 := 268435456.0
	profile := makeProfile(metrics.PercentileSet{
		P50: memP95 * 0.5, P90: memP95 * 0.9, P95: memP95,
		P99: memP95 * 1.1, Max: memP95 * 1.2,
	}, nil)

	e := &PercentileEstimator{Percentile: 95, IsCPU: false}
	result := e.Estimate(profile, resource.MustParse("512Mi"))

	// Should produce 268435456 bytes in BinarySI format.
	assert.Equal(t, int64(268435456), result.Value(),
		"memory P95 should produce correct byte value")
	assert.Equal(t, resource.BinarySI, result.Format,
		"memory quantity should use BinarySI format")
}

func TestQuantityFromFloat_CPUSubMillicore(t *testing.T) {
	// 0.0005 cores = 0.5m, ceil should round up to 1m.
	q := quantityFromFloat(0.0005, true)
	assert.Equal(t, int64(1), q.MilliValue(),
		"sub-millicore CPU should round up to 1m")
}

func TestQuantityFromFloat_MemoryFractional(t *testing.T) {
	// 100.3 bytes should round up to 101 bytes.
	q := quantityFromFloat(100.3, false)
	assert.Equal(t, int64(101), q.Value(),
		"fractional bytes should round up")
	assert.Equal(t, resource.BinarySI, q.Format)
}

func TestPercentileEstimator_NaNReturnsCurrent(t *testing.T) {
	e := &PercentileEstimator{Percentile: 95}
	nanPS := metrics.PercentileSet{
		P50: math.NaN(), P90: math.NaN(), P95: math.NaN(),
		P99: math.NaN(), Max: math.NaN(),
	}
	profile := makeProfile(nanPS, nil)
	current := resource.MustParse("500m")

	result := e.Estimate(profile, current)
	assert.Equal(t, current.MilliValue(), result.MilliValue(),
		"NaN percentiles should fall back to current allocation")
}

func TestPercentileEstimator_InfReturnsCurrent(t *testing.T) {
	e := &PercentileEstimator{Percentile: 95}
	infPS := metrics.PercentileSet{
		P50: math.Inf(1), P90: math.Inf(1), P95: math.Inf(1),
		P99: math.Inf(1), Max: math.Inf(1),
	}
	profile := makeProfile(infPS, nil)
	current := resource.MustParse("500m")

	result := e.Estimate(profile, current)
	assert.Equal(t, current.MilliValue(), result.MilliValue(),
		"+Inf percentiles should fall back to current allocation")
}
