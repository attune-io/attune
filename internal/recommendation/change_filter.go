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

	"github.com/SebTardif/kube-rightsize/internal/metrics"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ChangeFilter rejects changes that are too small (below MinChangePercent)
// or caps changes that are too large (above MaxChangePercent). This prevents
// thrashing from tiny adjustments and dangerous large swings.
type ChangeFilter struct {
	// MinChangePercent is the minimum percentage change required for the
	// recommendation to differ from current (e.g. 10 means 10%).
	MinChangePercent float64
	// MaxChangePercent is the maximum percentage change allowed per
	// recommendation (e.g. 50 means 50%).
	MaxChangePercent float64
	// Inner is the wrapped estimator whose result is filtered.
	Inner Estimator
}

// Estimate delegates to the inner estimator and then applies change
// filtering. If the change is below MinChangePercent, the current value
// is returned unchanged. If the change exceeds MaxChangePercent, it is
// capped at MaxChangePercent in the appropriate direction.
func (e *ChangeFilter) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	recommended := e.Inner.Estimate(profile, current)

	currentMillis := float64(current.MilliValue())
	recommendedMillis := float64(recommended.MilliValue())

	// If current is zero, return recommended as-is to avoid division by zero.
	if currentMillis == 0 {
		return recommended
	}

	changePct := math.Abs(recommendedMillis-currentMillis) / currentMillis * 100

	// Below minimum threshold: return current unchanged.
	if changePct < e.MinChangePercent {
		return current.DeepCopy()
	}

	// Above maximum threshold: cap the change.
	if changePct > e.MaxChangePercent {
		maxDelta := currentMillis * e.MaxChangePercent / 100
		var capped float64
		if recommendedMillis > currentMillis {
			capped = currentMillis + maxDelta
		} else {
			capped = currentMillis - maxDelta
		}
		if recommended.Format == resource.BinarySI {
			return *resource.NewQuantity(int64(math.Ceil(capped/1000)), resource.BinarySI)
		}
		return *resource.NewMilliQuantity(int64(math.Ceil(capped)), resource.DecimalSI)
	}

	return recommended
}
