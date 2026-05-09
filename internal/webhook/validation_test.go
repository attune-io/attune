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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

func validPolicy() *rightsizev1alpha1.RightSizePolicy {
	name := "my-app"
	return &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &name,
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: "Recommend",
			},
		},
	}
}

func TestValidate_ValidPolicy(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_CPUBoundsInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.CPU.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2"),
		Max: resource.MustParse("1"),
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.bounds.min")
	assert.Contains(t, err.Error(), "must be <= cpu.bounds.max")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryBoundsInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.Memory.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2Gi"),
		Max: resource.MustParse("1Gi"),
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory.bounds.min")
	assert.Contains(t, err.Error(), "must be <= memory.bounds.max")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryModeWithoutConfig(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Mode = "Canary"
	policy.Spec.UpdateStrategy.Canary = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "updateStrategy.canary is required when mode is Canary")
	assert.Empty(t, warnings)
}

func TestValidate_NoTargetRef(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targetRef must specify either name or selector")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryDecreaseWarning(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	allowDecrease := true
	policy.Spec.Memory.AllowDecrease = &allowDecrease

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "memory.allowDecrease is enabled")
	assert.Contains(t, warnings[0], "OOMKill risk")
}

func TestValidateDelete_AlwaysSucceeds(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateDelete(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}
