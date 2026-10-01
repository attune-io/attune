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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mustQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func qtyPtr(t *testing.T, s string) *resource.Quantity {
	t.Helper()
	q := mustQty(t, s)
	return &q
}

func policyWithCPUMax(t *testing.T, max string) *AttunePolicy {
	t.Helper()
	return &AttunePolicy{
		Spec: AttunePolicySpec{
			CPU: ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
				MaxAllowed: qtyPtr(t, max),
			},
			Memory: ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
			},
		},
	}
}

func TestEffectiveContainerResources_EmptyListReturnsPolicyValues(t *testing.T) {
	t.Parallel()
	for _, list := range [][]ContainerResourcePolicy{nil, {}} {
		policy := policyWithCPUMax(t, "200m")
		policy.Spec.ContainerPolicies = list
		cpu, mem := EffectiveContainerResources(policy, "sidecar")
		assert.Equal(t, policy.Spec.CPU.MaxAllowed, cpu.MaxAllowed)
		assert.Equal(t, int32(95), cpu.Percentile)
		assert.Equal(t, "20", cpu.Overhead)
		assert.Equal(t, int32(99), mem.Percentile)
		appCPU, _ := EffectiveContainerResources(policy, "app")
		assert.Equal(t, policy.Spec.CPU.MaxAllowed, appCPU.MaxAllowed)
	}
}

func TestEffectiveContainerResources_NonEmptyDeepCopyDoesNotAlias(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	sideMax := qtyPtr(t, "200m")
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &ResourceConfig{MaxAllowed: sideMax},
	}}
	cpu, _ := EffectiveContainerResources(policy, "sidecar")
	require.NotNil(t, cpu.MaxAllowed)
	cpu.MaxAllowed.SetMilli(1)
	assert.Equal(t, int64(4000), policy.Spec.CPU.MaxAllowed.MilliValue())
	assert.Equal(t, int64(200), policy.Spec.ContainerPolicies[0].CPU.MaxAllowed.MilliValue())
	assert.Equal(t, int64(1), cpu.MaxAllowed.MilliValue())
}

func TestEffectiveContainerResources_PercentileZeroInheritsOverheadZeroDoesNot(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{
		{
			ContainerName: "sidecar",
			CPU:           &ResourceConfig{Percentile: 0, Overhead: "0"},
		},
		{
			ContainerName: "app",
			CPU:           &ResourceConfig{Percentile: 50},
		},
	}
	side, _ := EffectiveContainerResources(policy, "sidecar")
	assert.Equal(t, int32(95), side.Percentile)
	assert.Equal(t, "0", side.Overhead)
	app, _ := EffectiveContainerResources(policy, "app")
	assert.Equal(t, int32(50), app.Percentile)
	assert.Equal(t, "20", app.Overhead)
	other, _ := EffectiveContainerResources(policy, "main")
	assert.Equal(t, int32(95), other.Percentile)
	assert.Equal(t, "20", other.Overhead)
}

func TestEffectiveContainerResources_WildcardThenLiteral(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	ral := ControlledRequestsAndLimits
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{
		{
			ContainerName: ContainerPolicyWildcard,
			CPU:           &ResourceConfig{MaxAllowed: qtyPtr(t, "300m")},
		},
		{
			ContainerName: "app",
			CPU:           &ResourceConfig{ControlledValues: &ral},
		},
		{
			ContainerName: "sidecar",
			CPU:           &ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
		},
	}
	app, _ := EffectiveContainerResources(policy, "app")
	assert.Equal(t, int64(300), app.MaxAllowed.MilliValue())
	require.NotNil(t, app.ControlledValues)
	assert.Equal(t, ControlledRequestsAndLimits, *app.ControlledValues)
	assert.Equal(t, int32(95), app.Percentile)
	side, _ := EffectiveContainerResources(policy, "sidecar")
	assert.Equal(t, int64(200), side.MaxAllowed.MilliValue())
	assert.Equal(t, int32(95), side.Percentile)
	other, _ := EffectiveContainerResources(policy, "main")
	assert.Equal(t, int64(300), other.MaxAllowed.MilliValue())
	star, _ := EffectiveContainerResources(policy, ContainerPolicyWildcard)
	assert.Equal(t, int64(300), star.MaxAllowed.MilliValue())
	assert.Equal(t, int32(95), star.Percentile)
}

