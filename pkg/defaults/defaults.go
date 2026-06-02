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

// Package defaults provides shared default-value and merge logic for
// AttunePolicy fields. Both the controller (internal/controller) and
// the kubectl plugin (cmd/kubectl-attune) use these functions so
// their defaulting behavior stays in sync.
package defaults

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// ApplyBuiltInDefaults fills strategy and metrics fields still unset after
// MergeDefaults with the operator's built-in default values. This runs
// AFTER MergeDefaults so that cluster-wide AttuneDefaults take precedence.
//
// Per-resource fields (Percentile, Overhead, MinAllowed/MaxAllowed, BurstSensitivity)
// are NOT set here; they are handled defensively at their usage sites in
// buildRecommendationEngines.
func ApplyBuiltInDefaults(policy *attunev1alpha1.AttunePolicy) {
	if policy.Spec.UpdateStrategy == nil {
		policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	}
	if policy.Spec.UpdateStrategy.Type == "" {
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.DefaultUpdateType
	}
	if policy.Spec.CPU.MaxChangePercent == nil {
		v := attunev1alpha1.DefaultCPUMaxChangePercent
		policy.Spec.CPU.MaxChangePercent = &v
	}
	if policy.Spec.Memory.MaxChangePercent == nil {
		v := attunev1alpha1.DefaultMemoryMaxChangePercent
		policy.Spec.Memory.MaxChangePercent = &v
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		d, _ := time.ParseDuration(attunev1alpha1.DefaultCooldown)
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: d}
	}
	if policy.Spec.UpdateStrategy.AutoRevert == nil {
		v := attunev1alpha1.DefaultAutoRevert
		policy.Spec.UpdateStrategy.AutoRevert = &v
	}
	if policy.Spec.UpdateStrategy.ResizeMethod == "" {
		policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.DefaultResizeMethod
	}
	if policy.Spec.MetricsSource.MinimumDataPoints == nil {
		v := attunev1alpha1.DefaultMinimumDataPoints
		policy.Spec.MetricsSource.MinimumDataPoints = &v
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		d, _ := time.ParseDuration(attunev1alpha1.DefaultHistoryWindow)
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: d}
	}
	if policy.Spec.MetricsSource.QueryStep == nil {
		policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: attunev1alpha1.DefaultQueryStep}
	}
	if policy.Spec.CPU.ControlledValues == nil {
		cv := attunev1alpha1.DefaultControlledValues
		policy.Spec.CPU.ControlledValues = &cv
	}
	if policy.Spec.Memory.ControlledValues == nil {
		cv := attunev1alpha1.DefaultControlledValues
		policy.Spec.Memory.ControlledValues = &cv
	}
}

// MergeDefaults merges values from an AttuneDefaults resource into the
// policy where the policy has not specified its own values. Returns the
// list of field names that were inherited (for debug logging by callers).
func MergeDefaults(policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) []string {
	if defaults == nil {
		return nil
	}
	spec := defaults.Spec

	inherited := make([]string, 0, 4) //nolint:mnd // 4 merge sections: cpu, memory, metrics, strategy
	inherited = append(inherited, MergeResourceConfig(&policy.Spec.CPU, spec.CPU, "cpu")...)
	inherited = append(inherited, MergeResourceConfig(&policy.Spec.Memory, spec.Memory, "memory")...)
	inherited = append(inherited, MergeMetricsSource(&policy.Spec.MetricsSource, spec.MetricsSource)...)
	if policy.Spec.UpdateStrategy == nil {
		policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	}
	inherited = append(inherited, MergeUpdateStrategy(policy.Spec.UpdateStrategy, spec.UpdateStrategy)...)
	return inherited
}

// MergeResourceConfig merges default resource config values into the policy.
func MergeResourceConfig(policy *attunev1alpha1.ResourceConfig, defaults *attunev1alpha1.ResourceConfig, prefix string) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.Percentile == 0 && defaults.Percentile != 0 {
		policy.Percentile = defaults.Percentile
		inherited = append(inherited, prefix+".percentile")
	}
	if policy.Overhead == "" && defaults.Overhead != "" {
		policy.Overhead = defaults.Overhead
		inherited = append(inherited, prefix+".overhead")
	}
	if policy.MinAllowed == nil && defaults.MinAllowed != nil {
		policy.MinAllowed = defaults.MinAllowed
		inherited = append(inherited, prefix+".minAllowed")
	}
	if policy.MaxAllowed == nil && defaults.MaxAllowed != nil {
		policy.MaxAllowed = defaults.MaxAllowed
		inherited = append(inherited, prefix+".maxAllowed")
	}
	if policy.ControlledValues == nil && defaults.ControlledValues != nil {
		policy.ControlledValues = defaults.ControlledValues
		inherited = append(inherited, prefix+".controlledValues")
	}
	if policy.BurstSensitivity == nil && defaults.BurstSensitivity != nil {
		policy.BurstSensitivity = defaults.BurstSensitivity
		inherited = append(inherited, prefix+".burstSensitivity")
	}
	if policy.AllowDecrease == nil && defaults.AllowDecrease != nil {
		policy.AllowDecrease = defaults.AllowDecrease
		inherited = append(inherited, prefix+".allowDecrease")
	}
	if policy.MemoryFromCPURatio == nil && defaults.MemoryFromCPURatio != nil {
		policy.MemoryFromCPURatio = defaults.MemoryFromCPURatio
		inherited = append(inherited, prefix+".memoryFromCpuRatio")
	}
	if policy.StartupBoost == nil && defaults.StartupBoost != nil {
		policy.StartupBoost = defaults.StartupBoost
		inherited = append(inherited, prefix+".startupBoost")
	}
	if policy.MaxChangePercent == nil && defaults.MaxChangePercent != nil {
		policy.MaxChangePercent = defaults.MaxChangePercent
		inherited = append(inherited, prefix+".maxChangePercent")
	}
	if policy.MaxIncreasePercent == nil && defaults.MaxIncreasePercent != nil {
		policy.MaxIncreasePercent = defaults.MaxIncreasePercent
		inherited = append(inherited, prefix+".maxIncreasePercent")
	}
	if policy.MaxDecreasePercent == nil && defaults.MaxDecreasePercent != nil {
		policy.MaxDecreasePercent = defaults.MaxDecreasePercent
		inherited = append(inherited, prefix+".maxDecreasePercent")
	}
	return inherited
}

