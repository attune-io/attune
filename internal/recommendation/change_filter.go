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
)

// applyChangeFilter is the single min/max change implementation used by
// RecommendWithExplanation. A change below minChangePercent is skipped.
// A change above the directional cap is clamped. currentMillis==0 skips
// the filter so a first recommendation is not divided by zero.
func applyChangeFilter(current, recommended resource.Quantity, minChangePercent, maxIncreasePercent, maxDecreasePercent float64) (resource.Quantity, string) {
	currentMillis := float64(current.MilliValue())
	if currentMillis == 0 {
		return recommended, ""
	}

	recommendedMillis := float64(recommended.MilliValue())
	changePct := math.Abs(recommendedMillis-currentMillis) / currentMillis * 100
	isIncrease := recommendedMillis > currentMillis
	maxPct := maxDecreasePercent
	if isIncrease {
		maxPct = maxIncreasePercent
	}

	if changePct < minChangePercent {
		return current.DeepCopy(), "min_change_filtered"
	}
	if changePct > maxPct {
		maxDelta := currentMillis * maxPct / 100
		capped := currentMillis - maxDelta
		if isIncrease {
			capped = currentMillis + maxDelta
		}
		if recommended.Format == resource.BinarySI {
			return *resource.NewQuantity(int64(math.Ceil(capped/1000)), resource.BinarySI), "max_change_capped"
		}
		return *resource.NewMilliQuantity(int64(math.Ceil(capped)), resource.DecimalSI), "max_change_capped"
	}
	return recommended, ""
}
