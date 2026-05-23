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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/validation"
)

// RightSizeDefaultsValidator validates RightSizeDefaults resources.
type RightSizeDefaultsValidator struct{}

// RightSizeNamespaceDefaultsValidator validates RightSizeNamespaceDefaults resources.
type RightSizeNamespaceDefaultsValidator struct{}

// ValidateCreate validates a new RightSizeDefaults.
func (v *RightSizeDefaultsValidator) ValidateCreate(_ context.Context, defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("defaults_validate_create")
	defer timer.Observe()
	w, err := v.validate(defaults)
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated RightSizeDefaults.
func (v *RightSizeDefaultsValidator) ValidateUpdate(_ context.Context, _, defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("defaults_validate_update")
	defer timer.Observe()
	w, err := v.validate(defaults)
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates a RightSizeDefaults deletion (always succeeds).
func (v *RightSizeDefaultsValidator) ValidateDelete(_ context.Context, _ *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	return nil, nil
}

func (v *RightSizeDefaultsValidator) validate(defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	return validateDefaultsSpec(defaults.Spec)
}

// ValidateCreate validates a new RightSizeNamespaceDefaults.
func (v *RightSizeNamespaceDefaultsValidator) ValidateCreate(_ context.Context, defaults *rightsizev1alpha1.RightSizeNamespaceDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("namespace_defaults_validate_create")
	defer timer.Observe()
	w, err := validateDefaultsSpec(defaults.Spec)
	timer.RecordResult(err)
	return w, err
}

// ValidateUpdate validates an updated RightSizeNamespaceDefaults.
func (v *RightSizeNamespaceDefaultsValidator) ValidateUpdate(_ context.Context, _, defaults *rightsizev1alpha1.RightSizeNamespaceDefaults) (admission.Warnings, error) {
	timer := operatormetrics.NewWebhookTimer("namespace_defaults_validate_update")
	defer timer.Observe()
	w, err := validateDefaultsSpec(defaults.Spec)
	timer.RecordResult(err)
	return w, err
}

// ValidateDelete validates a RightSizeNamespaceDefaults deletion (always succeeds).
func (v *RightSizeNamespaceDefaultsValidator) ValidateDelete(_ context.Context, _ *rightsizev1alpha1.RightSizeNamespaceDefaults) (admission.Warnings, error) {
	return nil, nil
}

func validateDefaultsSpec(spec rightsizev1alpha1.RightSizeDefaultsSpec) (admission.Warnings, error) {
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
	if spec.CPU != nil {
		if err := validateResourceConfigFields("cpu", spec.CPU); err != nil {
			return warnings, err
		}
	}

	// Validate memory resource config fields.
	if spec.Memory != nil {
		if err := validateResourceConfigFields("memory", spec.Memory); err != nil {
			return warnings, err
		}
		if spec.Memory.StartupBoost != nil {
			warnings = append(warnings, "memory.startupBoost has no effect; startup boost only applies to CPU resources")
		}
	}

	// Validate cooldown minimum floor.
	if spec.UpdateStrategy != nil && spec.UpdateStrategy.Cooldown != nil {
		cd := spec.UpdateStrategy.Cooldown.Duration
		if cd < 0 {
			return warnings, fmt.Errorf("updateStrategy.cooldown must be non-negative, got %s", cd)
		}
		if cd > 0 && cd < time.Minute {
			return warnings, fmt.Errorf("updateStrategy.cooldown must be at least 1m to prevent excessive reconciliation, got %s", cd)
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

	return warnings, nil
}

// validateResourceConfigFields validates fields that are shared between
// policy and defaults ResourceConfig. The prefix (e.g. "cpu", "memory")
// is used in error messages.
func validateResourceConfigFields(prefix string, rc *rightsizev1alpha1.ResourceConfig) error {
	// Overhead
	if err := validateOverhead(prefix, rc.Overhead); err != nil {
		return err
	}

	// BurstSensitivity
	if err := validateBurstSensitivity(prefix, rc.BurstSensitivity); err != nil {
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
