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
// Per-resource fields (Percentile, Overhead, MinAllowed/MaxAllowed,
// BurstSensitivity, LimitMultiplier) are NOT set here. LimitMultiplier has
// no built-in default: omitted keeps the live request-to-limit ratio.
// The other fields are handled at their usage sites in
// buildRecommendationEngines. A set oomBump or surge block is the
// exception: unset inners are filled, and a nil block stays nil.
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
	if policy.Spec.Memory.DecreaseUsageMarginPercent == nil {
		v := attunev1alpha1.DefaultDecreaseUsageMarginPercent
		policy.Spec.Memory.DecreaseUsageMarginPercent = &v
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{
			Duration: mustParseBuiltInDuration(attunev1alpha1.DefaultCooldown),
		}
	}
	if policy.Spec.UpdateStrategy.AutoRevert == nil {
		v := attunev1alpha1.DefaultAutoRevert
		policy.Spec.UpdateStrategy.AutoRevert = &v
	}
	if policy.Spec.UpdateStrategy.MaxConcurrentResizes == 0 {
		policy.Spec.UpdateStrategy.MaxConcurrentResizes = attunev1alpha1.DefaultMaxConcurrentResizes
	}
	if policy.Spec.UpdateStrategy.ResizeMethod == "" {
		policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.DefaultResizeMethod
	}
	if policy.Spec.MetricsSource.MinimumDataPoints == nil {
		v := attunev1alpha1.DefaultMinimumDataPoints
		policy.Spec.MetricsSource.MinimumDataPoints = &v
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{
			Duration: mustParseBuiltInDuration(attunev1alpha1.DefaultHistoryWindow),
		}
	}
	if policy.Spec.MetricsSource.QueryStep == nil {
		policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: attunev1alpha1.DefaultQueryStep}
	}
	if policy.Spec.MetricsSource.PodAggregation == "" {
		policy.Spec.MetricsSource.PodAggregation = attunev1alpha1.DefaultPodAggregation
	}
	if policy.Spec.CPU.ControlledValues == nil {
		cv := attunev1alpha1.DefaultControlledValues
		policy.Spec.CPU.ControlledValues = &cv
	}
	if policy.Spec.Memory.ControlledValues == nil {
		cv := attunev1alpha1.DefaultControlledValues
		policy.Spec.Memory.ControlledValues = &cv
	}
	if policy.Spec.ExcludeKnownSidecars == nil {
		v := attunev1alpha1.DefaultExcludeKnownSidecars
		policy.Spec.ExcludeKnownSidecars = &v
	}
	applyRuntimeProfileDefaults(policy)
	applySurgeDefaults(policy.Spec.CPU.Surge)
	applySurgeDefaults(policy.Spec.Memory.Surge)
	applyOOMBumpDefaults(policy.Spec.Memory.OOMBump)
}

// applySurgeDefaults fills empty inners on a surge block.
// A nil block stays nil, so the feature stays off. CPU and memory both fill.
func applySurgeDefaults(block *attunev1alpha1.Surge) {
	if block == nil {
		return
	}
	if block.TriggerRatio == "" {
		block.TriggerRatio = attunev1alpha1.DefaultSurgeTriggerRatio
	}
	if block.Percentile == nil {
		v := attunev1alpha1.DefaultSurgePercentile
		block.Percentile = &v
	}
	if block.Window == nil {
		block.Window = &metav1.Duration{Duration: attunev1alpha1.DefaultSurgeWindow}
	}
}

// applyOOMBumpDefaults fills nil fields on a memory oomBump block.
// A nil block stays nil, so the feature stays off. CPU is not filled.
func applyOOMBumpDefaults(block *attunev1alpha1.OOMBump) {
	if block == nil {
		return
	}
	if block.Ratio == nil {
		v := attunev1alpha1.DefaultOOMBumpRatio
		block.Ratio = &v
	}
	if block.MinBump == nil {
		q := attunev1alpha1.DefaultOOMBumpMinBump.DeepCopy()
		block.MinBump = &q
	}
	if block.MaxBumps == nil {
		v := attunev1alpha1.DefaultOOMBumpMaxBumps
		block.MaxBumps = &v
	}
	if block.Hold == nil {
		block.Hold = &metav1.Duration{Duration: attunev1alpha1.DefaultOOMBumpHold}
	}
}

