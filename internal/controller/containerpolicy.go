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
	"k8s.io/apimachinery/pkg/api/resource"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/recommendation"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

// enginesForContainer returns the shared policy engines when containerPolicies
// is omitted or empty. Those are the same pointers the caller passed in.
// A non-empty list always builds engines from the effective config so a
// shared engine cannot leak one container's max onto another.
func enginesForContainer(
	policy *attunev1alpha1.AttunePolicy,
	name string,
	sharedCPU, sharedMem *recommendation.RecommendationEngine,
) (*recommendation.RecommendationEngine, *recommendation.RecommendationEngine) {
	if policy == nil || len(policy.Spec.ContainerPolicies) == 0 {
		if sharedCPU == nil || sharedMem == nil {
			return buildRecommendationEngines(policy)
		}
		return sharedCPU, sharedMem
	}
	cpu, mem := attunev1alpha1.EffectiveContainerResources(policy, name)
	return buildEnginesFromResourceConfig(cpu, mem)
}

// containerDecreaseAllowed applies the nil split on the effective config.
// CPU nil means decreases are allowed. Memory nil means decreases are blocked.
func containerDecreaseAllowed(policy *attunev1alpha1.AttunePolicy, name string) (cpuOK, memOK bool) {
	if policy == nil {
		return true, false
	}
	cpu, mem := attunev1alpha1.EffectiveContainerResources(policy, name)
	cpuOK = cpu.AllowDecrease == nil || *cpu.AllowDecrease
	memOK = mem.AllowDecrease != nil && *mem.AllowDecrease
	return cpuOK, memOK
}

// containerControlledRequestsOnly reports whether limits for res are left
// alone for this container. An omitted or empty containerPolicies list
// uses the policy block, matching resourceControlledRequestsOnly.
func containerControlledRequestsOnly(policy *attunev1alpha1.AttunePolicy, containerName string, res corev1.ResourceName) bool {
	if policy == nil || len(policy.Spec.ContainerPolicies) == 0 {
		return resourceControlledRequestsOnly(policy, res)
	}
	cpu, mem := attunev1alpha1.EffectiveContainerResources(policy, containerName)
	var cv *string
	switch res {
	case corev1.ResourceCPU:
		cv = cpu.ControlledValues
	case corev1.ResourceMemory:
		cv = mem.ControlledValues
	default:
		return true
	}
	if cv == nil || *cv == "" || *cv == attunev1alpha1.DefaultControlledValues || *cv == attunev1alpha1.ControlledRequestsOnly {
		return true
	}
	return false
}

// podResourceRequestsOnly is false when any non-excluded app container is
// RequestsAndLimits. Init containers are not scanned: a Resource metric
// sums spec.containers only. Native sidecars (init restartPolicy Always)
// stay out of this cap on purpose, including when their container policy
// is RequestsAndLimits. CREATE initial sizing still counts them in
// createPodRequestsOnly. An omitted list, a nil pod, or a pod with no
// managed app container keeps fallback.
func podResourceRequestsOnly(policy *attunev1alpha1.AttunePolicy, pod *corev1.Pod, res corev1.ResourceName, fallback bool) bool {
	if policy == nil || pod == nil || len(policy.Spec.ContainerPolicies) == 0 {
		return fallback
	}
	excluded := pkgdefaults.EffectiveExcludedContainers(policy)
	saw := false
	for i := range pod.Spec.Containers {
		name := pod.Spec.Containers[i].Name
		if excluded[name] {
			continue
		}
		saw = true
		if !containerControlledRequestsOnly(policy, name, res) {
			return false
		}
	}
	if !saw {
		return fallback
	}
	return true
}

// effectiveMemoryMaxAllowed is the memory ceiling for one container.
// An omitted list aliases the policy pointer. A container max replaces it.
func effectiveMemoryMaxAllowed(policy *attunev1alpha1.AttunePolicy, container string) *resource.Quantity {
	if policy == nil {
		return nil
	}
	_, mem := attunev1alpha1.EffectiveContainerResources(policy, container)
	return mem.MaxAllowed
}
