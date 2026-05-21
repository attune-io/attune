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

package defaults

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

func TestApplyBuiltInDefaults_FillsAllFields(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{}
	ApplyBuiltInDefaults(policy)

	assert.Equal(t, rightsizev1alpha1.DefaultUpdateMode, policy.Spec.UpdateStrategy.Mode)
	assert.NotNil(t, policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	assert.Equal(t, rightsizev1alpha1.DefaultMaxCPUChangePercent, *policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	assert.NotNil(t, policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
	assert.Equal(t, rightsizev1alpha1.DefaultMaxMemoryChangePercent, *policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
	assert.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, rightsizev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, rightsizev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
	assert.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, rightsizev1alpha1.DefaultQueryStep, policy.Spec.MetricsSource.QueryStep.Duration)
	assert.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, rightsizev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	assert.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, rightsizev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
}

func TestApplyBuiltInDefaults_DoesNotOverrideExistingValues(t *testing.T) {
	mode := rightsizev1alpha1.UpdateModeAuto
	maxCPU := int32(25)
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                mode,
				MaxCPUChangePercent: &maxCPU,
			},
		},
	}
	ApplyBuiltInDefaults(policy)

	assert.Equal(t, mode, policy.Spec.UpdateStrategy.Mode)
	assert.Equal(t, int32(25), *policy.Spec.UpdateStrategy.MaxCPUChangePercent)
}

func TestMergeDefaults_NilDefaultsIsNoOp(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{Percentile: 95},
		},
	}
	inherited := MergeDefaults(policy, nil)
	assert.Empty(t, inherited)
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
}

func TestMergeDefaults_InheritsUnsetFields(t *testing.T) {
	cooldown := &metav1.Duration{Duration: 2 * time.Hour}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "1.5",
			},
			Memory: &rightsizev1alpha1.ResourceConfig{
				Percentile: 95,
			},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Cooldown: cooldown,
			},
		},
	}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	inherited := MergeDefaults(policy, defaults)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, cooldown, policy.Spec.UpdateStrategy.Cooldown)
	assert.Contains(t, inherited, "cpu.percentile")
	assert.Contains(t, inherited, "cpu.safetyMargin")
	assert.Contains(t, inherited, "memory.percentile")
	assert.Contains(t, inherited, "cooldown")
}

func TestMergeDefaults_PolicyFieldsTakePrecedence(t *testing.T) {
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "1.5",
			},
		},
	}
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.1",
			},
		},
	}

	inherited := MergeDefaults(policy, defaults)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.1", policy.Spec.CPU.SafetyMargin)
	assert.Empty(t, inherited)
}

func TestMergeUpdateStrategy_AllFields(t *testing.T) {
	autoRevert := true
	maxCPU := int32(40)
	maxMem := int32(20)
	maxConc := int32(3)
	defaults := &rightsizev1alpha1.UpdateStrategy{
		Mode:                   rightsizev1alpha1.UpdateModeAuto,
		AutoRevert:             &autoRevert,
		ResizeMethod:           rightsizev1alpha1.ResizeMethodInPlaceOrEvict,
		MaxCPUChangePercent:    &maxCPU,
		MaxMemoryChangePercent: &maxMem,
		MaxConcurrentResizes:   maxConc,
		Cooldown:               &metav1.Duration{Duration: 30 * time.Minute},
	}
	policy := &rightsizev1alpha1.UpdateStrategy{}

	inherited := MergeUpdateStrategy(policy, defaults)

	assert.Equal(t, rightsizev1alpha1.UpdateModeAuto, policy.Mode)
	assert.True(t, *policy.AutoRevert)
	assert.Equal(t, rightsizev1alpha1.ResizeMethodInPlaceOrEvict, policy.ResizeMethod)
	assert.Equal(t, int32(40), *policy.MaxCPUChangePercent)
	assert.Equal(t, int32(20), *policy.MaxMemoryChangePercent)
	assert.Equal(t, int32(3), policy.MaxConcurrentResizes)
	assert.NotNil(t, policy.Cooldown)
	assert.Contains(t, inherited, "mode")
	assert.Contains(t, inherited, "autoRevert")
	assert.Contains(t, inherited, "resizeMethod")
	assert.Contains(t, inherited, "maxCpuChangePercent")
	assert.Contains(t, inherited, "maxMemoryChangePercent")
	assert.Contains(t, inherited, "maxConcurrentResizes")
	assert.Contains(t, inherited, "cooldown")
}
