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
	"math"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/validation"
)

// AttunePolicyValidator implements the typed Validator interface for AttunePolicy.
type AttunePolicyValidator struct{}

// ValidateCreate validates a new AttunePolicy.
func (v *AttunePolicyValidator) ValidateCreate(ctx context.Context, policy *attunev1alpha1.AttunePolicy) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("validate_create")
	defer timer.Observe()
	w, err := v.validate(policy)
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated AttunePolicy.
func (v *AttunePolicyValidator) ValidateUpdate(ctx context.Context, oldPolicy, policy *attunev1alpha1.AttunePolicy) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("validate_update")
	defer timer.Observe()
	w, err := v.validate(policy)
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates an AttunePolicy deletion (always succeeds).
func (v *AttunePolicyValidator) ValidateDelete(ctx context.Context, policy *attunev1alpha1.AttunePolicy) (admission.Warnings, error) {
	return nil, nil
}

func (v *AttunePolicyValidator) validate(policy *attunev1alpha1.AttunePolicy) (admission.Warnings, error) {
	var warnings admission.Warnings

	// Use a local pointer to avoid mutating the input object.
	us := policy.Spec.UpdateStrategy
	if us == nil {
		us = &attunev1alpha1.UpdateStrategy{}
	}

	// Validate shared ResourceConfig fields (overhead, burstSensitivity,
	// memoryFromCpuRatio, percentile, bounds, startupBoost) for CPU and memory.
	// These checks are shared with AttuneDefaults validation.
	if err := validateResourceConfigFields("cpu", &policy.Spec.CPU); err != nil {
		return warnings, err
	}
	if err := validateResourceConfigFields("memory", &policy.Spec.Memory); err != nil {
		return warnings, err
	}

	// Policy-specific caps beyond what ResourceConfig validates:
	// CPU maxAllowed capped at 256 cores, memory maxAllowed capped at 16Ti.
	if policy.Spec.CPU.MaxAllowed != nil {
		maxCPU := resource.MustParse("256")
		if policy.Spec.CPU.MaxAllowed.Cmp(maxCPU) > 0 {
			return warnings, fmt.Errorf("cpu.maxAllowed (%s) exceeds the maximum allowed value of 256 cores",
				policy.Spec.CPU.MaxAllowed.String())
		}
	}
	if policy.Spec.Memory.MaxAllowed != nil {
		maxMemory := resource.MustParse("16Ti")
		if policy.Spec.Memory.MaxAllowed.Cmp(maxMemory) > 0 {
			return warnings, fmt.Errorf("memory.maxAllowed (%s) exceeds the maximum allowed value of 16Ti",
				policy.Spec.Memory.MaxAllowed.String())
		}
	}

	// Warn if memory startup boost is set (only CPU boost is implemented).
	if policy.Spec.Memory.StartupBoost != nil {
		warnings = append(warnings, "memory.startupBoost has no effect; startup boost only applies to CPU resources")
	}

	// Canary config required when type is Canary
	if us.Type == attunev1alpha1.UpdateTypeCanary && us.Canary == nil {
		return warnings, fmt.Errorf("updateStrategy.canary is required when updateStrategy.type is Canary")
	}

	// Validate canary observation period has a minimum floor.
	if us.Canary != nil {
		if err := validateDurationFloor("updateStrategy.canary.observationPeriod",
			us.Canary.ObservationPeriod.Duration, time.Minute); err != nil {
			return warnings, err
		}
		if us.Canary.ObservationPeriod.Duration == 0 {
			warnings = append(warnings, "updateStrategy.canary.observationPeriod is 0; the default observation period will be used")
		}
	}

	// Validate safetyObservationPeriod has a minimum floor.
	if us.SafetyObservationPeriod != nil {
		if err := validateDurationFloor("updateStrategy.safetyObservationPeriod",
			us.SafetyObservationPeriod.Duration, time.Minute); err != nil {
			return warnings, err
		}
	}

	// targetRef must have name or selector, but not both.
	hasName := policy.Spec.TargetRef.Name != nil && *policy.Spec.TargetRef.Name != ""
	hasSelector := policy.Spec.TargetRef.Selector != nil
	if hasSelector {
		sel := policy.Spec.TargetRef.Selector
		selectorEmpty := len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0
		if selectorEmpty {
			return warnings, fmt.Errorf("targetRef.selector must include at least one matchLabels or matchExpressions entry")
		}
	}
	if !hasName && !hasSelector {
		return warnings, fmt.Errorf("targetRef must specify either name or selector")
	}
	if hasName && hasSelector {
		return warnings, fmt.Errorf("targetRef must specify name or selector, not both")
	}

	// Validate targetRef.kind is a supported workload type.
	if !attunev1alpha1.IsSupportedTargetKind(policy.Spec.TargetRef.Kind) {
		return warnings, fmt.Errorf(
			"targetRef.kind %q is not supported; must be one of: %s",
			policy.Spec.TargetRef.Kind, attunev1alpha1.SupportedTargetKindsCSV)
	}

	// Validate cooldown has a minimum floor to prevent resource exhaustion via tight reconciliation loops.
	if us.Cooldown != nil {
		if err := validateDurationFloor("updateStrategy.cooldown",
			us.Cooldown.Duration, time.Minute); err != nil {
			return warnings, err
		}
	}

	// Validate budget caps are non-negative.
	if q := us.MaxTotalCPUIncrease; q != nil && q.MilliValue() < 0 {
		return warnings, fmt.Errorf("updateStrategy.maxTotalCpuIncrease must be non-negative, got %s", q)
	}
	if q := us.MaxTotalMemoryIncrease; q != nil && q.Value() < 0 {
		return warnings, fmt.Errorf("updateStrategy.maxTotalMemoryIncrease must be non-negative, got %s", q)
	}

	// Validate historyWindow is within reasonable bounds (1h to 720h/30d).
	if policy.Spec.MetricsSource.HistoryWindow != nil {
		hw := policy.Spec.MetricsSource.HistoryWindow.Duration
		if hw < time.Hour {
			return warnings, fmt.Errorf("metricsSource.historyWindow must be at least 1h, got %s", hw)
		}
		if hw > 720*time.Hour {
			return warnings, fmt.Errorf("metricsSource.historyWindow must be at most 720h (30d), got %s", hw)
		}
	}

	// Validate queryStep bounds (10s to 1h).
	if policy.Spec.MetricsSource.QueryStep != nil {
		qs := policy.Spec.MetricsSource.QueryStep.Duration
		if qs < 10*time.Second {
			return warnings, fmt.Errorf("metricsSource.queryStep must be at least 10s, got %s", qs)
		}
		if qs > time.Hour {
			return warnings, fmt.Errorf("metricsSource.queryStep must be at most 1h, got %s", qs)
		}
	}

	// Validate rateWindow bounds (30s to historyWindow).
	if policy.Spec.MetricsSource.RateWindow != nil {
		rw := policy.Spec.MetricsSource.RateWindow.Duration
		if rw < 30*time.Second {
			return warnings, fmt.Errorf("metricsSource.rateWindow must be at least 30s, got %s", rw)
		}
		maxWindow, _ := time.ParseDuration(attunev1alpha1.DefaultHistoryWindow)
		if policy.Spec.MetricsSource.HistoryWindow != nil {
			maxWindow = policy.Spec.MetricsSource.HistoryWindow.Duration
		}
		if rw > maxWindow {
			return warnings, fmt.Errorf("metricsSource.rateWindow (%s) must not exceed historyWindow (%s)", rw, maxWindow)
		}
	}

	// Validate at most one metrics source is configured.
	sourceCount := 0
	if policy.Spec.MetricsSource.Prometheus != nil {
		sourceCount++
	}
	if policy.Spec.MetricsSource.Datadog != nil {
		sourceCount++
	}
	if policy.Spec.MetricsSource.CloudWatch != nil {
		sourceCount++
	}
	if policy.Spec.MetricsSource.VPA != nil {
		sourceCount++
	}
	if sourceCount > 1 {
		return warnings, fmt.Errorf("metricsSource: at most one of prometheus, datadog, cloudwatch, or vpa may be set")
	}

	// Validate VPA settings if specified.
	if vpa := policy.Spec.MetricsSource.VPA; vpa != nil {
		if vpa.Name == "" {
			return warnings, fmt.Errorf("metricsSource.vpa.name is required")
		}
	}

	// Validate Prometheus settings if specified.
	if prometheus := policy.Spec.MetricsSource.Prometheus; prometheus != nil {
		if prometheus.Address != "" {
			if err := ValidatePrometheusAddress(prometheus.Address); err != nil {
				return warnings, fmt.Errorf("metricsSource.prometheus.address: %w", err)
			}
		}
		if err := validation.PrometheusQueryParameters(prometheus.QueryParameters); err != nil {
			return warnings, fmt.Errorf("metricsSource.prometheus.queryParameters: %w", err)
		}
		// Reject bearer token secret names containing "/" to prevent
		// cross-namespace secret references. The secret is always read
		// from the policy's own namespace.
		if prometheus.BearerTokenSecret != nil {
			if strings.Contains(prometheus.BearerTokenSecret.Name, "/") {
				return warnings, fmt.Errorf("metricsSource.prometheus.bearerTokenSecret.name must not contain '/'; secrets are read from the policy's namespace")
			}
		}
	}

	// Validate Datadog settings if specified.
	if dd := policy.Spec.MetricsSource.Datadog; dd != nil {
		validSites := map[string]bool{
			"datadoghq.com": true, "datadoghq.eu": true,
			"us3.datadoghq.com": true, "us5.datadoghq.com": true,
			"ap1.datadoghq.com": true, "ddog-gov.com": true,
		}
		if dd.Site != "" && !validSites[dd.Site] {
			return warnings, fmt.Errorf("metricsSource.datadog.site %q is not a recognized Datadog site", dd.Site)
		}
		if dd.APIKeySecretRef.Name == "" {
			return warnings, fmt.Errorf("metricsSource.datadog.apiKeySecretRef.name is required")
		}
		if dd.APIKeySecretRef.Key == "" {
			return warnings, fmt.Errorf("metricsSource.datadog.apiKeySecretRef.key is required")
		}
		if strings.Contains(dd.APIKeySecretRef.Name, "/") {
			return warnings, fmt.Errorf("metricsSource.datadog.apiKeySecretRef.name must not contain '/'; secrets are read from the policy's namespace")
		}
	}

	// Validate CloudWatch settings if specified.
	if cw := policy.Spec.MetricsSource.CloudWatch; cw != nil {
		if cw.Region == "" {
			return warnings, fmt.Errorf("metricsSource.cloudwatch.region is required")
		}
		if cw.ClusterName == "" {
			return warnings, fmt.Errorf("metricsSource.cloudwatch.clusterName is required")
		}
	}

	// Validate SLO guardrails.
	if err := validateSLOGuardrails(us.SLOGuardrails); err != nil {
		return warnings, err
	}

	// Validate schedule fields.
	if schedule := us.Schedule; schedule != nil {
		if err := validateSchedule(schedule); err != nil {
			return warnings, err
		}
	}

	// Warn if memory decrease is enabled
	if policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease {
		warnings = append(warnings, "memory.allowDecrease is enabled; this carries OOMKill risk")
	}

	// Warn if paused with an active mode.
	if policy.Spec.Paused != nil && *policy.Spec.Paused {
		mode := us.Type
		if mode == attunev1alpha1.UpdateTypeAuto || mode == attunev1alpha1.UpdateTypeOneShot || mode == attunev1alpha1.UpdateTypeCanary {
			warnings = append(warnings, fmt.Sprintf(
				"spec.paused is true but type is %s; no metrics collection or resizes will occur while paused", mode))
		}
	}

	// Warn about settings that have no effect in non-resizing modes.
	warnings = append(warnings, warnIneffectiveSettings(policy)...)

	return warnings, nil
}

