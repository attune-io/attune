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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// ContainerPolicyWildcard is the single field-wise fallback name.
const ContainerPolicyWildcard = "*"

// ContainerResourcePolicy sets honored CPU and memory fields for one
// container name. containerName "*" is the field-wise fallback, not a
// whole-block replacement. Exact case-sensitive match. Not a regular
// expression.
type ContainerResourcePolicy struct {
	// ContainerName is the container to configure, or "*" for the
	// field-wise fallback. Required. At most one "*" entry is allowed.
	// Not a regular expression. Exact case-sensitive match.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	ContainerName string `json:"containerName"`

	// CPU overrides honored CPU fields for this container.
	// +optional
	CPU *ResourceConfig `json:"cpu,omitempty"`

	// Memory overrides honored memory fields for this container.
	// +optional
	Memory *ResourceConfig `json:"memory,omitempty"`
}

// ContainerPolicySource names where one honored field came from.
type ContainerPolicySource string

const (
	// ContainerPolicySourcePolicy is the merged policy cpu or memory block.
	ContainerPolicySourcePolicy ContainerPolicySource = "policy"
	// ContainerPolicySourceWildcard is a set field on the "*" entry.
	ContainerPolicySourceWildcard ContainerPolicySource = "wildcard"
	// ContainerPolicySourceContainer is a set field on this entry.
	ContainerPolicySourceContainer ContainerPolicySource = "container"
)

// ContainerPolicyFieldSources records the origin of each honored field.
type ContainerPolicyFieldSources struct {
	Percentile         ContainerPolicySource
	Overhead           ContainerPolicySource
	MinAllowed         ContainerPolicySource
	MaxAllowed         ContainerPolicySource
	BurstSensitivity   ContainerPolicySource
	MaxChangePercent   ContainerPolicySource
	MaxIncreasePercent ContainerPolicySource
	MaxDecreasePercent ContainerPolicySource
	AllowDecrease      ContainerPolicySource
	ControlledValues   ContainerPolicySource
}

// ExplainedContainerPolicy is one containerPolicies row plus the effective
// CPU and memory config and the source of each honored field.
type ExplainedContainerPolicy struct {
	ContainerName string
	CPU           ResourceConfig
	Memory        ResourceConfig
	CPUSources    ContainerPolicyFieldSources
	MemorySources ContainerPolicyFieldSources
}

// EffectiveContainerResources resolves CPU and memory for containerName.
// An omitted or empty containerPolicies list returns the policy blocks by
// value. Nested pointers alias the policy; callers only read them.
// A non-empty list deep-copies the policy blocks, then overlays honored
// fields from "*" and then the literal name. The first match wins.
// Lookup of "*" applies that entry once. Percentile 0, overhead "", and
// nil pointers are unset. Overhead "0" is set. Built-in engine defaults
// are not written onto the result. startupBoost, memoryFromCpuRatio,
// decreaseUsageMarginPercent, limitMultiplier, oomBump, and surge stay
// on the policy block. The webhook rejects those fields on a container
// entry.
func EffectiveContainerResources(policy *AttunePolicy, containerName string) (cpu, memory ResourceConfig) {
	if policy == nil {
		return ResourceConfig{}, ResourceConfig{}
	}
	if len(policy.Spec.ContainerPolicies) == 0 {
		return policy.Spec.CPU, policy.Spec.Memory
	}
	cpu = *policy.Spec.CPU.DeepCopy()
	memory = *policy.Spec.Memory.DeepCopy()
	star, literal := firstContainerPolicies(policy.Spec.ContainerPolicies, containerName)
	if containerName == ContainerPolicyWildcard {
		if star != nil {
			overlayHonoredFields(&cpu, star.CPU, nil, "")
			overlayHonoredFields(&memory, star.Memory, nil, "")
		}
		return cpu, memory
	}
	if star != nil {
		overlayHonoredFields(&cpu, star.CPU, nil, "")
		overlayHonoredFields(&memory, star.Memory, nil, "")
	}
	if literal != nil {
		overlayHonoredFields(&cpu, literal.CPU, nil, "")
		overlayHonoredFields(&memory, literal.Memory, nil, "")
	}
	return cpu, memory
}

