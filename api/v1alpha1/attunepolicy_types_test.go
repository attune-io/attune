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
}
