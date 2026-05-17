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
	// Validate Prometheus address if provided (SSRF prevention).
	if spec.MetricsSource != nil &&
		spec.MetricsSource.Prometheus != nil &&
		spec.MetricsSource.Prometheus.Address != "" {
		if err := ValidatePrometheusAddress(spec.MetricsSource.Prometheus.Address); err != nil {
			return nil, fmt.Errorf("metricsSource.prometheus.address: %w", err)
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

	// Warn if memory startup boost is set (only CPU boost is implemented).
	var warnings admission.Warnings
	if spec.Memory != nil && spec.Memory.StartupBoost != nil {
		warnings = append(warnings, "memory.startupBoost has no effect; startup boost only applies to CPU resources")
	}

	return warnings, nil
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