// ExplainContainerPolicies returns one row per containerPolicies entry.
// An omitted or empty list returns nil. A named row applies "*" with
// source wildcard, then that entry with source container. The "*" row
// labels its own set fields as container. A later duplicate name still
// shows that entry; recommendation uses the first match.
func ExplainContainerPolicies(policy *AttunePolicy) []ExplainedContainerPolicy {
	if policy == nil || len(policy.Spec.ContainerPolicies) == 0 {
		return nil
	}
	rows := make([]ExplainedContainerPolicy, 0, len(policy.Spec.ContainerPolicies))
	var star *ContainerResourcePolicy
	for i := range policy.Spec.ContainerPolicies {
		if policy.Spec.ContainerPolicies[i].ContainerName == ContainerPolicyWildcard {
			star = &policy.Spec.ContainerPolicies[i]
			break
		}
	}
	seenStar := false
	for i := range policy.Spec.ContainerPolicies {
		entry := &policy.Spec.ContainerPolicies[i]
		row := ExplainedContainerPolicy{ContainerName: entry.ContainerName}
		row.CPU = *policy.Spec.CPU.DeepCopy()
		row.Memory = *policy.Spec.Memory.DeepCopy()
		row.CPUSources = policyFieldSources()
		row.MemorySources = policyFieldSources()
		if entry.ContainerName == ContainerPolicyWildcard {
			// A second "*" row shows the first "*" fields. Recommendation
			// also keeps the first match when the webhook is bypassed.
			apply := entry
			if seenStar && star != nil {
				apply = star
			}
			seenStar = true
			overlayHonoredFields(&row.CPU, apply.CPU, &row.CPUSources, ContainerPolicySourceContainer)
			overlayHonoredFields(&row.Memory, apply.Memory, &row.MemorySources, ContainerPolicySourceContainer)
		} else {
			if star != nil {
				overlayHonoredFields(&row.CPU, star.CPU, &row.CPUSources, ContainerPolicySourceWildcard)
				overlayHonoredFields(&row.Memory, star.Memory, &row.MemorySources, ContainerPolicySourceWildcard)
			}
			overlayHonoredFields(&row.CPU, entry.CPU, &row.CPUSources, ContainerPolicySourceContainer)
			overlayHonoredFields(&row.Memory, entry.Memory, &row.MemorySources, ContainerPolicySourceContainer)
		}
		rows = append(rows, row)
	}
	return rows
}

func firstContainerPolicies(policies []ContainerResourcePolicy, containerName string) (star, literal *ContainerResourcePolicy) {
	for i := range policies {
		entry := &policies[i]
		if entry.ContainerName == ContainerPolicyWildcard {
			if star == nil {
				star = entry
			}
			continue
		}
		if literal == nil && entry.ContainerName == containerName {
			literal = entry
		}
	}
	return star, literal
}

func policyFieldSources() ContainerPolicyFieldSources {
	return ContainerPolicyFieldSources{
		Percentile:         ContainerPolicySourcePolicy,
		Overhead:           ContainerPolicySourcePolicy,
		MinAllowed:         ContainerPolicySourcePolicy,
		MaxAllowed:         ContainerPolicySourcePolicy,
		BurstSensitivity:   ContainerPolicySourcePolicy,
		MaxChangePercent:   ContainerPolicySourcePolicy,
		MaxIncreasePercent: ContainerPolicySourcePolicy,
		MaxDecreasePercent: ContainerPolicySourcePolicy,
		AllowDecrease:      ContainerPolicySourcePolicy,
		ControlledValues:   ContainerPolicySourcePolicy,
	}
}

func overlayHonoredFields(dst *ResourceConfig, src *ResourceConfig, sources *ContainerPolicyFieldSources, source ContainerPolicySource) {
	if dst == nil || src == nil {
		return
	}
	if src.Percentile != 0 {
		dst.Percentile = src.Percentile
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.Percentile = source })
	}
	if src.Overhead != "" {
		dst.Overhead = src.Overhead
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.Overhead = source })
	}
	if src.MinAllowed != nil {
		dst.MinAllowed = cloneQuantity(src.MinAllowed)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.MinAllowed = source })
	}
	if src.MaxAllowed != nil {
		dst.MaxAllowed = cloneQuantity(src.MaxAllowed)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.MaxAllowed = source })
	}
	if src.BurstSensitivity != nil {
		dst.BurstSensitivity = cloneString(src.BurstSensitivity)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.BurstSensitivity = source })
	}
	if src.MaxChangePercent != nil {
		dst.MaxChangePercent = cloneInt32(src.MaxChangePercent)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.MaxChangePercent = source })
	}
	if src.MaxIncreasePercent != nil {
		dst.MaxIncreasePercent = cloneInt32(src.MaxIncreasePercent)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.MaxIncreasePercent = source })
	}
	if src.MaxDecreasePercent != nil {
		dst.MaxDecreasePercent = cloneInt32(src.MaxDecreasePercent)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.MaxDecreasePercent = source })
	}
	if src.AllowDecrease != nil {
		dst.AllowDecrease = cloneBool(src.AllowDecrease)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.AllowDecrease = source })
	}
	if src.ControlledValues != nil {
		dst.ControlledValues = cloneString(src.ControlledValues)
		setSource(sources, func(s *ContainerPolicyFieldSources) { s.ControlledValues = source })
	}
}

func setSource(sources *ContainerPolicyFieldSources, set func(*ContainerPolicyFieldSources)) {
	if sources == nil {
		return
	}
	set(sources)
}

func cloneString(in *string) *string {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneInt32(in *int32) *int32 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneQuantity(in *resource.Quantity) *resource.Quantity {
	if in == nil {
		return nil
	}
	copied := in.DeepCopy()
	return &copied
}
