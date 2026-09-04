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

// Condition type constants for AttunePolicy.
const (
	ConditionReady           = "Ready"
	ConditionResizing        = "Resizing"
	ConditionDegraded        = "Degraded"
	ConditionScheduleBlocked = "ScheduleBlocked"
	// ConditionResizeBlocked is True when one or more target pods are stuck
	// Deferred (kubelet cannot accept yet) or Infeasible (cannot complete
	// in-place on the current node).
	ConditionResizeBlocked = "ResizeBlocked"
	// ConditionGitOpsPullRequest reports opt-in PR automation status.
	ConditionGitOpsPullRequest = "GitOpsPullRequest"
)

// Condition reason constants for AttunePolicy.
const (
	ReasonMonitoring       = "Monitoring"
	ReasonInsufficientData = "InsufficientData"
	// ReasonMetricsUnavailable is set when the configured metrics backend
	// (Prometheus, Datadog, or CloudWatch) cannot be resolved or queried.
	ReasonMetricsUnavailable = "MetricsUnavailable"
	// ReasonPrometheusUnavailable is the pre-0.1.26 Ready reason for the
	// same condition. Writers emit ReasonMetricsUnavailable. Readers treat
	// both as metrics-backend unavailable so existing status still skips
	// requeue jitter until the next reconcile.
	ReasonPrometheusUnavailable = "PrometheusUnavailable"
	// ReasonSeriesCapped means a Prometheus range query returned more series
	// than the configured cap; partial data was used for recommendations.
	ReasonSeriesCapped            = "PrometheusSeriesCapped"
	ReasonInvalidConfig           = "InvalidConfig"
	ReasonInProgress              = "InProgress"
	ReasonIdle                    = "Idle"
	ReasonCooldownActive          = "CooldownActive"
	ReasonHighRevertRate          = "HighRevertRate"
	ReasonNoWorkloadsFound        = "NoWorkloadsFound"
	ReasonWorkloadDiscoveryFailed = "WorkloadDiscoveryFailed"
	// ReasonConflictCheckFailed is set when listing AttunePolicies for
	// conflict detection fails. Recommendations from the last successful
	// cycle are kept; this cycle does not compute new ones.
	ReasonConflictCheckFailed = "ConflictCheckFailed"
	ReasonOutsideWindow       = "OutsideWindow"
	ReasonInsideWindow        = "InsideWindow"
	ReasonPaused              = "Paused"
	// ResizeBlocked condition reasons.
	ReasonPodsDeferred              = "PodsDeferred"
	ReasonPodsInfeasible            = "PodsInfeasible"
	ReasonPodsDeferredAndInfeasible = "PodsDeferredAndInfeasible"
	// GitOps PR automation reasons.
	ReasonGitOpsPROpen          = "PullRequestOpen"
	ReasonGitOpsPRFailed        = "PullRequestFailed"
	ReasonGitOpsPRNoDrift       = "NoDrift"
	ReasonGitOpsPRCooldown      = "PullRequestCooldown"
	ReasonGitOpsPRDryRun        = "PullRequestDryRun"
	ReasonGitOpsPRDisabled      = "PullRequestDisabled"
	ReasonGitOpsPRUnchanged     = "PullRequestUnchanged"
	ReasonGitOpsEndpointBlocked = "GitOpsEndpointBlocked"
)

// IsMetricsUnavailable reports whether a Ready reason means the metrics
// backend could not be used. Accepts both MetricsUnavailable and the
// pre-0.1.26 PrometheusUnavailable alias.
func IsMetricsUnavailable(reason string) bool {
	return reason == ReasonMetricsUnavailable || reason == ReasonPrometheusUnavailable
}

// CanaryPhaseInProgress and CanaryPhaseFullRollout are now typed constants
// defined in attunepolicy_types.go as CanaryPhase values.
