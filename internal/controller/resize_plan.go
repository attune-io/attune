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

package controller

import (
	corev1 "k8s.io/api/core/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// resizeAction is one container apply computed from an observed pod.
// Plan is pure: no client Get. Budget filter and execute consume this.
type resizeAction struct {
	Container    string
	ContainerRec attunev1alpha1.ContainerRecommendation
	Target       corev1.ResourceRequirements
	ApplyMeta    liveResizeApplyMeta
	Clamped      []string
	CPUIncrease  int64
	MemIncrease  int64
	// AtTarget means the observed container already matches the applied
	// target. Execute still emits clamp/floor, then skips reserve/apply.
	AtTarget bool
}

// planPodActions builds the applied target for every recommended container
// that still differs on the observed pod. I/O (live Get) belongs to the
// caller so this stays table-testable.
func (r *AttunePolicyReconciler) planPodActions(
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	rec attunev1alpha1.WorkloadRecommendation,
) []resizeAction {
	if pod == nil {
		return nil
	}
	var actions []resizeAction
	for _, containerRec := range rec.Containers {
		target, clamped := buildResizeTarget(containerRec)
		target, applyMeta := r.applyLiveResizeTarget(policy, pod, containerRec, target)
		c := findContainerByName(pod, containerRec.Name)
		atTarget := c != nil && containerMatchesAppliedTarget(c, target)
		cpuInc, memInc := int64(0), int64(0)
		if !atTarget {
			cpuInc, memInc = budgetIncrease(pod, containerRec.Name, target)
		}
		actions = append(actions, resizeAction{
			Container:    containerRec.Name,
			ContainerRec: containerRec,
			Target:       target,
			ApplyMeta:    applyMeta,
			Clamped:      clamped,
			CPUIncrease:  cpuInc,
			MemIncrease:  memInc,
			AtTarget:     atTarget,
		})
	}
	return actions
}

// filterActionsByBudget keeps actions whose increases fit the remaining
// cycle caps and defers the rest. A later cheaper action can still be
// kept after an over-budget one (same as reserveBudget continue).
// cpuBudget/memBudget of -1 means unlimited. Decreases (zero cost) always fit.
func filterActionsByBudget(actions []resizeAction, cpuBudget, memBudget int64) (keep, deferred []resizeAction) {
	for _, a := range actions {
		exceeded := (cpuBudget >= 0 && a.CPUIncrease > cpuBudget) ||
			(memBudget >= 0 && a.MemIncrease > memBudget)
		if exceeded {
			deferred = append(deferred, a)
			continue
		}
		keep = append(keep, a)
		if cpuBudget >= 0 {
			cpuBudget -= a.CPUIncrease
		}
		if memBudget >= 0 {
			memBudget -= a.MemIncrease
		}
	}
	return keep, deferred
}
