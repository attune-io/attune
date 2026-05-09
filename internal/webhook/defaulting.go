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
		policy.Spec.CPU.Percentile = 95
	}
	if policy.Spec.CPU.SafetyMargin == "" {
		policy.Spec.CPU.SafetyMargin = "1.2"
	}
	if policy.Spec.Memory.Percentile == 0 {
		policy.Spec.Memory.Percentile = 99
	}
	if policy.Spec.Memory.SafetyMargin == "" {
		policy.Spec.Memory.SafetyMargin = "1.3"
	}
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = "Recommend"
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = 50
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = 30
	}
	if policy.Spec.Weight == 0 {
		policy.Spec.Weight = 100
	}
	return nil
}