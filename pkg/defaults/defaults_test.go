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
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func ptrInt32(v int32) *int32 { return &v }
func ptrBool(v bool) *bool    { return &v }
func ptrStr(v string) *string { return &v }

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestApplyBuiltInDefaults_FillsAllFields(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	ApplyBuiltInDefaults(policy)

	assert.Equal(t, attunev1alpha1.DefaultUpdateType, policy.Spec.UpdateStrategy.Type)
	assert.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultCPUMaxChangePercent, *policy.Spec.CPU.MaxChangePercent)
	assert.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultMemoryMaxChangePercent, *policy.Spec.Memory.MaxChangePercent)
	assert.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, attunev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
	assert.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, attunev1alpha1.DefaultQueryStep, policy.Spec.MetricsSource.QueryStep.Duration)
	assert.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	assert.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.True(t, *policy.Spec.ExcludeKnownSidecars)
	assert.Equal(t, attunev1alpha1.DefaultExcludeKnownSidecars, *policy.Spec.ExcludeKnownSidecars)
}

func TestApplyBuiltInDefaults_DoesNotOverrideExistingValues(t *testing.T) {
	mode := attunev1alpha1.UpdateTypeAuto
	maxCPU := int32(25)
	falseVal := false
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: mode,
			},
			CPU: attunev1alpha1.ResourceConfig{
				MaxChangePercent: &maxCPU,
			},
			ExcludeKnownSidecars: &falseVal,
		},
	}
	ApplyBuiltInDefaults(policy)

	assert.Equal(t, mode, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(25), *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.False(t, *policy.Spec.ExcludeKnownSidecars)
}

func TestMergeDefaults_NilDefaultsIsNoOp(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{Percentile: 95},
		},
	}
	inherited := MergeDefaults(policy, nil)
	assert.Empty(t, inherited)
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
}

func TestMergeDefaults_InheritsUnsetFields(t *testing.T) {
	cooldown := &metav1.Duration{Duration: 2 * time.Hour}
	excludeKnown := false
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "50",
			},
			Memory: &attunev1alpha1.ResourceConfig{
				Percentile: 95,
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: cooldown,
			},
			ExcludeKnownSidecars: &excludeKnown,
		},
	}
	policy := &attunev1alpha1.AttunePolicy{}

	inherited := MergeDefaults(policy, defaults)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "50", policy.Spec.CPU.Overhead)
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, cooldown, policy.Spec.UpdateStrategy.Cooldown)
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.False(t, *policy.Spec.ExcludeKnownSidecars)
	assert.Contains(t, inherited, "cpu.percentile")
	assert.Contains(t, inherited, "cpu.overhead")
	assert.Contains(t, inherited, "memory.percentile")
	assert.Contains(t, inherited, "cooldown")
	assert.Contains(t, inherited, "excludeKnownSidecars")
}

func TestMergeDefaults_ExcludeKnownSidecarsPolicyTakesPrecedence(t *testing.T) {
	defaultsFalse := false
	policyTrue := true
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			ExcludeKnownSidecars: &defaultsFalse,
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ExcludeKnownSidecars: &policyTrue,
		},
	}
	inherited := MergeDefaults(policy, defaults)
	assert.NotContains(t, inherited, "excludeKnownSidecars")
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.True(t, *policy.Spec.ExcludeKnownSidecars)
}

func TestMergeDefaults_PolicyFieldsTakePrecedence(t *testing.T) {
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "50",
			},
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "10",
			},
		},
	}

	inherited := MergeDefaults(policy, defaults)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "10", policy.Spec.CPU.Overhead)
	assert.Empty(t, inherited)
}

