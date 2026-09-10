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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResolveInput is the QoS source plus raw target for ResolveAppliedTarget.
// Pod is required for clamp (resize policy + live limits) and live QoS.
type ResolveInput struct {
	Target                     corev1.ResourceRequirements
	Pod                        *corev1.Pod
	Container                  string
	AllowInPlaceMemoryDecrease bool
	// ApplyUsageFloor runs FloorMemoryLimitForUsage only when the
	// platform clamp did not fire. Live apply sets this; revert does not.
	ApplyUsageFloor    bool
	CurrentMemoryLimit resource.Quantity
	RecentUsage        resource.Quantity
	UsageMarginPercent float64
}

// ResolveMeta is the clamp/floor/raise decision so callers can emit
// events without re-deriving them.
type ResolveMeta struct {
	PreClamped              corev1.ResourceRequirements
	PlatformClamped         bool
	RequestedMemLimit       resource.Quantity
	ClampedMemLimit         resource.Quantity
	GuaranteedRequestRaised bool
	FloorApplied            bool
	FloorFromLimit          resource.Quantity
	FloorToLimit            resource.Quantity
}

// ResolveAppliedTarget is clamp → (usage floor if not clamped) →
// Guaranteed request raise. Live apply, OneShot compare, revert
// compare, and RevertPod must share this so they cannot drift.
func ResolveAppliedTarget(in ResolveInput) (corev1.ResourceRequirements, ResolveMeta) {
	target := in.Target
	meta := ResolveMeta{PreClamped: *target.DeepCopy()}
	target = ClampMemoryLimitForPolicy(in.Pod, in.Container, target, in.AllowInPlaceMemoryDecrease)
	if memLim, ok := meta.PreClamped.Limits[corev1.ResourceMemory]; ok {
		if clampedLim, cok := target.Limits[corev1.ResourceMemory]; cok && !memLim.Equal(clampedLim) {
			meta.PlatformClamped = true
			meta.RequestedMemLimit = memLim
			meta.ClampedMemLimit = clampedLim
		}
	}
	if meta.PlatformClamped {
		before := target.DeepCopy()
		target = RaiseGuaranteedMemoryRequestToLimit(in.Pod, target)
		meta.GuaranteedRequestRaised = memoryRequestRaised(before, &target)
		return target, meta
	}
	if in.ApplyUsageFloor {
		floored, applied := FloorMemoryLimitForUsage(target, in.CurrentMemoryLimit, in.RecentUsage, in.UsageMarginPercent)
		if applied {
			meta.FloorApplied = true
			meta.FloorFromLimit = target.Limits[corev1.ResourceMemory]
			meta.FloorToLimit = floored.Limits[corev1.ResourceMemory]
			target = floored
		}
	}
	target = RaiseGuaranteedMemoryRequestToLimit(in.Pod, target)
	return target, meta
}

func memoryRequestRaised(before, after *corev1.ResourceRequirements) bool {
	if before == nil || after == nil {
		return false
	}
	got, ok := after.Requests[corev1.ResourceMemory]
	if !ok {
		return false
	}
	was, wok := before.Requests[corev1.ResourceMemory]
	if !wok {
		return true
	}
	return !got.Equal(was)
}