// applyRuntimeProfileDefaults fills unset resource fields from the optional
// runtimeProfile. Explicit policy fields always win.
func applyRuntimeProfileDefaults(policy *attunev1alpha1.AttunePolicy) {
	switch policy.Spec.RuntimeProfile {
	case "", "generic":
		return
	case "java":
		// JVM heaps often ignore live cgroup memory decreases; prefer no
		// memory decrease and slightly higher memory overhead when unset.
		if policy.Spec.Memory.AllowDecrease == nil {
			v := false
			policy.Spec.Memory.AllowDecrease = &v
		}
		if policy.Spec.Memory.Overhead == "" {
			policy.Spec.Memory.Overhead = "40"
		}
	case "python", "nodejs":
		if policy.Spec.Memory.AllowDecrease == nil {
			v := false
			policy.Spec.Memory.AllowDecrease = &v
		}
	case "golang":
		// Go runtimes generally adapt; leave allowDecrease unset (platform default).
		return
	}
}

// mustParseBuiltInDuration parses a package-level default duration constant.
// It panics on error because the constants are fixed strings in this module;
// a parse failure means a broken default was introduced at build time, not a
// runtime user input problem.
func mustParseBuiltInDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic("invalid built-in default duration " + s + ": " + err.Error())
	}
	return d
}

// CombineDefaultsLayers builds effective defaults from optional cluster and
// namespace layers. Namespace fields win; cluster fills fields left unset on
// the namespace object. Returns nil if both layers are nil.
//
// Precedence when both exist: cluster < namespace (policy still overrides later
// via MergeDefaults). CostPricing and all other Spec fields follow the same rule.
func CombineDefaultsLayers(cluster, namespace *attunev1alpha1.AttuneDefaults) *attunev1alpha1.AttuneDefaults {
	if namespace == nil {
		return cluster
	}
	if cluster == nil {
		return namespace
	}

	// Seed a synthetic policy with namespace values (higher priority), then
	// MergeDefaults fills gaps from cluster (lower priority).
	policy := &attunev1alpha1.AttunePolicy{}
	applyDefaultsSpecToPolicy(policy, namespace.Spec)
	// false: this fold is two defaults layers, not a policy. A namespace
	// RequestsOnly must not drop a cluster limitMultiplier.
	_ = mergeDefaults(policy, cluster, false)

	out := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: namespace.ObjectMeta,
		Spec:       policyToDefaultsSpec(policy, pickCostPricing(namespace.Spec.CostPricing, cluster.Spec.CostPricing)),
	}
	return out
}

// applyDefaultsSpecToPolicy copies set fields from a defaults Spec onto a policy.
func applyDefaultsSpecToPolicy(policy *attunev1alpha1.AttunePolicy, spec attunev1alpha1.AttuneDefaultsSpec) {
	if spec.CPU != nil {
		policy.Spec.CPU = *spec.CPU.DeepCopy()
	}
	if spec.Memory != nil {
		policy.Spec.Memory = *spec.Memory.DeepCopy()
	}
	if spec.MetricsSource != nil {
		policy.Spec.MetricsSource = *spec.MetricsSource.DeepCopy()
	}
	if spec.UpdateStrategy != nil {
		policy.Spec.UpdateStrategy = spec.UpdateStrategy.DeepCopy()
	}
	if spec.ExcludeKnownSidecars != nil {
		v := *spec.ExcludeKnownSidecars
		policy.Spec.ExcludeKnownSidecars = &v
	}
}