// MergeMetricsSource merges default metrics source values into the policy.
func MergeMetricsSource(policy *attunev1alpha1.MetricsSource, defaults *attunev1alpha1.MetricsSource) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.HistoryWindow == nil && defaults.HistoryWindow != nil {
		policy.HistoryWindow = defaults.HistoryWindow
		inherited = append(inherited, "historyWindow")
	}
	if policy.MinimumDataPoints == nil && defaults.MinimumDataPoints != nil {
		policy.MinimumDataPoints = defaults.MinimumDataPoints
		inherited = append(inherited, "minimumDataPoints")
	}
	if policy.QueryStep == nil && defaults.QueryStep != nil {
		policy.QueryStep = defaults.QueryStep
		inherited = append(inherited, "queryStep")
	}
	if policy.RateWindow == nil && defaults.RateWindow != nil {
		policy.RateWindow = defaults.RateWindow
		inherited = append(inherited, "rateWindow")
	}
	return inherited
}

// MergeUpdateStrategy merges default update strategy values into the policy.
func MergeUpdateStrategy(policy *attunev1alpha1.UpdateStrategy, defaults *attunev1alpha1.UpdateStrategy) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.Type == "" {
		policy.Type = defaults.Type
		inherited = append(inherited, "type")
	}
	if policy.Cooldown == nil && defaults.Cooldown != nil {
		policy.Cooldown = defaults.Cooldown
		inherited = append(inherited, "cooldown")
	}
	if policy.AutoRevert == nil && defaults.AutoRevert != nil {
		policy.AutoRevert = defaults.AutoRevert
		inherited = append(inherited, "autoRevert")
	}
	if policy.ResizeMethod == "" && defaults.ResizeMethod != "" {
		policy.ResizeMethod = defaults.ResizeMethod
		inherited = append(inherited, "resizeMethod")
	}
	if policy.InitialSizing == nil && defaults.InitialSizing != nil {
		policy.InitialSizing = defaults.InitialSizing
		inherited = append(inherited, "initialSizing")
	}
	if policy.MaxConcurrentResizes == 0 && defaults.MaxConcurrentResizes != 0 {
		policy.MaxConcurrentResizes = defaults.MaxConcurrentResizes
		inherited = append(inherited, "maxConcurrentResizes")
	}
	if policy.MaxTotalCPUIncrease == nil && defaults.MaxTotalCPUIncrease != nil {
		policy.MaxTotalCPUIncrease = defaults.MaxTotalCPUIncrease
		inherited = append(inherited, "maxTotalCpuIncrease")
	}
	if policy.MaxTotalMemoryIncrease == nil && defaults.MaxTotalMemoryIncrease != nil {
		policy.MaxTotalMemoryIncrease = defaults.MaxTotalMemoryIncrease
		inherited = append(inherited, "maxTotalMemoryIncrease")
	}
	if policy.Schedule == nil && defaults.Schedule != nil {
		policy.Schedule = defaults.Schedule
		inherited = append(inherited, "schedule")
	}
	if policy.Export == nil && defaults.Export != nil {
		policy.Export = defaults.Export
		inherited = append(inherited, "export")
	}
	if policy.Canary == nil && defaults.Canary != nil {
		policy.Canary = defaults.Canary
		inherited = append(inherited, "canary")
	}
	if policy.SafetyObservationPeriod == nil && defaults.SafetyObservationPeriod != nil {
		policy.SafetyObservationPeriod = defaults.SafetyObservationPeriod
		inherited = append(inherited, "safetyObservationPeriod")
	}
	if len(policy.SLOGuardrails) == 0 && len(defaults.SLOGuardrails) > 0 {
		policy.SLOGuardrails = defaults.SLOGuardrails
		inherited = append(inherited, "sloGuardrails")
	}
	return inherited
}
