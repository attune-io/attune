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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition type constants for RightSizePolicy.
const (
	ConditionReady    = "Ready"
	ConditionResizing = "Resizing"
	ConditionDegraded = "Degraded"
)

// Condition reason constants for RightSizePolicy.
const (
	ReasonMonitoring            = "Monitoring"
	ReasonInsufficientData      = "InsufficientData"
	ReasonPrometheusUnavailable = "PrometheusUnavailable"
	ReasonInvalidConfig         = "InvalidConfig"
	ReasonInProgress            = "InProgress"
	ReasonIdle                  = "Idle"
	ReasonCooldownActive        = "CooldownActive"
	ReasonPartialFailure        = "PartialFailure"
	ReasonHighRevertRate        = "HighRevertRate"
)

// SetReady sets the Ready condition on the policy status.
func (s *RightSizePolicyStatus) SetReady(ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// IsReady returns true if the Ready condition is True.
func (s *RightSizePolicyStatus) IsReady() bool {
	return meta.IsStatusConditionTrue(s.Conditions, ConditionReady)
}