// policyToDefaultsSpec converts a fully merged policy Spec back into defaults Spec.
// CostPricing is not stored on AttunePolicy and is passed separately.
func policyToDefaultsSpec(policy *attunev1alpha1.AttunePolicy, cost *attunev1alpha1.CostPricing) attunev1alpha1.AttuneDefaultsSpec {
	spec := attunev1alpha1.AttuneDefaultsSpec{CostPricing: cost}
	// ResourceConfig.DeepCopy and MetricsSource.DeepCopy return pointers.
	spec.CPU = policy.Spec.CPU.DeepCopy()
	spec.Memory = policy.Spec.Memory.DeepCopy()
	spec.MetricsSource = policy.Spec.MetricsSource.DeepCopy()
	if policy.Spec.UpdateStrategy != nil {
		spec.UpdateStrategy = policy.Spec.UpdateStrategy.DeepCopy()
	}
	if policy.Spec.ExcludeKnownSidecars != nil {
		v := *policy.Spec.ExcludeKnownSidecars
		spec.ExcludeKnownSidecars = &v
	}
	return spec
}

func pickCostPricing(namespace, cluster *attunev1alpha1.CostPricing) *attunev1alpha1.CostPricing {
	if namespace == nil && cluster == nil {
		return nil
	}
	out := &attunev1alpha1.CostPricing{}
	switch {
	case namespace != nil && namespace.CPUPerCoreHour != "":
		out.CPUPerCoreHour = namespace.CPUPerCoreHour
	case cluster != nil && cluster.CPUPerCoreHour != "":
		out.CPUPerCoreHour = cluster.CPUPerCoreHour
	}
	switch {
	case namespace != nil && namespace.MemoryPerGiBHour != "":
		out.MemoryPerGiBHour = namespace.MemoryPerGiBHour
	case cluster != nil && cluster.MemoryPerGiBHour != "":
		out.MemoryPerGiBHour = cluster.MemoryPerGiBHour
	}
	if out.CPUPerCoreHour == "" && out.MemoryPerGiBHour == "" {
		return nil
	}
	return out
}

// MergeDefaults merges values from an AttuneDefaults resource into the
// policy where the policy has not specified its own values. Returns the
// list of field names that were inherited (for debug logging by callers).
func MergeDefaults(policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) []string {
	return mergeDefaults(policy, defaults, true)
}

// mergeDefaults copies unset policy fields from defaults. skipRequestsOnlyMultiplier
// is true for a real policy: an explicit RequestsOnly block does not copy
// limitMultiplier. CombineDefaultsLayers passes false so the cluster multiplier
// stays on the effective defaults object.
func mergeDefaults(policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults, skipRequestsOnlyMultiplier bool) []string {
	if defaults == nil {
		return nil
	}
	spec := defaults.Spec

	inherited := make([]string, 0, 4) //nolint:mnd // 4 merge sections: cpu, memory, metrics, strategy
	inherited = append(inherited, MergeResourceConfig(&policy.Spec.CPU, spec.CPU, "cpu", skipRequestsOnlyMultiplier)...)
	inherited = append(inherited, MergeResourceConfig(&policy.Spec.Memory, spec.Memory, "memory", skipRequestsOnlyMultiplier)...)
	inherited = append(inherited, MergeMetricsSource(&policy.Spec.MetricsSource, spec.MetricsSource)...)
	if policy.Spec.UpdateStrategy == nil {
		policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	}
	inherited = append(inherited, MergeUpdateStrategy(policy.Spec.UpdateStrategy, spec.UpdateStrategy)...)
	if policy.Spec.ExcludeKnownSidecars == nil && spec.ExcludeKnownSidecars != nil {
		policy.Spec.ExcludeKnownSidecars = spec.ExcludeKnownSidecars
		inherited = append(inherited, "excludeKnownSidecars")
	}
	return inherited
}

// EffectiveExcludedContainers returns the set of container names that must
// be skipped for recommendations and resizes. When excludeKnownSidecars is
// true (default), the built-in known-sidecar list is unioned with
// policy.Spec.ExcludedContainers. When false, only the policy list applies.
//
// Call ApplyBuiltInDefaults (or ensure ExcludeKnownSidecars is non-nil)
// before relying on the default-true behavior; a nil pointer is treated as
// true so hot paths that skip defaulting still get the safe default.
func EffectiveExcludedContainers(policy *attunev1alpha1.AttunePolicy) map[string]bool {
	if policy == nil {
		return map[string]bool{}
	}
	knownOn := true
	if policy.Spec.ExcludeKnownSidecars != nil {
		knownOn = *policy.Spec.ExcludeKnownSidecars
	}
	n := len(policy.Spec.ExcludedContainers)
	if knownOn {
		n += len(attunev1alpha1.KnownSidecarContainers)
	}
	set := make(map[string]bool, n)
	if knownOn {
		for _, name := range attunev1alpha1.KnownSidecarContainers {
			set[name] = true
		}
	}
	for _, name := range policy.Spec.ExcludedContainers {
		set[name] = true
	}
	return set
}