// warnIneffectiveSettings detects configuration combinations that are
// technically valid but have no effect, helping users catch typos and
// misunderstandings before they wonder why nothing is happening.
func warnIneffectiveSettings(policy *attunev1alpha1.AttunePolicy) admission.Warnings {
	var w admission.Warnings
	if policy.Spec.UpdateStrategy == nil {
		return w
	}
	mode := policy.Spec.UpdateStrategy.Type
	isNonResizing := mode == attunev1alpha1.UpdateTypeObserve ||
		mode == attunev1alpha1.UpdateTypeRecommend ||
		mode == ""

	// Settings that only matter when resizes happen.
	if isNonResizing {
		if policy.Spec.UpdateStrategy.InitialSizing != nil && *policy.Spec.UpdateStrategy.InitialSizing {
			w = append(w, fmt.Sprintf("initialSizing has no effect in %s mode; it requires Auto, OneShot, or Canary", mode))
		}
		if policy.Spec.UpdateStrategy.AutoRevert != nil && *policy.Spec.UpdateStrategy.AutoRevert {
			w = append(w, fmt.Sprintf("autoRevert has no effect in %s mode; no resizes occur to revert", mode))
		}
		if len(policy.Spec.UpdateStrategy.SLOGuardrails) > 0 {
			w = append(w, fmt.Sprintf("sloGuardrails have no effect in %s mode; no resizes occur to guard", mode))
		}
		if policy.Spec.UpdateStrategy.Schedule != nil {
			w = append(w, fmt.Sprintf("schedule has no effect in %s mode; no resizes occur to schedule", mode))
		}
		if policy.Spec.UpdateStrategy.ResizeMethod != "" {
			w = append(w, fmt.Sprintf("resizeMethod has no effect in %s mode; no resizes occur", mode))
		}
		if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
			w = append(w, fmt.Sprintf("maxTotalCpuIncrease has no effect in %s mode; no resizes occur", mode))
		}
		if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
			w = append(w, fmt.Sprintf("maxTotalMemoryIncrease has no effect in %s mode; no resizes occur", mode))
		}
	}

	// Export in Observe mode produces nothing (no recommendations surfaced).
	if mode == attunev1alpha1.UpdateTypeObserve {
		if policy.Spec.UpdateStrategy.Export != nil && policy.Spec.UpdateStrategy.Export.ConfigMap {
			w = append(w, "export.configMap has no effect in Observe mode; recommendations are not surfaced")
		}
	}

	// Canary config outside Canary mode.
	if policy.Spec.UpdateStrategy.Canary != nil && mode != attunev1alpha1.UpdateTypeCanary {
		w = append(w, fmt.Sprintf("canary configuration has no effect in %s mode", mode))
	}

	// SLO guardrails with autoRevert disabled.
	if len(policy.Spec.UpdateStrategy.SLOGuardrails) > 0 && !isNonResizing {
		if policy.Spec.UpdateStrategy.AutoRevert != nil && !*policy.Spec.UpdateStrategy.AutoRevert {
			w = append(w, "sloGuardrails are configured but autoRevert is false; SLO breaches will be detected but not acted on")
		}
	}

	// SLO guardrails with VPA source (no Prometheus query interface).
	if len(policy.Spec.UpdateStrategy.SLOGuardrails) > 0 && policy.Spec.MetricsSource.VPA != nil {
		w = append(w, "sloGuardrails require a Prometheus-compatible metrics source; VPA source does not support PromQL queries")
	}

	// maxConcurrentResizes > 1 in OneShot mode.
	if mode == attunev1alpha1.UpdateTypeOneShot && policy.Spec.UpdateStrategy.MaxConcurrentResizes > 1 {
		w = append(w, "maxConcurrentResizes > 1 has no effect in OneShot mode; only one pod is resized per cycle")
	}

	// memoryFromCpuRatio makes memory percentile/overhead redundant.
	if policy.Spec.Memory.MemoryFromCPURatio != nil && *policy.Spec.Memory.MemoryFromCPURatio != "" {
		if policy.Spec.Memory.Percentile != 0 {
			w = append(w, "memory.percentile has no effect when memoryFromCpuRatio is set; memory is derived from CPU")
		}
		if policy.Spec.Memory.Overhead != "" {
			w = append(w, "memory.overhead has no effect when memoryFromCpuRatio is set; memory is derived from CPU")
		}
	}

	return w
}

