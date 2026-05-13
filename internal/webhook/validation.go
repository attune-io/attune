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

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardif/kube-rightsize/internal/validation"
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

	// CPU bounds: min must be <= max
	if policy.Spec.CPU.Bounds != nil {
		if policy.Spec.CPU.Bounds.Min.Cmp(policy.Spec.CPU.Bounds.Max) > 0 {
			return warnings, fmt.Errorf("cpu.bounds.min (%s) must be <= cpu.bounds.max (%s)",
				policy.Spec.CPU.Bounds.Min.String(), policy.Spec.CPU.Bounds.Max.String())
		}
	}

	// Memory bounds: min must be <= max
	if policy.Spec.Memory.Bounds != nil {
		if policy.Spec.Memory.Bounds.Min.Cmp(policy.Spec.Memory.Bounds.Max) > 0 {
			return warnings, fmt.Errorf("memory.bounds.min (%s) must be <= memory.bounds.max (%s)",
				policy.Spec.Memory.Bounds.Min.String(), policy.Spec.Memory.Bounds.Max.String())
		}
	}

	// Canary config required when mode is Canary
	if policy.Spec.UpdateStrategy.Mode == rightsizev1alpha1.ModeCanary && policy.Spec.UpdateStrategy.Canary == nil {
		return warnings, fmt.Errorf("updateStrategy.canary is required when mode is Canary")
	}

	// targetRef must have name or selector
	if (policy.Spec.TargetRef.Name == nil || *policy.Spec.TargetRef.Name == "") && policy.Spec.TargetRef.Selector == nil {
		return warnings, fmt.Errorf("targetRef must specify either name or selector")
	}

	// Validate safetyMargin is a valid positive float.
	if w, err := validateSafetyMargin("cpu", policy.Spec.CPU.SafetyMargin); err != nil {
		return warnings, err
	} else if w != "" {
		warnings = append(warnings, w)
	}
	if w, err := validateSafetyMargin("memory", policy.Spec.Memory.SafetyMargin); err != nil {
		return warnings, err
	} else if w != "" {
		warnings = append(warnings, w)
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

	// Validate Prometheus address URL scheme if specified.
	if policy.Spec.MetricsSource.Prometheus != nil && policy.Spec.MetricsSource.Prometheus.Address != "" {
		if err := ValidatePrometheusAddress(policy.Spec.MetricsSource.Prometheus.Address); err != nil {
			return warnings, fmt.Errorf("metricsSource.prometheus.address: %w", err)
		}
	}

	// Warn if memory decrease is enabled
	if policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease {
		warnings = append(warnings, "memory.allowDecrease is enabled; this carries OOMKill risk")
	}

	return warnings, nil
}

func validateSafetyMargin(resource, margin string) (warning string, err error) {
	if margin == "" {
		return "", nil
	}
	v, err := strconv.ParseFloat(margin, 64)
	if err != nil {
		return "", fmt.Errorf("%s.safetyMargin %q is not a valid number: %w", resource, margin, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", fmt.Errorf("%s.safetyMargin must be a finite number, got %s", resource, margin)
	}
	if v <= 0 {
		return "", fmt.Errorf("%s.safetyMargin must be positive, got %s", resource, margin)
	}
	// Upper bound prevents excessive resource allocation that could exhaust nodes.
	if v > 10.0 {
		return "", fmt.Errorf("%s.safetyMargin must be <= 10.0, got %s", resource, margin)
	}
	if v < 1.0 {
		return fmt.Sprintf(
			"%s.safetyMargin %.2f is below 1.0 and will reduce resources below the target percentile; did you mean %.1f?",
			resource, v, 1+v), nil
	}
	return "", nil
}

// ValidatePrometheusAddress delegates to the shared validation package.
// Kept as a wrapper for backward compatibility with webhook callers.
func ValidatePrometheusAddress(address string) error {
	return validation.PrometheusAddress(address)
}