// ExclusionReason returns a short reason string for why a container name
// is excluded. Callers should only use this when the name is present in
// EffectiveExcludedContainers. When the known list is on and the name is
// both a known sidecar and listed in excludedContainers, the known-list
// reason wins.
func ExclusionReason(policy *attunev1alpha1.AttunePolicy, containerName string) string {
	knownOn := true
	if policy != nil && policy.Spec.ExcludeKnownSidecars != nil {
		knownOn = *policy.Spec.ExcludeKnownSidecars
	}
	if knownOn {
		for _, name := range attunev1alpha1.KnownSidecarContainers {
			if name == containerName {
				return "known sidecar auto-exclude"
			}
		}
	}
	return "listed in excludedContainers"
}

// MergeResourceConfig merges default resource config values into the policy.
// skipRequestsOnlyMultiplier skips limitMultiplier only when this block was
// already RequestsOnly before controlledValues is copied from defaults.
func MergeResourceConfig(policy *attunev1alpha1.ResourceConfig, defaults *attunev1alpha1.ResourceConfig, prefix string, skipRequestsOnlyMultiplier bool) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	explicitRequestsOnly := policy.ControlledValues != nil && *policy.ControlledValues == attunev1alpha1.ControlledRequestsOnly
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
	// An empty multiplier is unset. The pre-copy mode still copies when a
	// real policy omitted controlledValues and defaults are RequestsOnly.
	// Folding defaults layers passes skipRequestsOnlyMultiplier false.
	if !(skipRequestsOnlyMultiplier && explicitRequestsOnly) &&
		(policy.LimitMultiplier == nil || *policy.LimitMultiplier == "") &&
		defaults.LimitMultiplier != nil && *defaults.LimitMultiplier != "" {
		policy.LimitMultiplier = defaults.LimitMultiplier
		inherited = append(inherited, prefix+".limitMultiplier")
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
	if policy.DecreaseUsageMarginPercent == nil && defaults.DecreaseUsageMarginPercent != nil {
		policy.DecreaseUsageMarginPercent = defaults.DecreaseUsageMarginPercent
		inherited = append(inherited, prefix+".decreaseUsageMarginPercent")
	}
	inherited = append(inherited, mergeOOMBump(policy, defaults, prefix)...)
	inherited = append(inherited, mergeSurge(policy, defaults, prefix)...)
	return inherited
}

// mergeSurge copies a defaults block when the policy omits surge.
// An empty policy block stays on and fills only unset fields from defaults.
func mergeSurge(policy, defaults *attunev1alpha1.ResourceConfig, prefix string) []string {
	if defaults == nil || defaults.Surge == nil {
		return nil
	}
	if policy.Surge == nil {
		// Copy. ApplyBuiltInDefaults fills empty inners on this block, and
		// explain reads the defaults object to decide which inners were set.
		policy.Surge = defaults.Surge.DeepCopy()
		return []string{prefix + ".surge"}
	}
	var inherited []string
	dst := policy.Surge
	src := defaults.Surge
	if dst.TriggerRatio == "" && src.TriggerRatio != "" {
		dst.TriggerRatio = src.TriggerRatio
		inherited = append(inherited, prefix+".surge.triggerRatio")
	}
	if dst.Percentile == nil && src.Percentile != nil {
		v := *src.Percentile
		dst.Percentile = &v
		inherited = append(inherited, prefix+".surge.percentile")
	}
	if dst.Window == nil && src.Window != nil {
		w := *src.Window
		dst.Window = &w
		inherited = append(inherited, prefix+".surge.window")
	}
	return inherited
}

