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
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

// RightSizeDefaultsValidator validates RightSizeDefaults resources.
type RightSizeDefaultsValidator struct{}

// ValidateCreate validates a new RightSizeDefaults.
func (v *RightSizeDefaultsValidator) ValidateCreate(_ context.Context, defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	return v.validate(defaults)
}

// ValidateUpdate validates an updated RightSizeDefaults.
func (v *RightSizeDefaultsValidator) ValidateUpdate(_ context.Context, _, defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	return v.validate(defaults)
}

// ValidateDelete validates a RightSizeDefaults deletion (always succeeds).
func (v *RightSizeDefaultsValidator) ValidateDelete(_ context.Context, _ *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	return nil, nil
}

func (v *RightSizeDefaultsValidator) validate(defaults *rightsizev1alpha1.RightSizeDefaults) (admission.Warnings, error) {
	// Validate Prometheus address if provided (SSRF prevention).
	if defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Prometheus != nil &&
		defaults.Spec.MetricsSource.Prometheus.Address != "" {
		if err := ValidatePrometheusAddress(defaults.Spec.MetricsSource.Prometheus.Address); err != nil {
			return nil, fmt.Errorf("metricsSource.prometheus.address: %w", err)
		}
	}

	if defaults.Spec.CostPricing == nil {
		return nil, nil
	}

	if err := validatePositiveFloat("costPricing.cpuPerCoreHour", defaults.Spec.CostPricing.CPUPerCoreHour); err != nil {
		return nil, err
	}
	if err := validatePositiveFloat("costPricing.memoryPerGiBHour", defaults.Spec.CostPricing.MemoryPerGiBHour); err != nil {
		return nil, err
	}

	return nil, nil
}

func validatePositiveFloat(field, value string) error {
	if value == "" {
		return nil
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid number: %w", field, value, err)
	}
	if v <= 0 {
		return fmt.Errorf("%s must be positive, got %s", field, value)
	}
	return nil
}
