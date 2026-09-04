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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConstants(t *testing.T) {
	// Verify condition type constants.
	assert.Equal(t, "Ready", ConditionReady)
	assert.Equal(t, "Resizing", ConditionResizing)
	assert.Equal(t, "Degraded", ConditionDegraded)

	// Verify all reason constants are non-empty.
	reasons := []string{
		ReasonMonitoring,
		ReasonInsufficientData,
		ReasonMetricsUnavailable,
		ReasonPrometheusUnavailable,
		ReasonInvalidConfig,
		ReasonInProgress,
		ReasonIdle,
		ReasonCooldownActive,

		ReasonHighRevertRate,
	}
	for _, r := range reasons {
		assert.NotEmpty(t, r, "reason constant should not be empty")
	}
	assert.Equal(t, "MetricsUnavailable", ReasonMetricsUnavailable)
	assert.Equal(t, "PrometheusUnavailable", ReasonPrometheusUnavailable)
	assert.True(t, IsMetricsUnavailable(ReasonMetricsUnavailable))
	assert.True(t, IsMetricsUnavailable(ReasonPrometheusUnavailable))
	assert.False(t, IsMetricsUnavailable(ReasonInsufficientData))
}

func TestIsSupportedTargetKind(t *testing.T) {
	t.Parallel()

	supported := strings.Split(SupportedTargetKindsCSV, ", ")
	require.NotEmpty(t, supported)
	for _, kind := range supported {
		assert.True(t, IsSupportedTargetKind(kind), "CSV kind %q must be accepted", kind)
	}

	// Must stay aligned with the kubebuilder Enum on TargetRef.Kind.
	assert.Equal(t, []string{
		"Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "ReplicaSet",
	}, supported)

	rejects := []string{
		"",
		"deployment",
		"DEPLOYMENT",
		" Deployment",
		"Pod",
		"HorizontalPodAutoscaler",
		"Rollout",
		"ReplicaSet ",
	}
	for _, kind := range rejects {
		assert.False(t, IsSupportedTargetKind(kind), "kind %q must be rejected", kind)
	}
}
