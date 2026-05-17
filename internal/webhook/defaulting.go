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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

// RightSizePolicyDefaulter implements the typed Defaulter interface for RightSizePolicy.
type RightSizePolicyDefaulter struct{}

// Default sets default values on a RightSizePolicy.
//
// Most fields are NOT defaulted here. They are defaulted by the controller
// (applyBuiltInDefaults) after mergeDefaults, so that cluster-wide
// RightSizeDefaults and namespace-scoped RightSizeNamespaceDefaults can
// override built-in defaults. Only fields that are never overridable by
// cluster defaults (like Weight) are set here.
func (d *RightSizePolicyDefaulter) Default(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (err error) {
	timer := operatormetrics.NewWebhookTimer("defaulting")
	defer timer.Observe()
	defer func() { timer.RecordResult(err) }()
	if policy.Spec.Weight == 0 {
		policy.Spec.Weight = rightsizev1alpha1.DefaultWeight
	}
	return nil
}
