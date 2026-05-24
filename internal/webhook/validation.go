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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/validation"
)

// RightSizePolicyValidator implements the typed Validator interface for RightSizePolicy.
type RightSizePolicyValidator struct{}

// ValidateCreate validates a new RightSizePolicy.
func (v *RightSizePolicyValidator) ValidateCreate(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("validate_create")
	defer timer.Observe()
	w, err := v.validate(policy)
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated RightSizePolicy.
func (v *RightSizePolicyValidator) ValidateUpdate(ctx context.Context, oldPolicy, policy *rightsizev1alpha1.RightSizePolicy) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("validate_update")
	defer timer.Observe()
	w, err := v.validate(policy)
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates a RightSizePolicy deletion (always succeeds).
func (v *RightSizePolicyValidator) ValidateDelete(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (admission.Warnings, error) {
	return nil, nil
}

func (v *RightSizePolicyValidator) validate(policy *rightsizev1alpha1.RightSizePolicy) (admission.Warnings, error) {
	var warnings admission.Warnings

	// CPU bounds: minAllowed must be <= maxAllowed, and maxAllowed capped at 256 cores.
	if policy.Spec.CPU.MinAllowed != nil && policy.Spec.CPU.MaxAllowed != nil {
		if policy.Spec.CPU.MinAllowed.Cmp(*policy.Spec.CPU.MaxAllowed) > 0 {
			return warnings, fmt.Errorf("cpu.minAllowed (%s) must be <= cpu.maxAllowed (%s)",
				policy.Spec.CPU.MinAllowed.String(), policy.Spec.CPU.MaxAllowed.String())
		}
	}
	if policy.Spec.CPU.MaxAllowed != nil {
		maxCPU := resource.MustParse("256")
		if policy.Spec.CPU.MaxAllowed.Cmp(maxCPU) > 0 {
			return warnings, fmt.Errorf("cpu.maxAllowed (%s) exceeds the maximum allowed value of 256 cores",
				policy.Spec.CPU.MaxAllowed.String())
		}
	}

	// Memory bounds: minAllowed must be <= maxAllowed, and maxAllowed capped at 16Ti.
	if policy.Spec.Memory.MinAllowed != nil && policy.Spec.Memory.MaxAllowed != nil {
		if policy.Spec.Memory.MinAllowed.Cmp(*policy.Spec.Memory.MaxAllowed) > 0 {
			return warnings, fmt.Errorf("memory.minAllowed (%s) must be <= memory.maxAllowed (%s)",
				policy.Spec.Memory.MinAllowed.String(), policy.Spec.Memory.MaxAllowed.String())
		}
	}
	if policy.Spec.Memory.MaxAllowed != nil {
		maxMemory := resource.MustParse("16Ti")
		if policy.Spec.Memory.MaxAllowed.Cmp(maxMemory) > 0 {
			return warnings, fmt.Errorf("memory.maxAllowed (%s) exceeds the maximum allowed value of 16Ti",
				policy.Spec.Memory.MaxAllowed.String())
		}
	}

	// Canary config required when mode is Canary
	if policy.Spec.UpdateStrategy.Type == rightsizev1alpha1.UpdateTypeCanary && policy.Spec.UpdateStrategy.Canary == nil {
		return warnings, fmt.Errorf("updateStrategy.canary is required when mode is Canary")
	}

	// Validate canary observation period has a minimum floor.
	if policy.Spec.UpdateStrategy.Canary != nil {
		op := policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
		if op < 0 {
			return warnings, fmt.Errorf("updateStrategy.canary.observationPeriod must be non-negative, got %s", op)
		}
		if op > 0 && op < time.Minute {
			return warnings, fmt.Errorf("updateStrategy.canary.observationPeriod must be at least 1m, got %s", op)
		}
		if op == 0 {
			warnings = append(warnings, "updateStrategy.canary.observationPeriod is 0; the default observation period will be used")
		}
	}

	// Validate safetyObservationPeriod has a minimum floor.
	if policy.Spec.UpdateStrategy.SafetyObservationPeriod != nil {
		sop := policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration
		if sop < 0 {
			return warnings, fmt.Errorf("updateStrategy.safetyObservationPeriod must be non-negative, got %s", sop)
		}
		if sop > 0 && sop < time.Minute {
			return warnings, fmt.Errorf("updateStrategy.safetyObservationPeriod must be at least 1m, got %s", sop)
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
	if !rightsizev1alpha1.IsSupportedTargetKind(policy.Spec.TargetRef.Kind) {
		return warnings, fmt.Errorf(
			"targetRef.kind %q is not supported; must be one of: %s",
			policy.Spec.TargetRef.Kind, rightsizev1alpha1.SupportedTargetKindsCSV)
	}

	// Validate overhead is a valid non-negative percentage.
	if err := validateOverhead("cpu", policy.Spec.CPU.Overhead); err != nil {
		return warnings, err
	}
	if err := validateOverhead("memory", policy.Spec.Memory.Overhead); err != nil {
		return warnings, err
	}

	// Validate burstSensitivity is a valid non-negative float, max 1.0.
	if err := validateBurstSensitivity("cpu", policy.Spec.CPU.BurstSensitivity); err != nil {
		return warnings, err
	}
	if err := validateBurstSensitivity("memory", policy.Spec.Memory.BurstSensitivity); err != nil {
		return warnings, err
	}

	// Validate CPU startup boost if configured.
	if sb := policy.Spec.CPU.StartupBoost; sb != nil {
		m, err := strconv.ParseFloat(sb.Multiplier, 64)
		if err != nil {
			return warnings, fmt.Errorf("cpu.startupBoost.multiplier %q is not a valid number: %w", sb.Multiplier, err)
		}
		if math.IsNaN(m) || math.IsInf(m, 0) {
			return warnings, fmt.Errorf("cpu.startupBoost.multiplier must be a finite number, got %s", sb.Multiplier)
		}
		if m <= 1 {
			return warnings, fmt.Errorf("cpu.startupBoost.multiplier must be > 1.0, got %s", sb.Multiplier)
		}
		if m > 10 {
			return warnings, fmt.Errorf("cpu.startupBoost.multiplier must be <= 10.0, got %s", sb.Multiplier)
		}
		if sb.Duration.Duration < 10*time.Second {
			return warnings, fmt.Errorf("cpu.startupBoost.duration must be at least 10s, got %s", sb.Duration.Duration)
		}
		if sb.Duration.Duration > 1*time.Hour {
			return warnings, fmt.Errorf("cpu.startupBoost.duration must be at most 1h, got %s", sb.Duration.Duration)
		}
	}

	// Warn if memory startup boost is set (only CPU boost is implemented).
	if policy.Spec.Memory.StartupBoost != nil {
		warnings = append(warnings, "memory.startupBoost has no effect; startup boost only applies to CPU resources")
	}

	// Validate cooldown has a minimum floor to prevent resource exhaustion via tight reconciliation loops.
	if policy.Spec.UpdateStrategy.Cooldown != nil {
		cd := policy.Spec.UpdateStrategy.Cooldown.Duration
		if cd < 0 {
			return warnings, fmt.Errorf("updateStrategy.cooldown must be non-negative, got %s", cd)
		}
		if cd > 0 && cd < time.Minute {
			return warnings, fmt.Errorf("updateStrategy.cooldown must be at least 1m to prevent excessive reconciliation, got %s", cd)
		}
	}

	// Validate budget caps are non-negative.
	if q := policy.Spec.UpdateStrategy.MaxTotalCPUIncrease; q != nil && q.MilliValue() < 0 {
		return warnings, fmt.Errorf("updateStrategy.maxTotalCpuIncrease must be non-negative, got %s", q)
	}
	if q := policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease; q != nil && q.Value() < 0 {
		return warnings, fmt.Errorf("updateStrategy.maxTotalMemoryIncrease must be non-negative, got %s", q)
	}

	// Validate percentile values are in the supported set.
	supportedPercentiles := map[int32]bool{50: true, 90: true, 95: true, 99: true}
	if p := policy.Spec.CPU.Percentile; p != 0 && !supportedPercentiles[p] {
		return warnings, fmt.Errorf("cpu.percentile %d is not supported; must be one of: 50, 90, 95, 99", p)
	}
	if p := policy.Spec.Memory.Percentile; p != 0 && !supportedPercentiles[p] {
		return warnings, fmt.Errorf("memory.percentile %d is not supported; must be one of: 50, 90, 95, 99", p)
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
		maxWindow := 168 * time.Hour // default history window
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
	if err := validateSLOGuardrails(policy.Spec.UpdateStrategy.SLOGuardrails); err != nil {
		return warnings, err
	}

	// Validate schedule fields.
	if schedule := policy.Spec.UpdateStrategy.Schedule; schedule != nil {
		if err := validateSchedule(schedule); err != nil {
			return warnings, err
		}
	}

	// Warn if memory decrease is enabled
	if policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease {
		warnings = append(warnings, "memory.allowDecrease is enabled; this carries OOMKill risk")
	}

	return warnings, nil
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

// validWeekdays is the set of accepted day-of-week names (case-insensitive).
var validWeekdays = map[string]bool{
	"monday": true, "tuesday": true, "wednesday": true,
	"thursday": true, "friday": true, "saturday": true, "sunday": true,
}

func validateSchedule(schedule *rightsizev1alpha1.ResizeSchedule) error {
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
func validateSLOGuardrails(guardrails []rightsizev1alpha1.SLOGuardrail) error {
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
		if _, err := strconv.ParseFloat(g.Threshold, 64); err != nil {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].threshold %q is not a valid number: %w", i, g.Threshold, err)
		}

		if g.Comparison != "" && g.Comparison != "above" && g.Comparison != "below" {
			return fmt.Errorf("updateStrategy.sloGuardrails[%d].comparison must be \"above\" or \"below\", got %q", i, g.Comparison)
		}

		if g.EvaluationWindow != nil {
			ew := g.EvaluationWindow.Duration
			if ew < 0 {
				return fmt.Errorf("updateStrategy.sloGuardrails[%d].evaluationWindow must be non-negative, got %s", i, ew)
			}
			if ew > 0 && ew < time.Minute {
				return fmt.Errorf("updateStrategy.sloGuardrails[%d].evaluationWindow must be at least 1m, got %s", i, ew)
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