func validateOverhead(resource, overhead string) error {
	if overhead == "" {
		return nil
	}
	v, err := strconv.ParseFloat(overhead, 64)
	if err != nil {
		return fmt.Errorf("%s.overhead %q is not a valid number: %w", resource, overhead, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s.overhead must be a finite number, got %s", resource, overhead)
	}
	if v < 0 {
		return fmt.Errorf("%s.overhead must be non-negative, got %s", resource, overhead)
	}
	// Upper bound prevents excessive resource allocation that could exhaust nodes.
	// 900% overhead = 10x multiplier, matching the old overhead max of 10.0.
	if v > 900 {
		return fmt.Errorf("%s.overhead must be <= 900, got %s", resource, overhead)
	}
	return nil
}

func validateMemoryFromCPURatio(ratio *string) error {
	if ratio == nil || *ratio == "" {
		return nil
	}
	v, err := strconv.ParseFloat(*ratio, 64)
	if err != nil {
		return fmt.Errorf("memory.memoryFromCpuRatio %q is not a valid number: %w", *ratio, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("memory.memoryFromCpuRatio must be a finite number, got %s", *ratio)
	}
	if v <= 0 {
		return fmt.Errorf("memory.memoryFromCpuRatio must be positive, got %s", *ratio)
	}
	if v > 1000 { //nolint:mnd // 1000 matches the controller ceiling for GiB-per-core ratios
		return fmt.Errorf("memory.memoryFromCpuRatio must be <= 1000, got %s", *ratio)
	}
	return nil
}

func validateBurstSensitivity(resource string, value *string) error {
	if value == nil {
		return nil
	}
	v, err := strconv.ParseFloat(*value, 64)
	if err != nil {
		return fmt.Errorf("%s.burstSensitivity %q is not a valid number: %w", resource, *value, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s.burstSensitivity must be a finite number, got %s", resource, *value)
	}
	if v < 0 {
		return fmt.Errorf("%s.burstSensitivity must be non-negative, got %s", resource, *value)
	}
	if v > 1.0 {
		return fmt.Errorf("%s.burstSensitivity must be <= 1.0, got %s", resource, *value)
	}
	return nil
}

// validateDurationFloor checks that a duration is non-negative and, if positive,
// at least the specified minimum floor. This pattern is used for cooldown,
// observation periods, and evaluation windows throughout validation.
func validateDurationFloor(field string, d time.Duration, minFloor time.Duration) error { //nolint:unparam // minFloor kept as parameter for readability and future use
	if d < 0 {
		return fmt.Errorf("%s must be non-negative, got %s", field, d)
	}
	if d > 0 && d < minFloor {
		return fmt.Errorf("%s must be at least %s, got %s", field, minFloor, d)
	}
	return nil
}

// validWeekdays is the set of accepted day-of-week names (case-insensitive).
var validWeekdays = map[string]bool{
	"monday": true, "tuesday": true, "wednesday": true,
	"thursday": true, "friday": true, "saturday": true, "sunday": true,
}

func validateSchedule(schedule *attunev1alpha1.ResizeSchedule) error {
	// Validate timezone.
	if tz := schedule.Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("updateStrategy.schedule.timezone %q is not a valid IANA timezone: %w", tz, err)
		}
	}

	// Validate days of week.
	for _, day := range schedule.DaysOfWeek {
		if !validWeekdays[strings.ToLower(day)] {
			return fmt.Errorf("updateStrategy.schedule.daysOfWeek contains invalid day %q; valid values: Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday", day)
		}
	}

	// Validate time windows (defense-in-depth; CRD pattern also validates).
	for i, w := range schedule.Windows {
		if err := validateHHMM(fmt.Sprintf("schedule.windows[%d].start", i), w.Start); err != nil {
			return err
		}
		if err := validateHHMM(fmt.Sprintf("schedule.windows[%d].end", i), w.End); err != nil {
			return err
		}
	}

	return nil
}

func validateHHMM(field, value string) error {
	if len(value) != 5 || value[2] != ':' {
		return fmt.Errorf("updateStrategy.%s %q must be in HH:MM format", field, value)
	}
	h, err1 := strconv.Atoi(value[:2])
	m, err2 := strconv.Atoi(value[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("updateStrategy.%s %q is not a valid time (00:00-23:59)", field, value)
	}
	return nil
}

// validateSLOGuardrails validates all SLO guardrail entries.
func validateSLOGuardrails(guardrails []attunev1alpha1.SLOGuardrail) error {
	names := make(map[string]bool, len(guardrails))
	for i, g := range guardrails {
		if g.Name == "" {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].name is required", i)
		}
		if names[g.Name] {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].name %q is duplicated", i, g.Name)
		}
		names[g.Name] = true

		if g.Query == "" {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].query is required", i)
		}

		if g.Threshold == "" {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].threshold is required", i)
		}
		tv, err := strconv.ParseFloat(g.Threshold, 64)
		if err != nil {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].threshold %q is not a valid number: %w", i, g.Threshold, err)
		}
		if math.IsNaN(tv) || math.IsInf(tv, 0) {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].threshold must be a finite number, got %s", i, g.Threshold)
		}

		if g.Comparison != "" && g.Comparison != "above" && g.Comparison != "below" {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].comparison must be \"above\" or \"below\", got %q", i, g.Comparison)
		}

		if g.EvaluationWindow != nil {
			if err := validateDurationFloor(
				fmt.Sprintf("updateStrategy.sloGuardrails[%d].evaluationWindow", i),
				g.EvaluationWindow.Duration, time.Minute); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidatePrometheusAddress delegates to the shared validation package.
// Kept as a wrapper for backward compatibility with webhook callers.
func ValidatePrometheusAddress(address string) error {
	return validation.PrometheusAddress(address)
}
