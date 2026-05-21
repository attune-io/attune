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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

// fetchDefaults returns the effective defaults for the given namespace, checking
// namespace-scoped RightSizeNamespaceDefaults first, then falling back to
// cluster-scoped RightSizeDefaults. Returns nil if neither exists.
//
// If multiple defaults objects exist at the same scope, selection is
// deterministic: the lexicographically smallest metadata.name wins.
func (r *RightSizePolicyReconciler) fetchDefaults(ctx context.Context, namespace string) (*rightsizev1alpha1.RightSizeDefaults, error) {
	// Check namespace-scoped defaults first.
	var nsList rightsizev1alpha1.RightSizeNamespaceDefaultsList
	if err := r.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing RightSizeNamespaceDefaults in %s: %w", namespace, err)
	}
	if len(nsList.Items) > 0 {
		nsDefaults := nsList.Items[0]
		for i := 1; i < len(nsList.Items); i++ {
			if nsList.Items[i].Name < nsDefaults.Name {
				nsDefaults = nsList.Items[i]
			}
		}
		// Convert to RightSizeDefaults so callers don't need to know the source.
		return &rightsizev1alpha1.RightSizeDefaults{
			ObjectMeta: nsDefaults.ObjectMeta,
			Spec:       nsDefaults.Spec,
		}, nil
	}

	// Fall back to cluster-scoped defaults.
	var clusterList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &clusterList); err != nil {
		return nil, fmt.Errorf("listing RightSizeDefaults: %w", err)
	}
	if len(clusterList.Items) == 0 {
		return nil, nil
	}
	clusterDefaults := &clusterList.Items[0]
	for i := 1; i < len(clusterList.Items); i++ {
		if clusterList.Items[i].Name < clusterDefaults.Name {
			clusterDefaults = &clusterList.Items[i]
		}
	}
	return clusterDefaults, nil
}

// applyBuiltInDefaults fills strategy and metrics fields still unset after
// mergeDefaults with the operator's built-in default values. This runs AFTER
// mergeDefaults so that cluster-wide RightSizeDefaults take precedence.
//
// Per-resource fields (Percentile, SafetyMargin, Bounds, BurstSensitivity)
// are NOT set here; they are handled defensively at their usage sites in
// buildRecommendationEngines.
func (r *RightSizePolicyReconciler) applyBuiltInDefaults(policy *rightsizev1alpha1.RightSizePolicy) {
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.DefaultUpdateMode
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == nil {
		v := rightsizev1alpha1.DefaultMaxCPUChangePercent
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = &v
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == nil {
		v := rightsizev1alpha1.DefaultMaxMemoryChangePercent
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = &v
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		d, _ := time.ParseDuration(rightsizev1alpha1.DefaultCooldown)
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: d}
	}
	if policy.Spec.UpdateStrategy.AutoRevert == nil {
		v := rightsizev1alpha1.DefaultAutoRevert
		policy.Spec.UpdateStrategy.AutoRevert = &v
	}
	if policy.Spec.UpdateStrategy.ResizeMethod == "" {
		policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.DefaultResizeMethod
	}
	if policy.Spec.MetricsSource.MinimumDataPoints == nil {
		v := rightsizev1alpha1.DefaultMinimumDataPoints
		policy.Spec.MetricsSource.MinimumDataPoints = &v
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		d, _ := time.ParseDuration(rightsizev1alpha1.DefaultHistoryWindow)
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: d}
	}
	if policy.Spec.MetricsSource.QueryStep == nil {
		policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: defaultPrometheusStep}
	}
	if policy.Spec.CPU.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.CPU.ControlledValues = &cv
	}
	if policy.Spec.Memory.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.Memory.ControlledValues = &cv
	}
}

