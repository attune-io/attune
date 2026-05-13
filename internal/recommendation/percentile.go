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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
)

// PercentileEstimator selects the configured percentile from the usage
// profile. It takes the maximum across all hourly percentiles at the
// configured level to ensure coverage of peak hours.
type PercentileEstimator struct {
	// Percentile is the target percentile to use: 50, 90, 95, or 99.
	Percentile int
	// IsCPU indicates whether this estimator handles CPU (true) or memory (false).
	IsCPU bool
}

// Estimate returns a resource.Quantity derived from the maximum of the
// configured percentile across all 24 hourly buckets. The float64 value
// is interpreted as cores for CPU or bytes for memory.
func (e *PercentileEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	maxVal := e.selectPercentile(profile.OverallPercentiles)

	// Take the max across all hourly percentiles.
	for h := 0; h < 24; h++ {
		hourVal := e.selectPercentile(profile.HourlyPercentiles[h])
		maxVal = math.Max(maxVal, hourVal)
	}

	if maxVal <= 0 || math.IsNaN(maxVal) || math.IsInf(maxVal, 0) {
		return current
	}

	return quantityFromFloat(maxVal, e.IsCPU)
}

// selectPercentile extracts the value for the configured percentile level
// from a PercentileSet.
func (e *PercentileEstimator) selectPercentile(ps metrics.PercentileSet) float64 {
	switch e.Percentile {
	case 50:
		return ps.P50
	case 90:
		return ps.P90
	case 95:
		return ps.P95
	case 99:
		return ps.P99
	default:
		return ps.P95
	}
}

// quantityFromFloat converts a float64 value to a resource.Quantity.
// CPU values are in cores and use millicore precision with DecimalSI.
// Memory values are in bytes and use integer bytes with BinarySI.
func quantityFromFloat(val float64, isCPU bool) resource.Quantity {
	if isCPU {
		millis := int64(math.Ceil(val * 1000))
		return *resource.NewMilliQuantity(millis, resource.DecimalSI)
	}
	return *resource.NewQuantity(int64(math.Ceil(val)), resource.BinarySI)
}