func TestEffectiveContainerResources_HonoredFieldsAndNilAllowDecrease(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	burst := "0.5"
	inc := int32(80)
	dec := int32(40)
	change := int32(100)
	allow := false
	memAllow := true
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{{
		ContainerName: "app",
		CPU: &ResourceConfig{
			MinAllowed:         qtyPtr(t, "100m"),
			BurstSensitivity:   &burst,
			MaxChangePercent:   &change,
			MaxIncreasePercent: &inc,
			MaxDecreasePercent: &dec,
			AllowDecrease:      &allow,
		},
		Memory: &ResourceConfig{AllowDecrease: &memAllow},
	}}
	cpu, mem := EffectiveContainerResources(policy, "app")
	assert.Equal(t, int64(100), cpu.MinAllowed.MilliValue())
	require.NotNil(t, cpu.BurstSensitivity)
	assert.Equal(t, "0.5", *cpu.BurstSensitivity)
	require.NotNil(t, cpu.MaxChangePercent)
	assert.Equal(t, int32(100), *cpu.MaxChangePercent)
	require.NotNil(t, cpu.MaxIncreasePercent)
	assert.Equal(t, int32(80), *cpu.MaxIncreasePercent)
	require.NotNil(t, cpu.MaxDecreasePercent)
	assert.Equal(t, int32(40), *cpu.MaxDecreasePercent)
	require.NotNil(t, cpu.AllowDecrease)
	assert.False(t, *cpu.AllowDecrease)
	require.NotNil(t, mem.AllowDecrease)
	assert.True(t, *mem.AllowDecrease)

	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{{
		ContainerName: "app",
		CPU:           &ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	cpu, mem = EffectiveContainerResources(policy, "app")
	assert.Nil(t, cpu.AllowDecrease)
	assert.Nil(t, mem.AllowDecrease)
}

func TestEffectiveContainerResources_PolicyWideFieldsStayOnPolicy(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	ratio := "2"
	margin := int32(5)
	mult := "3"
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{{
		ContainerName: "app",
		CPU: &ResourceConfig{
			StartupBoost:    &StartupBoost{Multiplier: "2", Duration: metav1.Duration{Duration: time.Minute}},
			LimitMultiplier: &mult,
			Surge:           &Surge{TriggerRatio: "2"},
		},
		Memory: &ResourceConfig{
			MemoryFromCPURatio:         &ratio,
			DecreaseUsageMarginPercent: &margin,
			OOMBump:                    &OOMBump{Ratio: &ratio},
		},
	}}
	cpu, mem := EffectiveContainerResources(policy, "app")
	assert.Nil(t, cpu.StartupBoost)
	assert.Nil(t, cpu.LimitMultiplier)
	assert.Nil(t, cpu.Surge)
	assert.Nil(t, mem.MemoryFromCPURatio)
	assert.Nil(t, mem.DecreaseUsageMarginPercent)
	assert.Nil(t, mem.OOMBump)

	keep := &StartupBoost{Multiplier: "4", Duration: metav1.Duration{Duration: time.Minute}}
	policy.Spec.CPU.StartupBoost = keep
	cpu, _ = EffectiveContainerResources(policy, "app")
	require.NotNil(t, cpu.StartupBoost)
	assert.Equal(t, "4", cpu.StartupBoost.Multiplier)
}

func TestEffectiveContainerResources_FirstDuplicateWins(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{
		{ContainerName: "app", CPU: &ResourceConfig{MaxAllowed: qtyPtr(t, "100m")}},
		{ContainerName: "app", CPU: &ResourceConfig{MaxAllowed: qtyPtr(t, "500m")}},
	}
	cpu, _ := EffectiveContainerResources(policy, "app")
	assert.Equal(t, int64(100), cpu.MaxAllowed.MilliValue())
}

func TestEffectiveContainerResources_NilPolicy(t *testing.T) {
	t.Parallel()
	cpu, mem := EffectiveContainerResources(nil, "app")
	assert.Equal(t, ResourceConfig{}, cpu)
	assert.Equal(t, ResourceConfig{}, mem)
	assert.Nil(t, ExplainContainerPolicies(nil))
}

func TestExplainContainerPolicies_Sources(t *testing.T) {
	t.Parallel()
	policy := policyWithCPUMax(t, "4000m")
	ral := ControlledRequestsAndLimits
	policy.Spec.ContainerPolicies = []ContainerResourcePolicy{
		{ContainerName: "sidecar", CPU: &ResourceConfig{MaxAllowed: qtyPtr(t, "200m")}},
		{ContainerName: "app", CPU: &ResourceConfig{ControlledValues: &ral}},
		{ContainerName: ContainerPolicyWildcard, CPU: &ResourceConfig{MaxAllowed: qtyPtr(t, "300m")}},
	}
	rows := ExplainContainerPolicies(policy)
	require.Len(t, rows, 3)
	assert.Equal(t, ContainerPolicySourceContainer, rows[0].CPUSources.MaxAllowed)
	assert.Equal(t, int64(200), rows[0].CPU.MaxAllowed.MilliValue())
	assert.Equal(t, ContainerPolicySourcePolicy, rows[0].CPUSources.Percentile)
	assert.Equal(t, ContainerPolicySourceWildcard, rows[1].CPUSources.MaxAllowed)
	assert.Equal(t, int64(300), rows[1].CPU.MaxAllowed.MilliValue())
	assert.Equal(t, ContainerPolicySourceContainer, rows[1].CPUSources.ControlledValues)
	assert.Equal(t, ContainerPolicySourceContainer, rows[2].CPUSources.MaxAllowed)
	assert.Equal(t, int64(300), rows[2].CPU.MaxAllowed.MilliValue())
	assert.Nil(t, ExplainContainerPolicies(&AttunePolicy{}))
}