// mergeDefaults merges values from RightSizeDefaults into the policy where
// the policy has not specified its own values.
func (r *RightSizePolicyReconciler) mergeDefaults(policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) {
	if defaults == nil {
		ctrl.Log.V(1).Info("No cluster defaults configured, using built-in values only")
		return
	}
	spec := defaults.Spec

	// Track which fields are inherited for debug logging.
	var inherited []string

	// Merge CPU config
	inherited = append(inherited, mergeResourceConfig(&policy.Spec.CPU, spec.CPU, "cpu")...)

	// Merge Memory config
	inherited = append(inherited, mergeResourceConfig(&policy.Spec.Memory, spec.Memory, "memory")...)

	// Merge MetricsSource
	if spec.MetricsSource != nil {
		if policy.Spec.MetricsSource.HistoryWindow == nil && spec.MetricsSource.HistoryWindow != nil {
			policy.Spec.MetricsSource.HistoryWindow = spec.MetricsSource.HistoryWindow
			inherited = append(inherited, "historyWindow")
		}
		if policy.Spec.MetricsSource.MinimumDataPoints == nil && spec.MetricsSource.MinimumDataPoints != nil {
			policy.Spec.MetricsSource.MinimumDataPoints = spec.MetricsSource.MinimumDataPoints
			inherited = append(inherited, "minimumDataPoints")
		}
		if policy.Spec.MetricsSource.QueryStep == nil && spec.MetricsSource.QueryStep != nil {
			policy.Spec.MetricsSource.QueryStep = spec.MetricsSource.QueryStep
			inherited = append(inherited, "queryStep")
		}
		if policy.Spec.MetricsSource.RateWindow == nil && spec.MetricsSource.RateWindow != nil {
			policy.Spec.MetricsSource.RateWindow = spec.MetricsSource.RateWindow
			inherited = append(inherited, "rateWindow")
		}
	}

	// Merge UpdateStrategy
	if spec.UpdateStrategy != nil {
		if policy.Spec.UpdateStrategy.Mode == "" {
			policy.Spec.UpdateStrategy.Mode = spec.UpdateStrategy.Mode
			inherited = append(inherited, "mode")
		}
		if policy.Spec.UpdateStrategy.Cooldown == nil && spec.UpdateStrategy.Cooldown != nil {
			policy.Spec.UpdateStrategy.Cooldown = spec.UpdateStrategy.Cooldown
			inherited = append(inherited, "cooldown")
		}
		if policy.Spec.UpdateStrategy.AutoRevert == nil && spec.UpdateStrategy.AutoRevert != nil {
			policy.Spec.UpdateStrategy.AutoRevert = spec.UpdateStrategy.AutoRevert
			inherited = append(inherited, "autoRevert")
		}
		if policy.Spec.UpdateStrategy.ResizeMethod == "" && spec.UpdateStrategy.ResizeMethod != "" {
			policy.Spec.UpdateStrategy.ResizeMethod = spec.UpdateStrategy.ResizeMethod
			inherited = append(inherited, "resizeMethod")
		}
		if policy.Spec.UpdateStrategy.MaxCPUChangePercent == nil && spec.UpdateStrategy.MaxCPUChangePercent != nil {
			policy.Spec.UpdateStrategy.MaxCPUChangePercent = spec.UpdateStrategy.MaxCPUChangePercent
			inherited = append(inherited, "maxCpuChangePercent")
		}
		if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == nil && spec.UpdateStrategy.MaxMemoryChangePercent != nil {
			policy.Spec.UpdateStrategy.MaxMemoryChangePercent = spec.UpdateStrategy.MaxMemoryChangePercent
			inherited = append(inherited, "maxMemoryChangePercent")
		}
		if policy.Spec.UpdateStrategy.MaxConcurrentResizes == 0 && spec.UpdateStrategy.MaxConcurrentResizes != 0 {
			policy.Spec.UpdateStrategy.MaxConcurrentResizes = spec.UpdateStrategy.MaxConcurrentResizes
			inherited = append(inherited, "maxConcurrentResizes")
		}
		if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease == nil && spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
			policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = spec.UpdateStrategy.MaxTotalCPUIncrease
			inherited = append(inherited, "maxTotalCpuIncrease")
		}
		if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease == nil && spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
			policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = spec.UpdateStrategy.MaxTotalMemoryIncrease
			inherited = append(inherited, "maxTotalMemoryIncrease")
		}
		if policy.Spec.UpdateStrategy.Schedule == nil && spec.UpdateStrategy.Schedule != nil {
			policy.Spec.UpdateStrategy.Schedule = spec.UpdateStrategy.Schedule
			inherited = append(inherited, "schedule")
		}
		if policy.Spec.UpdateStrategy.Export == nil && spec.UpdateStrategy.Export != nil {
			policy.Spec.UpdateStrategy.Export = spec.UpdateStrategy.Export
			inherited = append(inherited, "export")
		}
		if policy.Spec.UpdateStrategy.Canary == nil && spec.UpdateStrategy.Canary != nil {
			policy.Spec.UpdateStrategy.Canary = spec.UpdateStrategy.Canary
			inherited = append(inherited, "canary")
		}
	}

	if len(inherited) > 0 {
		ctrl.Log.V(1).Info("Merged cluster defaults into policy",
			"defaultsName", defaults.Name,
			"fieldsInherited", inherited)
	} else {
		ctrl.Log.V(1).Info("All policy fields already set, no defaults applied",
			"defaultsName", defaults.Name)
	}
}

