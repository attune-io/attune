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

// Condition type constants for RightSizePolicy.
const (
	ConditionReady           = "Ready"
	ConditionResizing        = "Resizing"
	ConditionDegraded        = "Degraded"
	ConditionScheduleBlocked = "ScheduleBlocked"
)

// Condition reason constants for RightSizePolicy.
const (
	ReasonMonitoring              = "Monitoring"
	ReasonInsufficientData        = "InsufficientData"
	ReasonPrometheusUnavailable   = "PrometheusUnavailable"
	ReasonInvalidConfig           = "InvalidConfig"
	ReasonInProgress              = "InProgress"
	ReasonIdle                    = "Idle"
	ReasonCooldownActive          = "CooldownActive"
	ReasonHighRevertRate          = "HighRevertRate"
	ReasonNoWorkloadsFound        = "NoWorkloadsFound"
	ReasonWorkloadDiscoveryFailed = "WorkloadDiscoveryFailed"
	ReasonOutsideWindow           = "OutsideWindow"
	ReasonInsideWindow            = "InsideWindow"
	ReasonPaused                  = "Paused"
)

// CanaryPhaseInProgress and CanaryPhaseFullRollout are now typed constants
// defined in rightsizepolicy_types.go as CanaryPhase values.