func TestMergeUpdateStrategy_AllFields(t *testing.T) {
	autoRevert := true
	maxConc := int32(3)
	maxTotalCPU := resource.MustParse("2000m")
	maxTotalMem := resource.MustParse("4Gi")
	defaults := &attunev1alpha1.UpdateStrategy{
		Type:                    attunev1alpha1.UpdateTypeAuto,
		AutoRevert:              &autoRevert,
		ResizeMethod:            attunev1alpha1.ResizeMethodInPlaceOrRecreate,
		MaxConcurrentResizes:    maxConc,
		Cooldown:                &metav1.Duration{Duration: 30 * time.Minute},
		MaxTotalCPUIncrease:     &maxTotalCPU,
		MaxTotalMemoryIncrease:  &maxTotalMem,
		Schedule:                &attunev1alpha1.ResizeSchedule{Timezone: "UTC"},
		Export:                  &attunev1alpha1.ExportConfig{ConfigMap: true},
		Canary:                  &attunev1alpha1.CanaryConfig{Percentage: 10, ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute}},
		SafetyObservationPeriod: &metav1.Duration{Duration: 10 * time.Minute},
		SLOGuardrails: []attunev1alpha1.SLOGuardrail{
			{Name: "p99-latency", Query: "histogram_quantile(0.99, rate(http_duration_seconds_bucket[5m]))", Threshold: "0.5"},
		},
	}
	policy := &attunev1alpha1.UpdateStrategy{}

	inherited := MergeUpdateStrategy(policy, defaults)

	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Type)
	assert.True(t, *policy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.ResizeMethod)
	assert.Equal(t, int32(3), policy.MaxConcurrentResizes)
	assert.NotNil(t, policy.Cooldown)
	require.NotNil(t, policy.MaxTotalCPUIncrease)
	assert.True(t, policy.MaxTotalCPUIncrease.Equal(resource.MustParse("2000m")))
	require.NotNil(t, policy.MaxTotalMemoryIncrease)
	assert.Equal(t, "4Gi", policy.MaxTotalMemoryIncrease.String())
	require.NotNil(t, policy.Schedule)
	assert.Equal(t, "UTC", policy.Schedule.Timezone)
	require.NotNil(t, policy.Export)
	assert.True(t, policy.Export.ConfigMap)
	require.NotNil(t, policy.Canary)
	assert.Equal(t, int32(10), policy.Canary.Percentage)
	assert.Equal(t, 5*time.Minute, policy.Canary.ObservationPeriod.Duration)
	require.NotNil(t, policy.SafetyObservationPeriod)
	assert.Equal(t, 10*time.Minute, policy.SafetyObservationPeriod.Duration)
	require.Len(t, policy.SLOGuardrails, 1)
	assert.Equal(t, "p99-latency", policy.SLOGuardrails[0].Name)
	expectedInherited := []string{
		"type",
		"autoRevert",
		"resizeMethod",
		"maxConcurrentResizes",
		"cooldown",
		"maxTotalCpuIncrease",
		"maxTotalMemoryIncrease",
		"schedule",
		"export",
		"canary",
		"safetyObservationPeriod",
		"sloGuardrails",
	}
	assert.ElementsMatch(t, expectedInherited, inherited)
}

// ---------- Direct MergeResourceConfig tests ----------

func TestMergeResourceConfig_AllFields(t *testing.T) {
	defaults := &attunev1alpha1.ResourceConfig{
		Percentile:         90,
		Overhead:           "50",
		MinAllowed:         quantityPtr("50m"),
		MaxAllowed:         quantityPtr("4000m"),
		ControlledValues:   ptrStr("RequestsAndLimits"),
		BurstSensitivity:   ptrStr("0.3"),
		AllowDecrease:      ptrBool(true),
		MemoryFromCPURatio: ptrStr("2.0"),
		StartupBoost:       &attunev1alpha1.StartupBoost{Multiplier: "3.0", Duration: metav1.Duration{Duration: 2 * time.Minute}},
		MaxChangePercent:   ptrInt32(50),
		MaxIncreasePercent: ptrInt32(60),
		MaxDecreasePercent: ptrInt32(30),
	}
	policy := &attunev1alpha1.ResourceConfig{}

	inherited := MergeResourceConfig(policy, defaults, "cpu")

	assert.Equal(t, int32(90), policy.Percentile)
	assert.Equal(t, "50", policy.Overhead)
	require.NotNil(t, policy.MinAllowed)
	assert.Equal(t, resource.MustParse("50m"), *policy.MinAllowed)
	assert.Equal(t, resource.MustParse("4000m"), *policy.MaxAllowed)
	require.NotNil(t, policy.ControlledValues)
	assert.Equal(t, "RequestsAndLimits", *policy.ControlledValues)
	require.NotNil(t, policy.BurstSensitivity)
	assert.Equal(t, "0.3", *policy.BurstSensitivity)
	require.NotNil(t, policy.AllowDecrease)
	assert.True(t, *policy.AllowDecrease)
	require.NotNil(t, policy.MemoryFromCPURatio)
	assert.Equal(t, "2.0", *policy.MemoryFromCPURatio)
	require.NotNil(t, policy.StartupBoost)
	assert.Equal(t, "3.0", policy.StartupBoost.Multiplier)
	assert.Equal(t, 2*time.Minute, policy.StartupBoost.Duration.Duration)
	require.NotNil(t, policy.MaxChangePercent)
	assert.Equal(t, int32(50), *policy.MaxChangePercent)
	require.NotNil(t, policy.MaxIncreasePercent)
	assert.Equal(t, int32(60), *policy.MaxIncreasePercent)
	require.NotNil(t, policy.MaxDecreasePercent)
	assert.Equal(t, int32(30), *policy.MaxDecreasePercent)
	expectedInherited := []string{
		"cpu.percentile",
		"cpu.overhead",
		"cpu.minAllowed",
		"cpu.maxAllowed",
		"cpu.controlledValues",
		"cpu.burstSensitivity",
		"cpu.allowDecrease",
		"cpu.memoryFromCpuRatio",
		"cpu.startupBoost",
		"cpu.maxChangePercent",
		"cpu.maxIncreasePercent",
		"cpu.maxDecreasePercent",
	}
	assert.ElementsMatch(t, expectedInherited, inherited)
}