// mergeResourceConfig merges default resource config values into the policy.
func mergeResourceConfig(policy *rightsizev1alpha1.ResourceConfig, defaults *rightsizev1alpha1.ResourceConfig, prefix string) []string {
	if defaults == nil {
		return nil
	}
	var inherited []string
	if policy.Percentile == 0 && defaults.Percentile != 0 {
		policy.Percentile = defaults.Percentile
		inherited = append(inherited, prefix+".percentile")
	}
	if policy.SafetyMargin == "" && defaults.SafetyMargin != "" {
		policy.SafetyMargin = defaults.SafetyMargin
		inherited = append(inherited, prefix+".safetyMargin")
	}
	if policy.Bounds == nil && defaults.Bounds != nil {
		policy.Bounds = defaults.Bounds
		inherited = append(inherited, prefix+".bounds")
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
	if policy.StartupBoost == nil && defaults.StartupBoost != nil {
		policy.StartupBoost = defaults.StartupBoost
		inherited = append(inherited, prefix+".startupBoost")
	}
	return inherited
}

// isWithinResizeWindow returns true if the current time falls within the
// configured resize schedule. Returns true if no schedule is configured.
func isWithinResizeWindow(schedule *rightsizev1alpha1.ResizeSchedule, now time.Time) bool {
	if schedule == nil {
		return true
	}

	// Load timezone.
	tz := schedule.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Invalid timezone: fail open (allow resize) rather than silently
		// blocking resizes with an undetectable config error.
		return true
	}
	localNow := now.In(loc)

	// Check day-of-week constraint.
	if len(schedule.DaysOfWeek) > 0 {
		dayName := localNow.Weekday().String()
		dayAllowed := false
		for _, d := range schedule.DaysOfWeek {
			if strings.EqualFold(d, dayName) {
				dayAllowed = true
				break
			}
		}
		if !dayAllowed {
			return false
		}
	}

	// Check time windows. If no windows are specified, all times are allowed.
	if len(schedule.Windows) == 0 {
		return true
	}
	nowMinutes := localNow.Hour()*60 + localNow.Minute()
	for _, w := range schedule.Windows {
		start := parseHHMM(w.Start)
		end := parseHHMM(w.End)
		if start < 0 || end < 0 {
			continue
		}
		if start <= end {
			// Normal window: e.g. 02:00-06:00
			if nowMinutes >= start && nowMinutes < end {
				return true
			}
		} else {
			// Overnight window: e.g. 22:00-06:00
			if nowMinutes >= start || nowMinutes < end {
				return true
			}
		}
	}
	return false
}

// parseHHMM parses "HH:MM" into minutes since midnight. Returns -1 on error.
func parseHHMM(s string) int {
	if len(s) != 5 || s[2] != ':' {
		return -1
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
}
