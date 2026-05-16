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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

// RightSizePolicyDefaulter implements the typed Defaulter interface for RightSizePolicy.
type RightSizePolicyDefaulter struct{}

// Default sets default values on a RightSizePolicy.
func (d *RightSizePolicyDefaulter) Default(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (err error) {
	timer := operatormetrics.NewWebhookTimer("defaulting")
	defer timer.Observe()
	defer func() { timer.RecordResult(err) }()
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.DefaultUpdateMode
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = rightsizev1alpha1.DefaultMaxCPUChangePercent
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == 0 {
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = rightsizev1alpha1.DefaultMaxMemoryChangePercent
	}
	if policy.Spec.Weight == 0 {
		policy.Spec.Weight = rightsizev1alpha1.DefaultWeight
	}
	if policy.Spec.CPU.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.CPU.ControlledValues = &cv
	}
	if policy.Spec.Memory.ControlledValues == nil {
		cv := rightsizev1alpha1.DefaultControlledValues
		policy.Spec.Memory.ControlledValues = &cv
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		d, err := time.ParseDuration(rightsizev1alpha1.DefaultHistoryWindow)
		if err != nil {
			return fmt.Errorf("parsing default historyWindow %q: %w", rightsizev1alpha1.DefaultHistoryWindow, err)
		}
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: d}
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		d, err := time.ParseDuration(rightsizev1alpha1.DefaultCooldown)
		if err != nil {
			return fmt.Errorf("parsing default cooldown %q: %w", rightsizev1alpha1.DefaultCooldown, err)
		}
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: d}
	}
	return nil
}