func TestMergeResourceConfig_NilDefaultsIsNoOp(t *testing.T) {
	policy := &attunev1alpha1.ResourceConfig{Percentile: 95}
	inherited := MergeResourceConfig(policy, nil, "cpu")
	assert.Empty(t, inherited)
	assert.Equal(t, int32(95), policy.Percentile)
}

func TestMergeResourceConfig_PolicyFieldsTakePrecedence(t *testing.T) {
	defaults := &attunev1alpha1.ResourceConfig{
		Percentile:         90,
		Overhead:           "50",
		ControlledValues:   ptrStr("RequestsAndLimits"),
		BurstSensitivity:   ptrStr("0.5"),
		AllowDecrease:      ptrBool(true),
		MemoryFromCPURatio: ptrStr("2.0"),
		StartupBoost:       &attunev1alpha1.StartupBoost{Multiplier: "3.0"},
		MinAllowed:         quantityPtr("50m"),
		MaxChangePercent:   ptrInt32(50),
		MaxIncreasePercent: ptrInt32(60),
		MaxDecreasePercent: ptrInt32(30),
	}
	policy := &attunev1alpha1.ResourceConfig{
		Percentile:         99,
		Overhead:           "10",
		ControlledValues:   ptrStr("RequestsOnly"),
		BurstSensitivity:   ptrStr("0.1"),
		AllowDecrease:      ptrBool(false),
		MemoryFromCPURatio: ptrStr("4.0"),
		StartupBoost:       &attunev1alpha1.StartupBoost{Multiplier: "2.0"},
		MinAllowed:         quantityPtr("100m"),
		MaxChangePercent:   ptrInt32(40),
		MaxIncreasePercent: ptrInt32(70),
		MaxDecreasePercent: ptrInt32(20),
	}

	inherited := MergeResourceConfig(policy, defaults, "memory")

	assert.Empty(t, inherited)
	assert.Equal(t, int32(99), policy.Percentile)
	assert.Equal(t, "10", policy.Overhead)
	assert.Equal(t, "RequestsOnly", *policy.ControlledValues)
	assert.Equal(t, "0.1", *policy.BurstSensitivity)
	assert.False(t, *policy.AllowDecrease)
	assert.Equal(t, "4.0", *policy.MemoryFromCPURatio)
	assert.Equal(t, "2.0", policy.StartupBoost.Multiplier)
	assert.Equal(t, resource.MustParse("100m"), *policy.MinAllowed)
	assert.Equal(t, int32(40), *policy.MaxChangePercent)
	assert.Equal(t, int32(70), *policy.MaxIncreasePercent)
	assert.Equal(t, int32(20), *policy.MaxDecreasePercent)
}

func TestMergeResourceConfig_PrefixAppliedCorrectly(t *testing.T) {
	defaults := &attunev1alpha1.ResourceConfig{Percentile: 90}
	policy := &attunev1alpha1.ResourceConfig{}

	cpuInherited := MergeResourceConfig(policy, defaults, "cpu")
	assert.Contains(t, cpuInherited, "cpu.percentile")

	policy2 := &attunev1alpha1.ResourceConfig{}
	memInherited := MergeResourceConfig(policy2, defaults, "memory")
	assert.Contains(t, memInherited, "memory.percentile")
}

// ---------- Direct MergeMetricsSource tests ----------

