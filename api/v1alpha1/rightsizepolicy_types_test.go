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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetReadyTrue(t *testing.T) {
	status := &RightSizePolicyStatus{}
	status.SetReady(true, ReasonMonitoring, "All systems operational")

	assert.Len(t, status.Conditions, 1)
	assert.Equal(t, ConditionReady, status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, status.Conditions[0].Status)
	assert.Equal(t, ReasonMonitoring, status.Conditions[0].Reason)
	assert.Equal(t, "All systems operational", status.Conditions[0].Message)
	assert.False(t, status.Conditions[0].LastTransitionTime.IsZero(),
		"LastTransitionTime should be set")
}

func TestSetReadyFalse(t *testing.T) {
	status := &RightSizePolicyStatus{}
	status.SetReady(false, ReasonInsufficientData, "Not enough data points")

	assert.Len(t, status.Conditions, 1)
	assert.Equal(t, ConditionReady, status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)
	assert.Equal(t, ReasonInsufficientData, status.Conditions[0].Reason)
	assert.Equal(t, "Not enough data points", status.Conditions[0].Message)
}

func TestIsReadyNoConditions(t *testing.T) {
	status := &RightSizePolicyStatus{}
	assert.False(t, status.IsReady(), "should be false when no conditions exist")
}

func TestIsReadyTransitions(t *testing.T) {
	status := &RightSizePolicyStatus{}

	status.SetReady(true, ReasonMonitoring, "Ready")
	assert.True(t, status.IsReady(), "should be true after SetReady(true)")

	status.SetReady(false, ReasonInsufficientData, "Not ready")
	assert.False(t, status.IsReady(), "should be false after SetReady(false)")
}

func TestConditionTransitions(t *testing.T) {
	status := &RightSizePolicyStatus{}

	// Set initial condition to not ready.
	status.SetReady(false, ReasonInsufficientData, "Collecting data")
	assert.Len(t, status.Conditions, 1)
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)

	// Transition to ready; should update the existing condition, not add a new one.
	status.SetReady(true, ReasonMonitoring, "Sufficient data collected")
	assert.Len(t, status.Conditions, 1, "should update existing condition, not add new")
	assert.Equal(t, metav1.ConditionTrue, status.Conditions[0].Status)
	assert.Equal(t, ReasonMonitoring, status.Conditions[0].Reason)
	assert.Equal(t, "Sufficient data collected", status.Conditions[0].Message)

	// Transition back to not ready.
	status.SetReady(false, ReasonPrometheusUnavailable, "Lost connection")
	assert.Len(t, status.Conditions, 1, "should still have exactly one Ready condition")
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)
	assert.Equal(t, ReasonPrometheusUnavailable, status.Conditions[0].Reason)
}

func TestConditionReasonUpdate(t *testing.T) {
	status := &RightSizePolicyStatus{}

	// Set initial condition.
	status.SetReady(false, ReasonInsufficientData, "Collecting data")
	initialTime := status.Conditions[0].LastTransitionTime

	// Update reason without changing status.
	status.SetReady(false, ReasonPrometheusUnavailable, "Connection lost")

	assert.Len(t, status.Conditions, 1)
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)
	assert.Equal(t, ReasonPrometheusUnavailable, status.Conditions[0].Reason)
	assert.Equal(t, "Connection lost", status.Conditions[0].Message)
	// LastTransitionTime should be preserved when status does not change.
	assert.Equal(t, initialTime, status.Conditions[0].LastTransitionTime,
		"LastTransitionTime should not change when status stays the same")
}

func TestDefaultConstants(t *testing.T) {
	// Verify condition type constants.
	assert.Equal(t, "Ready", ConditionReady)
	assert.Equal(t, "Resizing", ConditionResizing)
	assert.Equal(t, "Degraded", ConditionDegraded)

	// Verify all reason constants are non-empty.
	reasons := []string{
		ReasonMonitoring,
		ReasonInsufficientData,
		ReasonPrometheusUnavailable,
		ReasonInvalidConfig,
		ReasonInProgress,
		ReasonIdle,
		ReasonCooldownActive,
		ReasonPartialFailure,
		ReasonHighRevertRate,
	}
	for _, r := range reasons {
		assert.NotEmpty(t, r, "reason constant should not be empty")
	}
}
