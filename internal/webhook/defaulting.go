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

package webhook

import (
	"context"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

// RightSizePolicyDefaulter implements the typed Defaulter interface for RightSizePolicy.
type RightSizePolicyDefaulter struct{}

// Default sets default values on a RightSizePolicy.
func (d *RightSizePolicyDefaulter) Default(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) error {
	if policy.Spec.CPU.Percentile == 0 {
		policy.Spec.CPU.Percentile = rightsizev1alpha1.DefaultCPUPercentile
	}
	if policy.Spec.CPU.SafetyMargin == "" {
		policy.Spec.CPU.SafetyMargin = rightsizev1alpha1.DefaultCPUSafetyMargin
	}
	if policy.Spec.Memory.Percentile == 0 {
		policy.Spec.Memory.Percentile = rightsizev1alpha1.DefaultMemoryPercentile
	}
	if policy.Spec.Memory.SafetyMargin == "" {
		policy.Spec.Memory.SafetyMargin = rightsizev1alpha1.DefaultMemorySafetyMargin
	}
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.DefaultUpdateMode
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = rightsizev1alpha1.DefaultMaxCPUChangePercent
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = rightsizev1alpha1.DefaultMaxMemoryChangePercent
	}
	if policy.Spec.Weight == 0 {
		policy.Spec.Weight = rightsizev1alpha1.DefaultWeight
	}
	return nil
}