func TestMergeMetricsSource_AllFields(t *testing.T) {
	defaults := &attunev1alpha1.MetricsSource{
		HistoryWindow:     &metav1.Duration{Duration: 336 * time.Hour},
		MinimumDataPoints: ptrInt32(96),
		QueryStep:         &metav1.Duration{Duration: 10 * time.Minute},
		RateWindow:        &metav1.Duration{Duration: 15 * time.Minute},
	}
	policy := &attunev1alpha1.MetricsSource{}

	inherited := MergeMetricsSource(policy, defaults)

	require.NotNil(t, policy.HistoryWindow)
	assert.Equal(t, 336*time.Hour, policy.HistoryWindow.Duration)
	require.NotNil(t, policy.MinimumDataPoints)
	assert.Equal(t, int32(96), *policy.MinimumDataPoints)
	require.NotNil(t, policy.QueryStep)
	assert.Equal(t, 10*time.Minute, policy.QueryStep.Duration)
	require.NotNil(t, policy.RateWindow)
	assert.Equal(t, 15*time.Minute, policy.RateWindow.Duration)
	assert.Len(t, inherited, 4)
	assert.Contains(t, inherited, "historyWindow")
	assert.Contains(t, inherited, "minimumDataPoints")
	assert.Contains(t, inherited, "queryStep")
	assert.Contains(t, inherited, "rateWindow")
}

func TestMergeMetricsSource_NilDefaultsIsNoOp(t *testing.T) {
	step := &metav1.Duration{Duration: 5 * time.Minute}
	policy := &attunev1alpha1.MetricsSource{QueryStep: step}

	inherited := MergeMetricsSource(policy, nil)

	assert.Empty(t, inherited)
	assert.Equal(t, step, policy.QueryStep)
}

func TestMergeMetricsSource_PolicyFieldsTakePrecedence(t *testing.T) {
	defaults := &attunev1alpha1.MetricsSource{
		HistoryWindow:     &metav1.Duration{Duration: 336 * time.Hour},
		MinimumDataPoints: ptrInt32(96),
		QueryStep:         &metav1.Duration{Duration: 10 * time.Minute},
		RateWindow:        &metav1.Duration{Duration: 15 * time.Minute},
	}
	policy := &attunev1alpha1.MetricsSource{
		HistoryWindow:     &metav1.Duration{Duration: 168 * time.Hour},
		MinimumDataPoints: ptrInt32(48),
		QueryStep:         &metav1.Duration{Duration: 5 * time.Minute},
		RateWindow:        &metav1.Duration{Duration: 10 * time.Minute},
	}

	inherited := MergeMetricsSource(policy, defaults)

	assert.Empty(t, inherited)
	assert.Equal(t, 168*time.Hour, policy.HistoryWindow.Duration)
	assert.Equal(t, int32(48), *policy.MinimumDataPoints)
	assert.Equal(t, 5*time.Minute, policy.QueryStep.Duration)
	assert.Equal(t, 10*time.Minute, policy.RateWindow.Duration)
}

func TestMergeMetricsSource_PartialInheritance(t *testing.T) {
	defaults := &attunev1alpha1.MetricsSource{
		HistoryWindow:     &metav1.Duration{Duration: 336 * time.Hour},
		MinimumDataPoints: ptrInt32(96),
		QueryStep:         &metav1.Duration{Duration: 10 * time.Minute},
		RateWindow:        &metav1.Duration{Duration: 15 * time.Minute},
	}
	policy := &attunev1alpha1.MetricsSource{
		QueryStep: &metav1.Duration{Duration: 5 * time.Minute},
	}

	inherited := MergeMetricsSource(policy, defaults)

	assert.Len(t, inherited, 3)
	assert.Contains(t, inherited, "historyWindow")
	assert.Contains(t, inherited, "minimumDataPoints")
	assert.Contains(t, inherited, "rateWindow")
	assert.NotContains(t, inherited, "queryStep")
	assert.Equal(t, 5*time.Minute, policy.QueryStep.Duration)
}

// ---------- MergeUpdateStrategy edge cases ----------

func TestMergeUpdateStrategy_NilDefaultsIsNoOp(t *testing.T) {
	policy := &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto}
	inherited := MergeUpdateStrategy(policy, nil)
	assert.Empty(t, inherited)
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Type)
}

