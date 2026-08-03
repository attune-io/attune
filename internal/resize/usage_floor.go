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

package resize

import (
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// FloorMemoryLimitForUsage raises target memory limit when it would fall at
// or below recentUsage * (1 + marginPercent/100). Returns the adjusted target
// and whether the limit was raised (usage floor applied).
//
// If recentUsage is zero or target does not decrease the memory limit relative
// to currentLimit, target is returned unchanged.
// marginPercent is clamped to [0, 100].
func FloorMemoryLimitForUsage(
	target corev1.ResourceRequirements,
	currentLimit, recentUsage resource.Quantity,
	marginPercent float64,
) (corev1.ResourceRequirements, bool) {
	targetLim, ok := target.Limits[corev1.ResourceMemory]
	if !ok || targetLim.IsZero() {
		return target, false
	}
	if currentLimit.IsZero() || targetLim.Cmp(currentLimit) >= 0 {
		return target, false
	}
	if recentUsage.IsZero() || recentUsage.Sign() <= 0 {
		return target, false
	}
	if math.IsNaN(marginPercent) || math.IsInf(marginPercent, 0) {
		marginPercent = 0
	}
	if marginPercent < 0 {
		marginPercent = 0
	}
	if marginPercent > 100 {
		marginPercent = 100
	}

	// floorBytes = usage * (1 + margin/100), rounded up to whole bytes.
	usageBytes := float64(recentUsage.Value())
	floorBytes := usageBytes * (1.0 + marginPercent/100.0)
	if floorBytes <= 0 || math.IsNaN(floorBytes) || math.IsInf(floorBytes, 0) {
		return target, false
	}
	// Round up so we never floor below the fractional product.
	floorInt := int64(math.Ceil(floorBytes))
	if floorInt <= 0 {
		return target, false
	}
	floor := *resource.NewQuantity(floorInt, resource.BinarySI)

	// Limit must be strictly above recent usage when margin is 0, so when
	// floor equals usage, require at least usage+1 byte if target would be
	// at or below usage.
	if marginPercent == 0 && floor.Cmp(recentUsage) <= 0 {
		floor = *resource.NewQuantity(recentUsage.Value()+1, resource.BinarySI)
	}

	if targetLim.Cmp(floor) >= 0 {
		return target, false
	}

	// Do not raise the limit above the current limit (we only prevent
	// unsafe decreases; we do not invent increases).
	if floor.Cmp(currentLimit) > 0 {
		floor = currentLimit.DeepCopy()
	}
	if targetLim.Cmp(floor) >= 0 {
		return target, false
	}

	adjusted := target.DeepCopy()
	if adjusted.Limits == nil {
		adjusted.Limits = corev1.ResourceList{}
	}
	adjusted.Limits[corev1.ResourceMemory] = floor
	// Keep request <= limit when both are set.
	if req, rok := adjusted.Requests[corev1.ResourceMemory]; rok && req.Cmp(floor) > 0 {
		adjusted.Requests[corev1.ResourceMemory] = floor.DeepCopy()
	}
	return *adjusted, true
}
