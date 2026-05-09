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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

// RightSizePolicyValidator implements webhook.CustomValidator for RightSizePolicy.
type RightSizePolicyValidator struct{}

var _ webhook.CustomValidator = &RightSizePolicyValidator{}

func (v *RightSizePolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj)
}

func (v *RightSizePolicyValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(newObj)
}

func (v *RightSizePolicyValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *RightSizePolicyValidator) validate(obj runtime.Object) (admission.Warnings, error) {
	policy, ok := obj.(*rightsizev1alpha1.RightSizePolicy)
	if !ok {
		return nil, nil
	}

	var warnings admission.Warnings

	// CPU bounds: min must be <= max
	if policy.Spec.CPU.Bounds != nil {
		if policy.Spec.CPU.Bounds.Min.Cmp(policy.Spec.CPU.Bounds.Max) > 0 {
			return warnings, fmt.Errorf("cpu.bounds.min (%s) must be <= cpu.bounds.max (%s)",
				policy.Spec.CPU.Bounds.Min.String(), policy.Spec.CPU.Bounds.Max.String())
		}
	}

	// Memory bounds: min must be <= max
	if policy.Spec.Memory.Bounds != nil {
		if policy.Spec.Memory.Bounds.Min.Cmp(policy.Spec.Memory.Bounds.Max) > 0 {
			return warnings, fmt.Errorf("memory.bounds.min (%s) must be <= memory.bounds.max (%s)",
				policy.Spec.Memory.Bounds.Min.String(), policy.Spec.Memory.Bounds.Max.String())
		}
	}

	// Canary config required when mode is Canary
	if policy.Spec.UpdateStrategy.Mode == "Canary" && policy.Spec.UpdateStrategy.Canary == nil {
		return warnings, fmt.Errorf("updateStrategy.canary is required when mode is Canary")
	}

	// targetRef must have name or selector
	if (policy.Spec.TargetRef.Name == nil || *policy.Spec.TargetRef.Name == "") && policy.Spec.TargetRef.Selector == nil {
		return warnings, fmt.Errorf("targetRef must specify either name or selector")
	}

	// Warn if memory decrease is enabled
	if policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease {
		warnings = append(warnings, "memory.allowDecrease is enabled; this carries OOMKill risk")
	}

	return warnings, nil
}