func TestMergeUpdateStrategy_PolicyFieldsTakePrecedence(t *testing.T) {
	defaults := &attunev1alpha1.UpdateStrategy{
		Type:                    attunev1alpha1.UpdateTypeAuto,
		Cooldown:                &metav1.Duration{Duration: 30 * time.Minute},
		AutoRevert:              ptrBool(false),
		ResizeMethod:            attunev1alpha1.ResizeMethodInPlaceOrRecreate,
		MaxConcurrentResizes:    5,
		SafetyObservationPeriod: &metav1.Duration{Duration: 10 * time.Minute},
		Schedule:                &attunev1alpha1.ResizeSchedule{Timezone: "UTC"},
		Export:                  &attunev1alpha1.ExportConfig{ConfigMap: true},
		Canary:                  &attunev1alpha1.CanaryConfig{Percentage: 10},
	}
	policy := &attunev1alpha1.UpdateStrategy{
		Type:                    attunev1alpha1.UpdateTypeRecommend,
		Cooldown:                &metav1.Duration{Duration: time.Hour},
		AutoRevert:              ptrBool(true),
		ResizeMethod:            attunev1alpha1.ResizeMethodInPlaceOnly,
		MaxConcurrentResizes:    3,
		SafetyObservationPeriod: &metav1.Duration{Duration: 5 * time.Minute},
		Schedule:                &attunev1alpha1.ResizeSchedule{Timezone: "America/New_York"},
		Export:                  &attunev1alpha1.ExportConfig{ConfigMap: false},
		Canary:                  &attunev1alpha1.CanaryConfig{Percentage: 20},
	}

	inherited := MergeUpdateStrategy(policy, defaults)

	assert.Empty(t, inherited)
	assert.Equal(t, attunev1alpha1.UpdateTypeRecommend, policy.Type)
	assert.Equal(t, time.Hour, policy.Cooldown.Duration)
	assert.True(t, *policy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOnly, policy.ResizeMethod)
	assert.Equal(t, int32(3), policy.MaxConcurrentResizes)
	assert.Equal(t, 5*time.Minute, policy.SafetyObservationPeriod.Duration)
	assert.Equal(t, "America/New_York", policy.Schedule.Timezone)
	assert.False(t, policy.Export.ConfigMap)
	assert.Equal(t, int32(20), policy.Canary.Percentage)
}

func TestMergeUpdateStrategy_PartialInheritance(t *testing.T) {
	defaults := &attunev1alpha1.UpdateStrategy{
		Type:                 attunev1alpha1.UpdateTypeAuto,
		Cooldown:             &metav1.Duration{Duration: 30 * time.Minute},
		MaxConcurrentResizes: 5,
		Schedule:             &attunev1alpha1.ResizeSchedule{Timezone: "UTC"},
	}
	policy := &attunev1alpha1.UpdateStrategy{
		Type:     attunev1alpha1.UpdateTypeRecommend,
		Cooldown: &metav1.Duration{Duration: time.Hour},
	}

	inherited := MergeUpdateStrategy(policy, defaults)

	assert.Len(t, inherited, 2)
	assert.Contains(t, inherited, "maxConcurrentResizes")
	assert.Contains(t, inherited, "schedule")
	assert.NotContains(t, inherited, "type")
	assert.NotContains(t, inherited, "cooldown")
	assert.Equal(t, attunev1alpha1.UpdateTypeRecommend, policy.Type)
	assert.Equal(t, time.Hour, policy.Cooldown.Duration)
}

func TestMergeUpdateStrategy_BothEmptyTypeNoInheritance(t *testing.T) {
	// When both policy.Type and defaults.Type are empty strings,
	// Type should NOT be reported as inherited (it should remain empty
	// for ApplyBuiltInDefaults to fill in later).
	defaults := &attunev1alpha1.UpdateStrategy{
		Type:     "",
		Cooldown: &metav1.Duration{Duration: 30 * time.Minute},
	}
	policy := &attunev1alpha1.UpdateStrategy{
		Type: "",
	}

	inherited := MergeUpdateStrategy(policy, defaults)

	assert.NotContains(t, inherited, "type", "empty default Type should not be inherited")
	assert.Contains(t, inherited, "cooldown")
	assert.Equal(t, attunev1alpha1.UpdateType(""), policy.Type,
		"Type should remain empty when default is also empty")
}

func TestMustParseBuiltInDuration_ValidConstants(t *testing.T) {
	// Guard against typoed default strings silently becoming zero durations.
	cooldown := mustParseBuiltInDuration(attunev1alpha1.DefaultCooldown)
	assert.Equal(t, time.Hour, cooldown)
	history := mustParseBuiltInDuration(attunev1alpha1.DefaultHistoryWindow)
	assert.Equal(t, 168*time.Hour, history)
}

func TestMustParseBuiltInDuration_InvalidPanics(t *testing.T) {
	assert.Panics(t, func() {
		mustParseBuiltInDuration("not-a-duration")
	})
}
