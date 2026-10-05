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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/validation"
)

// AttuneDefaultsValidator validates AttuneDefaults resources.
type AttuneDefaultsValidator struct{}

// AttuneNamespaceDefaultsValidator validates AttuneNamespaceDefaults resources.
type AttuneNamespaceDefaultsValidator struct {
	// SecretAccess, when set, requires the admission user to get each referenced Secret.
	SecretAccess SecretAccessChecker
}

// ValidateCreate validates a new AttuneDefaults.
func (v *AttuneDefaultsValidator) ValidateCreate(_ context.Context, defaults *attunev1alpha1.AttuneDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("defaults_validate_create")
	defer timer.Observe()
	w, err := v.validate(defaults)
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated AttuneDefaults.
func (v *AttuneDefaultsValidator) ValidateUpdate(_ context.Context, old, defaults *attunev1alpha1.AttuneDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("defaults_validate_update")
	defer timer.Observe()
	var previous *attunev1alpha1.AttuneDefaultsSpec
	if old != nil {
		previous = &old.Spec
	}
	w, err := v.validatePrevious(defaults, previous)
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates an AttuneDefaults deletion (always succeeds).
func (v *AttuneDefaultsValidator) ValidateDelete(_ context.Context, _ *attunev1alpha1.AttuneDefaults) (admission.Warnings, error) {
	return nil, nil
}

func (v *AttuneDefaultsValidator) validate(defaults *attunev1alpha1.AttuneDefaults) (admission.Warnings, error) {
	return v.validatePrevious(defaults, nil)
}

func (v *AttuneDefaultsValidator) validatePrevious(defaults *attunev1alpha1.AttuneDefaults, previous *attunev1alpha1.AttuneDefaultsSpec) (admission.Warnings, error) {
	w, err := validateDefaultsSpecPrevious(defaults.Spec, previous)
	if err != nil {
		return w, err
	}
	if defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Prometheus != nil &&
		defaults.Spec.MetricsSource.Prometheus.BearerTokenSecret != nil {
		w = append(w, DeprecatedClusterBearerTokenSecretWarning)
	}
	if defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Datadog != nil &&
		defaults.Spec.MetricsSource.Datadog.APIKeySecretRef != nil &&
		defaults.Spec.MetricsSource.Datadog.APIKeySecretRef.Name != "" {
		w = append(w, DeprecatedClusterDatadogAPIKeyWarning)
	}
	if defaults.Spec.UpdateStrategy != nil &&
		defaults.Spec.UpdateStrategy.Export != nil &&
		defaults.Spec.UpdateStrategy.Export.PullRequest != nil &&
		defaults.Spec.UpdateStrategy.Export.PullRequest.TokenSecretRef != nil &&
		defaults.Spec.UpdateStrategy.Export.PullRequest.TokenSecretRef.Name != "" {
		w = append(w, DeprecatedClusterGitOpsTokenWarning)
	}
	return w, nil
}

// DeprecatedClusterBearerTokenSecretWarning is the admission warning for
// bearerTokenSecret on cluster AttuneDefaults. Lookup is unchanged (policy
// namespace) until a later release rejects the field on this kind.
const DeprecatedClusterBearerTokenSecretWarning = "metricsSource.prometheus.bearerTokenSecret on AttuneDefaults is deprecated; the Secret name is still read from each AttunePolicy namespace. Prefer operator ServiceAccount token or an operator-namespace Secret for cluster-wide Prometheus auth."

// DeprecatedClusterDatadogAPIKeyWarning is the admission warning for
// apiKeySecretRef on cluster AttuneDefaults.
const DeprecatedClusterDatadogAPIKeyWarning = "metricsSource.datadog.apiKeySecretRef on AttuneDefaults is deprecated; the Secret name is still copied onto each policy. Without --datadog-api-key-secret it is read in the policy namespace. With that flag, a cluster-chosen Datadog config reads the operator-namespace Secret instead. Policy and AttuneNamespaceDefaults refs stay in their namespace."

// DeprecatedClusterGitOpsTokenWarning is the admission warning for
// export.pullRequest.tokenSecretRef on cluster AttuneDefaults.
const DeprecatedClusterGitOpsTokenWarning = "updateStrategy.export.pullRequest.tokenSecretRef on AttuneDefaults is deprecated; the Secret name is still read from each AttunePolicy namespace. Put the GitOps token Secret on the policy or AttuneNamespaceDefaults."

// ValidateCreate validates a new AttuneNamespaceDefaults.
func (v *AttuneNamespaceDefaultsValidator) ValidateCreate(ctx context.Context, defaults *attunev1alpha1.AttuneNamespaceDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("namespace_defaults_validate_create")
	defer timer.Observe()
	w, err := validateDefaultsSpec(defaults.Spec)
	if err == nil {
		err = v.checkReferencedSecretAccess(ctx, defaults)
	}
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated AttuneNamespaceDefaults.
func (v *AttuneNamespaceDefaultsValidator) ValidateUpdate(ctx context.Context, old, defaults *attunev1alpha1.AttuneNamespaceDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("namespace_defaults_validate_update")
	defer timer.Observe()
	var previous *attunev1alpha1.AttuneDefaultsSpec
	if old != nil {
		previous = &old.Spec
	}
	w, err := validateDefaultsSpecPrevious(defaults.Spec, previous)
	if err == nil {
		err = v.checkReferencedSecretAccess(ctx, defaults)
	}
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates an AttuneNamespaceDefaults deletion (always succeeds).
func (v *AttuneNamespaceDefaultsValidator) ValidateDelete(_ context.Context, _ *attunev1alpha1.AttuneNamespaceDefaults) (admission.Warnings, error) {
	return nil, nil
}

func defaultsStrategyDuration(spec *attunev1alpha1.AttuneDefaultsSpec, safety bool) *metav1.Duration {
	if spec == nil || spec.UpdateStrategy == nil {
		return nil
	}
	if safety {
		return spec.UpdateStrategy.SafetyObservationPeriod
	}
	return spec.UpdateStrategy.Cooldown
}

func validateDefaultsSpec(spec attunev1alpha1.AttuneDefaultsSpec) (admission.Warnings, error) {
	return validateDefaultsSpecPrevious(spec, nil)
}

func validateDefaultsSpecPrevious(spec attunev1alpha1.AttuneDefaultsSpec, previous *attunev1alpha1.AttuneDefaultsSpec) (admission.Warnings, error) {
	if err := exclusiveMetricsProviderError(spec.MetricsSource); err != nil {
		return nil, err
	}
	if err := validateMetricsSourceProviderFields(spec.MetricsSource); err != nil {
		return nil, err
	}

	// Validate Prometheus settings if provided.
	if spec.MetricsSource != nil && spec.MetricsSource.Prometheus != nil {
		prometheus := spec.MetricsSource.Prometheus
		if prometheus.Address != "" {
			if err := ValidatePrometheusAddress(prometheus.Address); err != nil {
				return nil, fmt.Errorf("metricsSource.prometheus.address: %w", err)
			}
		}
		if err := validation.PrometheusQueryParameters(prometheus.QueryParameters); err != nil {
			return nil, fmt.Errorf("metricsSource.prometheus.queryParameters: %w", err)
		}
	}

	// Validate schedule fields if present.
	if spec.UpdateStrategy != nil && spec.UpdateStrategy.Schedule != nil {
		if err := validateSchedule(spec.UpdateStrategy.Schedule); err != nil {
			return nil, err
		}
	}

	// GitOps PR on defaults is copied onto policies by MergeDefaults.
	if spec.UpdateStrategy != nil {
		if err := validateGitOpsPullRequest(*spec.UpdateStrategy); err != nil {
			return nil, err
		}
	}

	// Validate queryStep bounds (10s to 1h).
	if spec.MetricsSource != nil && spec.MetricsSource.QueryStep != nil {
		qs := spec.MetricsSource.QueryStep.Duration
		if qs < 10*time.Second {
			return nil, fmt.Errorf("metricsSource.queryStep must be at least 10s, got %s", qs)
		}
		if qs > time.Hour {
			return nil, fmt.Errorf("metricsSource.queryStep must be at most 1h, got %s", qs)
		}
	}

	if spec.CostPricing != nil {
		if err := validatePositiveFloat("costPricing.cpuPerCoreHour", spec.CostPricing.CPUPerCoreHour); err != nil {
			return nil, err
		}
		if err := validatePositiveFloat("costPricing.memoryPerGiBHour", spec.CostPricing.MemoryPerGiBHour); err != nil {
			return nil, err
		}
	}

	// Validate CPU resource config fields.
	var warnings admission.Warnings
	var history *metav1.Duration
	if spec.MetricsSource != nil {
		history = spec.MetricsSource.HistoryWindow
	}
	if spec.CPU != nil {
		if err := validateResourceConfigFields("cpu", spec.CPU, history); err != nil {
			return warnings, err
		}
	}

	// Validate memory resource config fields.
	if spec.Memory != nil {
		if err := validateResourceConfigFields("memory", spec.Memory, history); err != nil {
			return warnings, err
		}
		if spec.Memory.StartupBoost != nil {
			warnings = append(warnings, "memory.startupBoost has no effect; startup boost only applies to CPU resources")
		}
	}

	// Validate cooldown minimum floor. An unchanged stored 0s is not a
	// wait; parseCooldown still maps it to the 1h built-in.
	if spec.UpdateStrategy != nil && spec.UpdateStrategy.Cooldown != nil {
		if err := validateDurationFloorAllowZero("updateStrategy.cooldown",
			spec.UpdateStrategy.Cooldown.Duration,
			sameStoredZero(defaultsStrategyDuration(previous, false), spec.UpdateStrategy.Cooldown)); err != nil {
			return warnings, err
		}
	}

	// Validate budget caps are non-negative.
	if spec.UpdateStrategy != nil {
		if q := spec.UpdateStrategy.MaxTotalCPUIncrease; q != nil && q.MilliValue() < 0 {
			return warnings, fmt.Errorf("updateStrategy.maxTotalCpuIncrease must be non-negative, got %s", q)
		}
		if q := spec.UpdateStrategy.MaxTotalMemoryIncrease; q != nil && q.Value() < 0 {
			return warnings, fmt.Errorf("updateStrategy.maxTotalMemoryIncrease must be non-negative, got %s", q)
		}
		if q := spec.UpdateStrategy.MaxCPUIncreasePerMinute; q != nil && q.MilliValue() < 0 {
			return warnings, fmt.Errorf("updateStrategy.maxCpuIncreasePerMinute must be non-negative, got %s", q)
		}
		if q := spec.UpdateStrategy.MaxMemoryIncreasePerMinute; q != nil && q.Value() < 0 {
			return warnings, fmt.Errorf("updateStrategy.maxMemoryIncreasePerMinute must be non-negative, got %s", q)
		}
		if spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
			warnings = append(warnings, "maxTotalCpuIncrease is deprecated; prefer maxCpuIncreasePerMinute so the cap does not depend on reconcileInterval")
		}
		if spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
			warnings = append(warnings, "maxTotalMemoryIncrease is deprecated; prefer maxMemoryIncreasePerMinute so the cap does not depend on reconcileInterval")
		}
	}

	// Validate safetyObservationPeriod has a minimum floor.
	if spec.UpdateStrategy != nil && spec.UpdateStrategy.SafetyObservationPeriod != nil {
		if err := validateDurationFloorAllowZero("updateStrategy.safetyObservationPeriod",
			spec.UpdateStrategy.SafetyObservationPeriod.Duration,
			sameStoredZero(defaultsStrategyDuration(previous, true), spec.UpdateStrategy.SafetyObservationPeriod)); err != nil {
			return warnings, err
		}
	}

	// Canary observationPeriod is a non-pointer, so omitted and 0s both mean
	// the built-in observation period.
	if spec.UpdateStrategy != nil && spec.UpdateStrategy.Canary != nil {
		if err := validatePositiveDurationFloor("updateStrategy.canary.observationPeriod",
			spec.UpdateStrategy.Canary.ObservationPeriod.Duration); err != nil {
			return warnings, err
		}
	}

	// Validate SLO guardrails. Index match is the same rule as AttunePolicy.
	if spec.UpdateStrategy != nil {
		var previousSLO []attunev1alpha1.SLOGuardrail
		if previous != nil && previous.UpdateStrategy != nil {
			previousSLO = previous.UpdateStrategy.SLOGuardrails
		}
		if err := validateSLOGuardrails(spec.UpdateStrategy.SLOGuardrails, previousSLO); err != nil {
			return warnings, err
		}
		if err := validateHPATargetBounds(spec.UpdateStrategy); err != nil {
			return warnings, err
		}
	}

	// Validate historyWindow bounds.
	if spec.MetricsSource != nil && spec.MetricsSource.HistoryWindow != nil {
		hw := spec.MetricsSource.HistoryWindow.Duration
		if hw < time.Hour {
			return warnings, fmt.Errorf("metricsSource.historyWindow must be at least 1h, got %s", hw)
		}
		if hw > 720*time.Hour {
			return warnings, fmt.Errorf("metricsSource.historyWindow must be at most 720h (30d), got %s", hw)
		}
	}

	// Validate rateWindow bounds (30s to historyWindow).
	if spec.MetricsSource != nil && spec.MetricsSource.RateWindow != nil {
		rw := spec.MetricsSource.RateWindow.Duration
		if rw < 30*time.Second {
			return warnings, fmt.Errorf("metricsSource.rateWindow must be at least 30s, got %s", rw)
		}
		maxWindow, _ := time.ParseDuration(attunev1alpha1.DefaultHistoryWindow)
		if spec.MetricsSource.HistoryWindow != nil {
			maxWindow = spec.MetricsSource.HistoryWindow.Duration
		}
		if rw > maxWindow {
			return warnings, fmt.Errorf("metricsSource.rateWindow (%s) must not exceed historyWindow (%s)", rw, maxWindow)
		}
	}

	return warnings, nil
}

// validateResourceConfigFields validates fields that are shared between
// policy and defaults ResourceConfig. The prefix (e.g. "cpu", "memory")
// is used in error messages.
func validateResourceConfigFields(prefix string, rc *attunev1alpha1.ResourceConfig, history *metav1.Duration) error {
	// Overhead
	if err := validateOverhead(prefix, rc.Overhead); err != nil {
		return err
	}

	// BurstSensitivity
	if err := validateBurstSensitivity(prefix, rc.BurstSensitivity); err != nil {
		return err
	}

	// MemoryFromCPURatio (only meaningful on memory, but validate on any ResourceConfig)
	if err := validateMemoryFromCPURatio(prefix+".memoryFromCpuRatio", rc.MemoryFromCPURatio); err != nil {
		return err
	}

	if err := validateLimitMultiplier(prefix, rc); err != nil {
		return err
	}
	if err := validateOOMBump(prefix, rc); err != nil {
		return err
	}
	if err := validateSurge(prefix, rc, history); err != nil {
		return err
	}

	// Percentile
	supportedPercentiles := map[int32]bool{50: true, 90: true, 95: true, 99: true}
	if p := rc.Percentile; p != 0 && !supportedPercentiles[p] {
		return fmt.Errorf("%s.percentile %d is not supported; must be one of: 50, 90, 95, 99", prefix, p)
	}

	// Bounds (minAllowed/maxAllowed)
	if rc.MinAllowed != nil && rc.MaxAllowed != nil {
		if rc.MinAllowed.Cmp(*rc.MaxAllowed) > 0 {
			return fmt.Errorf("%s.minAllowed (%s) must be <= %s.maxAllowed (%s)",
				prefix, rc.MinAllowed.String(), prefix, rc.MaxAllowed.String())
		}
	}
	if err := validateResourceMaxAllowedCap(prefix, rc); err != nil {
		return err
	}

	// StartupBoost (only valid for CPU, but validate format for both)
	if sb := rc.StartupBoost; sb != nil {
		m, err := strconv.ParseFloat(sb.Multiplier, 64)
		if err != nil {
			return fmt.Errorf("%s.startupBoost.multiplier %q is not a valid number: %w", prefix, sb.Multiplier, err)
		}
		if math.IsNaN(m) || math.IsInf(m, 0) {
			return fmt.Errorf("%s.startupBoost.multiplier must be a finite number, got %s", prefix, sb.Multiplier)
		}
		if m <= 1 {
			return fmt.Errorf("%s.startupBoost.multiplier must be > 1.0, got %s", prefix, sb.Multiplier)
		}
		if m > 10 {
			return fmt.Errorf("%s.startupBoost.multiplier must be <= 10.0, got %s", prefix, sb.Multiplier)
		}
		if sb.Duration.Duration < 10*time.Second {
			return fmt.Errorf("%s.startupBoost.duration must be at least 10s, got %s", prefix, sb.Duration.Duration)
		}
		if sb.Duration.Duration > 1*time.Hour {
			return fmt.Errorf("%s.startupBoost.duration must be at most 1h, got %s", prefix, sb.Duration.Duration)
		}
	}

	return nil
}

// resourceCapKind classifies a ResourceConfig path. Policy fields are
// "cpu" and "memory". Container entries use "containerPolicies[i].cpu".
func resourceCapKind(prefix string) string {
	switch {
	case prefix == "cpu" || strings.HasSuffix(prefix, ".cpu"):
		return "cpu"
	case prefix == "memory" || strings.HasSuffix(prefix, ".memory"):
		return "memory"
	default:
		return ""
	}
}

// validateResourceMaxAllowedCap enforces the same absolute ceilings used
// by AttunePolicy admission (256 cores CPU, 16Ti memory) so AttuneDefaults
// cannot merge uncapped maxAllowed onto policies after admission.
func validateResourceMaxAllowedCap(prefix string, rc *attunev1alpha1.ResourceConfig) error {
	if rc == nil || rc.MaxAllowed == nil {
		return nil
	}
	var capStr, human string
	switch resourceCapKind(prefix) {
	case "cpu":
		capStr, human = "256", "256 cores"
	case "memory":
		capStr, human = "16Ti", "16Ti"
	default:
		return nil
	}
	cap, err := resource.ParseQuantity(capStr)
	if err != nil {
		return fmt.Errorf("%s.maxAllowed cap %q: %w", prefix, capStr, err)
	}
	if rc.MaxAllowed.Cmp(cap) > 0 {
		return fmt.Errorf("%s.maxAllowed (%s) exceeds the maximum allowed value of %s",
			prefix, rc.MaxAllowed.String(), human)
	}
	return nil
}

func validatePositiveFloat(field, value string) error {
	if value == "" {
		return nil
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid number: %w", field, value, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number, got %s", field, value)
	}
	if v <= 0 {
		return fmt.Errorf("%s must be positive, got %s", field, value)
	}
	return nil
}