// mergeOOMBump copies a defaults block when the policy omits oomBump.
// An empty policy block stays on and fills only nil fields from defaults.
func mergeOOMBump(policy, defaults *attunev1alpha1.ResourceConfig, prefix string) []string {
	if defaults == nil || defaults.OOMBump == nil {
		return nil
	}
	if policy.OOMBump == nil {
		// Copy. ApplyBuiltInDefaults fills nil inners on this block, and
		// explain reads the defaults object to decide which inners were set.
		policy.OOMBump = defaults.OOMBump.DeepCopy()
		return []string{prefix + ".oomBump"}
	}
	var inherited []string
	dst := policy.OOMBump
	src := defaults.OOMBump
	if dst.Ratio == nil && src.Ratio != nil && *src.Ratio != "" {
		v := *src.Ratio
		dst.Ratio = &v
		inherited = append(inherited, prefix+".oomBump.ratio")
	}
	if dst.MinBump == nil && src.MinBump != nil {
		q := src.MinBump.DeepCopy()
		dst.MinBump = &q
		inherited = append(inherited, prefix+".oomBump.minBump")
	}
	if dst.MaxBumps == nil && src.MaxBumps != nil {
		v := *src.MaxBumps
		dst.MaxBumps = &v
		inherited = append(inherited, prefix+".oomBump.maxBumps")
	}
	if dst.Hold == nil && src.Hold != nil {
		h := *src.Hold
		dst.Hold = &h
		inherited = append(inherited, prefix+".oomBump.hold")
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
	if policy.PodAggregation == "" && defaults.PodAggregation != "" {
		policy.PodAggregation = defaults.PodAggregation
		inherited = append(inherited, "podAggregation")
	}
	if policy.CPURecordingMetric == "" && defaults.CPURecordingMetric != "" {
		policy.CPURecordingMetric = defaults.CPURecordingMetric
		inherited = append(inherited, "cpuRecordingMetric")
	}
	if policy.MemoryRecordingMetric == "" && defaults.MemoryRecordingMetric != "" {
		policy.MemoryRecordingMetric = defaults.MemoryRecordingMetric
		inherited = append(inherited, "memoryRecordingMetric")
	}
	// Provider blocks are mutually exclusive. Inherit only when the policy
	// did not set any of them so a cluster Datadog/CloudWatch/VPA/Prometheus
	// default is not dropped (and we never add a second provider).
	policyHasProvider := policy.Prometheus != nil || policy.Datadog != nil ||
		policy.CloudWatch != nil || policy.VPA != nil
	if !policyHasProvider {
		switch {
		case defaults.Prometheus != nil:
			policy.Prometheus = defaults.Prometheus
			inherited = append(inherited, "prometheus")
		case defaults.Datadog != nil:
			policy.Datadog = defaults.Datadog
			inherited = append(inherited, "datadog")
		case defaults.CloudWatch != nil:
			policy.CloudWatch = defaults.CloudWatch
			inherited = append(inherited, "cloudwatch")
		case defaults.VPA != nil:
			policy.VPA = defaults.VPA
			inherited = append(inherited, "vpa")
		}
	}
	return inherited
}

// MergeUpdateStrategy merges default update strategy values into the policy.
func MergeUpdateStrategy(policy *attunev1alpha1.UpdateStrategy, defaults *attunev1alpha1.UpdateStrategy) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.Type == "" && defaults.Type != "" {
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
	if policy.MaxStatusRecommendations == nil && defaults.MaxStatusRecommendations != nil {
		policy.MaxStatusRecommendations = defaults.MaxStatusRecommendations
		inherited = append(inherited, "maxStatusRecommendations")
	}
	if policy.IncludeExplanationsInStatus == nil && defaults.IncludeExplanationsInStatus != nil {
		policy.IncludeExplanationsInStatus = defaults.IncludeExplanationsInStatus
		inherited = append(inherited, "includeExplanationsInStatus")
	}
	if policy.MaxTotalCPUIncrease == nil && defaults.MaxTotalCPUIncrease != nil {
		policy.MaxTotalCPUIncrease = defaults.MaxTotalCPUIncrease
		inherited = append(inherited, "maxTotalCpuIncrease")
	}
	if policy.MaxTotalMemoryIncrease == nil && defaults.MaxTotalMemoryIncrease != nil {
		policy.MaxTotalMemoryIncrease = defaults.MaxTotalMemoryIncrease
		inherited = append(inherited, "maxTotalMemoryIncrease")
	}
	if policy.MaxCPUIncreasePerMinute == nil && defaults.MaxCPUIncreasePerMinute != nil {
		policy.MaxCPUIncreasePerMinute = defaults.MaxCPUIncreasePerMinute
		inherited = append(inherited, "maxCpuIncreasePerMinute")
	}
	if policy.MaxMemoryIncreasePerMinute == nil && defaults.MaxMemoryIncreasePerMinute != nil {
		policy.MaxMemoryIncreasePerMinute = defaults.MaxMemoryIncreasePerMinute
		inherited = append(inherited, "maxMemoryIncreasePerMinute")
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
	if policy.TemplatePersistence == nil && defaults.TemplatePersistence != nil {
		policy.TemplatePersistence = defaults.TemplatePersistence
		inherited = append(inherited, "templatePersistence")
	}
	inherited = append(inherited, mergeHPATargetBounds(policy, defaults)...)
	return inherited
}

// mergeHPATargetBounds copies a defaults band when the policy omits it.
// An empty policy band stays empty and fills only unset sides and pointers.
// Copies do not share min or max pointers with the defaults object.
func mergeHPATargetBounds(policy, defaults *attunev1alpha1.UpdateStrategy) []string {
	if defaults == nil || defaults.HPATargetBounds == nil {
		return nil
	}
	if policy.HPATargetBounds == nil {
		copied := defaults.HPATargetBounds.DeepCopy()
		if hpaBoundInverted(copied.CPU) {
			copied.CPU = nil
		}
		if hpaBoundInverted(copied.Memory) {
			copied.Memory = nil
		}
		if copied.CPU == nil && copied.Memory == nil {
			return nil
		}
		policy.HPATargetBounds = copied
		return []string{"hpaTargetBounds"}
	}
	cpu := mergeHPATargetBound(&policy.HPATargetBounds.CPU, defaults.HPATargetBounds.CPU, "hpaTargetBounds.cpu")
	memory := mergeHPATargetBound(&policy.HPATargetBounds.Memory, defaults.HPATargetBounds.Memory, "hpaTargetBounds.memory")
	inherited := make([]string, 0, len(cpu)+len(memory))
	inherited = append(inherited, cpu...)
	inherited = append(inherited, memory...)
	return inherited
}

func mergeHPATargetBound(dst **attunev1alpha1.HPATargetBound, src *attunev1alpha1.HPATargetBound, prefix string) []string {
	if src == nil {
		return nil
	}
	if *dst == nil {
		copied := src.DeepCopy()
		// Admission checks one object. A stored pair with min above max
		// must not become the effective band.
		if hpaBoundInverted(copied) {
			return nil
		}
		*dst = copied
		return []string{prefix}
	}
	var inherited []string
	// Copy a missing side only when the pair stays ordered. A policy max
	// plus a higher defaults min, or the reverse, would pass admission on
	// each object and then clamp the wrong way.
	if (*dst).Min == nil && src.Min != nil && !hpaMinAboveMax(src.Min, (*dst).Max) {
		v := *src.Min
		(*dst).Min = &v
		inherited = append(inherited, prefix+".min")
	}
	if (*dst).Max == nil && src.Max != nil && !hpaMinAboveMax((*dst).Min, src.Max) {
		v := *src.Max
		(*dst).Max = &v
		inherited = append(inherited, prefix+".max")
	}
	return inherited
}

func hpaBoundInverted(b *attunev1alpha1.HPATargetBound) bool {
	return b != nil && hpaMinAboveMax(b.Min, b.Max)
}

func hpaMinAboveMax(min, max *int32) bool {
	return min != nil && max != nil && *min > *max
}
