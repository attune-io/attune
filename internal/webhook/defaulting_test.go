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

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

func TestDefault_SetsPercentiles(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
}

func TestDefault_PreservesExisting(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	policy.Spec.CPU.Percentile = 90
	policy.Spec.CPU.SafetyMargin = "1.5"

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
	// Unset fields should still get defaults
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "1.3", policy.Spec.Memory.SafetyMargin)
}

func TestDefault_SetsMode(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, "Recommend", policy.Spec.UpdateStrategy.Mode)
	assert.Equal(t, int32(50), policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	assert.Equal(t, int32(30), policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
}

func TestDefault_SetsWeight(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, int32(100), policy.Spec.Weight)
}