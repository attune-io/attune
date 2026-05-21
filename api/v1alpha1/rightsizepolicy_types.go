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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpdateMode defines the operating mode of a RightSizePolicy.
type UpdateMode string

const (
	UpdateModeObserve   UpdateMode = "Observe"
	UpdateModeRecommend UpdateMode = "Recommend"
	UpdateModeOneShot   UpdateMode = "OneShot"
	UpdateModeCanary    UpdateMode = "Canary"
	UpdateModeAuto      UpdateMode = "Auto"
)

// ResizeMethodType defines the resize fallback strategy.
type ResizeMethodType string

const (
	ResizeMethodInPlaceOnly    ResizeMethodType = "InPlaceOnly"
	ResizeMethodInPlaceOrEvict ResizeMethodType = "InPlaceOrEvict"
)

// CanaryPhase defines the canary rollout state.
type CanaryPhase string

const (
	CanaryPhaseInProgress  CanaryPhase = "CanaryInProgress"
	CanaryPhaseFullRollout CanaryPhase = "FullRollout"
)

// ResizeResult defines the outcome of a resize operation.
type ResizeResult string

const (
	ResizeResultSuccess  ResizeResult = "Success"
	ResizeResultFailed   ResizeResult = "Failed"
	ResizeResultReverted ResizeResult = "Reverted"
	ResizeResultEvicted  ResizeResult = "Evicted"
)

// SupportedTargetKindsCSV is the canonical runtime list of workload kinds
// accepted by RightSizePolicy targetRef.kind. Keep it in sync with the
// kubebuilder enum on TargetRef.Kind.
const SupportedTargetKindsCSV = "Deployment, StatefulSet, DaemonSet, CronJob, Job"

// IsSupportedTargetKind reports whether kind is a supported targetRef.kind
// value at runtime.
func IsSupportedTargetKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job":
		return true
	default:
		return false
	}
}

// RightSizePolicySpec defines the desired state of RightSizePolicy.
type RightSizePolicySpec struct {
	// TargetRef identifies the workload(s) to be rightsized.
	TargetRef TargetRef `json:"targetRef"`

	// MetricsSource configures where and how to collect metrics.
	MetricsSource MetricsSource `json:"metricsSource"`

	// CPU configures CPU resource recommendations.
	CPU ResourceConfig `json:"cpu"`

	// Memory configures memory resource recommendations.
	Memory ResourceConfig `json:"memory"`

	// UpdateStrategy configures how and when to apply resource changes.
	// +optional
	UpdateStrategy UpdateStrategy `json:"updateStrategy,omitempty"`

	// ExcludeContainers is a list of container names to skip when computing
	// recommendations and performing resizes. Use this for sidecar containers
	// (e.g., istio-proxy, linkerd-proxy) that are managed by a service mesh
	// and should not be right-sized.
	// +optional
	ExcludeContainers []string `json:"excludeContainers,omitempty"`

	// Weight determines the priority of this policy when multiple policies
	// match the same workload. Higher values take precedence.
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Weight int32 `json:"weight,omitempty"`
}

// TargetRef identifies the target workload(s).
type TargetRef struct {
	// Kind is the kind of the target resource.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;CronJob;Job
	Kind string `json:"kind"`

	// Name is the name of a specific target resource.
	// +optional
	Name *string `json:"name,omitempty"`

	// Selector selects target resources by labels.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// MetricsSource configures the source of metrics data.
type MetricsSource struct {
	// Prometheus configures a Prometheus metrics source.
	// +optional
	Prometheus *PrometheusConfig `json:"prometheus,omitempty"`

	// HistoryWindow is the time window for historical metrics data.
	// Defaults to 7d (168h) if not specified.
	// +optional
	HistoryWindow *metav1.Duration `json:"historyWindow,omitempty"`

	// MinimumDataPoints is the minimum number of data points required
	// before generating recommendations. Minimum 1, default 48 samples.
	// With the default queryStep of 5m, 48 samples is about 4 hours of data.
	// Defaults to 48 if not set (applied by the controller so that
	// RightSizeDefaults cluster configuration can override it).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinimumDataPoints *int32 `json:"minimumDataPoints,omitempty"`

	// QueryStep is the step interval for Prometheus range queries and ETA
	// calculations. Should match your Prometheus scrape interval for
	// accurate time estimates. Minimum 10s, maximum 1h. Default 5m.
	// +optional
	QueryStep *metav1.Duration `json:"queryStep,omitempty"`
}

// PrometheusConfig configures a Prometheus-compatible metrics source.
// Works with Thanos, VictoriaMetrics, Grafana Mimir, and managed
// Prometheus services (AMP, GMP) that implement the Prometheus HTTP API.
type PrometheusConfig struct {
	// Address is the URL of the Prometheus-compatible query endpoint.
	Address string `json:"address"`

	// Headers are custom HTTP headers added to every query request.
	// Use for tenant IDs (e.g. "X-Scope-OrgID" for Mimir), API keys,
	// or other auth headers required by the backend.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// QueryParameters are appended to every query request URL.
	// Use for backend-specific settings such as Thanos deduplication
	// (e.g. {"dedup": "true", "partial_response": "true"}). Reserved
	// query keys controlled by the operator (`query`, `start`, `end`, `step`,
	// `time`, `timeout`) are rejected.
	// +optional
	QueryParameters map[string]string `json:"queryParameters,omitempty"`

	// BearerTokenSecret references a Kubernetes Secret containing a bearer
	// token for authenticating with managed Prometheus services.
	// +optional
	BearerTokenSecret *SecretKeyRef `json:"bearerTokenSecret,omitempty"`

	// TLS configures TLS settings for the connection.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
}

// SecretKeyRef references a key within a Kubernetes Secret.
type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key within the Secret.
	Key string `json:"key"`
}

// TLSConfig defines TLS settings for Prometheus connections.
type TLSConfig struct {
	// InsecureSkipVerify disables TLS certificate verification.
	// Use only for self-signed certificates in development.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ResourceConfig defines resource recommendation parameters.
type ResourceConfig struct {
	// Percentile is the usage percentile to target for recommendations.
	// Supported values: 50, 90, 95, 99. Omit or set to 0 to use the default.
	// +kubebuilder:validation:Enum=0;50;90;95;99
	// +optional
	Percentile int32 `json:"percentile,omitempty"`

	// SafetyMargin is a multiplier applied to the percentile recommendation.
	// Expressed as a decimal string (e.g. "1.2" means 20% headroom above the
	// target percentile). Must be a positive number, max 10.0. Values below
	// 1.0 reduce resources below the percentile and generate a warning.
	// +optional
	SafetyMargin string `json:"safetyMargin,omitempty"`

	// Bounds defines the minimum and maximum allowed resource values.
	// +optional
	Bounds *ResourceBounds `json:"bounds,omitempty"`

	// ControlledValues specifies which resource values to manage.
	// "RequestsOnly" (default) adjusts only requests, leaving limits unchanged.
	// "RequestsAndLimits" adjusts both requests and limits in lockstep.
	// For Guaranteed-QoS pods (where requests equal limits), use
	// "RequestsAndLimits" or resizes will be skipped to preserve QoS class.
	// +kubebuilder:validation:Enum=RequestsOnly;RequestsAndLimits
	// +optional
	ControlledValues *string `json:"controlledValues,omitempty"`

	// BurstSensitivity controls how much burst detection inflates the
	// recommendation. Expressed as a decimal string multiplied by
	// log2(burstMagnitude). Default "0.1" gives ~20% boost for magnitude 4,
	// ~30% for 8, ~40% for 16. Set "0" to disable burst boost entirely
	// (e.g. for batch jobs). Must be >= 0, max 1.0.
	// +optional
	BurstSensitivity *string `json:"burstSensitivity,omitempty"`

	// AllowDecrease controls whether the resource value can be decreased.
	// For CPU: nil defaults to true (decreases allowed, throttle detected by safety monitor).
	// For memory: nil defaults to false (decreases blocked to prevent OOMKill).
	// +optional
	AllowDecrease *bool `json:"allowDecrease,omitempty"`

	// StartupBoost temporarily increases CPU requests for newly created or
	// restarted pods to accelerate JVM/.NET class loading, JIT compilation,
	// and cache warming. After the duration expires (or the container reaches
	// Ready), the CPU is reduced to the steady-state recommendation.
	// Only applies to CPU resources.
	// +optional
	StartupBoost *StartupBoost `json:"startupBoost,omitempty"`
}

// StartupBoost configures temporary CPU inflation for cold-start optimization.
type StartupBoost struct {
	// Multiplier scales the recommended CPU request during startup.
	// For example, "3.0" means 3x the steady-state recommendation.
	// Must be > 1.0 and <= 10.0.
	// +kubebuilder:validation:Required
	Multiplier string `json:"multiplier"`

	// Duration is the maximum time the boost remains active after pod
	// creation or container restart. The boost is removed when the
	// container reaches Ready or this duration expires, whichever comes first.
	// Must be >= 10s and <= 1h.
	// +kubebuilder:validation:Required
	Duration metav1.Duration `json:"duration"`
}

// ResourceBounds defines the minimum and maximum resource values.
type ResourceBounds struct {
	// Min is the minimum allowed resource value.
	// +kubebuilder:validation:Required
	Min resource.Quantity `json:"min"`

	// Max is the maximum allowed resource value.
	// +kubebuilder:validation:Required
	Max resource.Quantity `json:"max"`
}

// UpdateStrategy configures how resource changes are applied.
type UpdateStrategy struct {
	// Mode determines the update behavior, graduated from safe to automated:
	//   Recommend: collects metrics and writes recommendations to status, no pod changes.
	//   OneShot: resizes one pod per reconcile cycle.
	//   Canary: resizes a percentage of pods first, then the rest after observation.
	//   Auto: resizes all eligible pods each cycle.
	//   Observe: collects metrics and tracks data points but does not surface recommendations or savings.
	// Start with Recommend in production and promote after reviewing status.
	// Defaults to Recommend if not set (applied by the controller, not the webhook,
	// so that RightSizeDefaults cluster configuration can override it).
	// +kubebuilder:validation:Enum=Observe;Recommend;OneShot;Canary;Auto
	// +optional
	Mode UpdateMode `json:"mode,omitempty"`

	// Canary configures canary rollout behavior when Mode is Canary.
	// +optional
	Canary *CanaryConfig `json:"canary,omitempty"`

	// MaxCPUChangePercent is the maximum allowed CPU change percentage per operation.
	// Defaults to 50 if not set (applied by the controller so that
	// RightSizeDefaults cluster configuration can override it).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxCPUChangePercent *int32 `json:"maxCpuChangePercent,omitempty"`

	// MaxMemoryChangePercent is the maximum allowed memory change percentage per operation.
	// Defaults to 30 if not set (applied by the controller so that
	// RightSizeDefaults cluster configuration can override it).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxMemoryChangePercent *int32 `json:"maxMemoryChangePercent,omitempty"`

	// Cooldown is the minimum time between successive resize operations.
	// Defaults to 1h if not specified.
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`

	// AutoRevert automatically reverts changes if degradation is detected.
	// Defaults to true if not set (applied by the controller so that
	// RightSizeDefaults cluster configuration can override it).
	// +optional
	AutoRevert *bool `json:"autoRevert,omitempty"`

	// ResizeMethod controls what happens when an in-place resize fails.
	//   InPlaceOnly (default): skip the pod and retry next cycle.
	//   InPlaceOrEvict: fall back to pod eviction if in-place resize
	//   fails or is marked Infeasible by kubelet. Evictions respect
	//   PodDisruptionBudgets and never evict the last replica.
	// Defaults to InPlaceOnly if not set (applied by the controller so that
	// RightSizeDefaults cluster configuration can override it).
	// +kubebuilder:validation:Enum=InPlaceOnly;InPlaceOrEvict
	// +optional
	ResizeMethod ResizeMethodType `json:"resizeMethod,omitempty"`

	// Schedule restricts when resize operations can occur. Recommendations
	// are always computed; only resize execution is gated. If omitted,
	// resizes can occur at any time (current behavior).
	// +optional
	Schedule *ResizeSchedule `json:"schedule,omitempty"`

	// MaxConcurrentResizes is the maximum number of pods to resize
	// concurrently within a single reconcile cycle. Default: 1 (serial).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=50
	// +optional
	MaxConcurrentResizes int32 `json:"maxConcurrentResizes,omitempty"`

	// MaxTotalCPUIncrease is the maximum aggregate CPU increase allowed
	// across all pods in a single reconcile cycle (e.g. "2000m", "4").
	// Once exhausted, remaining pods are deferred to the next cycle.
	// Decreases do not consume budget. Default: unlimited.
	// +optional
	MaxTotalCPUIncrease *resource.Quantity `json:"maxTotalCpuIncrease,omitempty"`

	// MaxTotalMemoryIncrease is the maximum aggregate memory increase
	// allowed across all pods in a single reconcile cycle (e.g. "4Gi").
	// Once exhausted, remaining pods are deferred to the next cycle.
	// Decreases do not consume budget. Default: unlimited.
	// +optional
	MaxTotalMemoryIncrease *resource.Quantity `json:"maxTotalMemoryIncrease,omitempty"`

	// Export configures how recommendations are exported for external
	// consumption (e.g. GitOps workflows with ArgoCD or Flux).
	// +optional
	Export *ExportConfig `json:"export,omitempty"`
}

// ExportConfig controls recommendation export to external systems.
type ExportConfig struct {
	// ConfigMap enables exporting recommendations to ConfigMaps.
	// One ConfigMap per workload is created, named
	// "<policy>-<workload>-recommendations", with an owner reference
	// to the policy for automatic cleanup.
	// +optional
	ConfigMap bool `json:"configMap,omitempty"`
}

// CanaryConfig defines canary rollout parameters.
type CanaryConfig struct {
	// Percentage is the percentage of pods to resize first.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percentage int32 `json:"percentage"`

	// ObservationPeriod is how long to observe canary pods before proceeding.
	ObservationPeriod metav1.Duration `json:"observationPeriod"`

	// AutoPromote controls whether the operator automatically promotes the
	// resize to all pods after the observation period passes without safety
	// violations. When false (default), the user must manually switch the
	// mode to Auto to resize the remaining pods.
	// +optional
	AutoPromote bool `json:"autoPromote,omitempty"`
}

// ResizeSchedule restricts when resize operations can be applied.
type ResizeSchedule struct {
	// Windows defines time-of-day ranges when resizes are allowed.
	// If multiple windows are specified, resizes are allowed during any of them.
	// +optional
	Windows []TimeWindow `json:"windows,omitempty"`

	// DaysOfWeek restricts resizes to specific days. Values: Monday through Sunday.
	// If omitted, all days are allowed.
	// +optional
	// +kubebuilder:validation:items:Enum=Monday;Tuesday;Wednesday;Thursday;Friday;Saturday;Sunday
	DaysOfWeek []string `json:"daysOfWeek,omitempty"`

	// Timezone for interpreting window start/end times. Must be a valid
	// IANA timezone name (e.g. "America/New_York"). Default: "UTC".
	// +kubebuilder:default=UTC
	// +optional
	Timezone string `json:"timezone,omitempty"`
}

// TimeWindow defines a daily time range.
type TimeWindow struct {
	// Start time in HH:MM format (24-hour).
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	Start string `json:"start"`

	// End time in HH:MM format (24-hour). If end < start, the window
	// wraps past midnight (e.g. start=22:00, end=06:00).
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	End string `json:"end"`
}

// RightSizePolicyStatus defines the observed state of RightSizePolicy.
type RightSizePolicyStatus struct {
	// Conditions represent the latest available observations of the policy's state.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Workloads summarizes workload discovery and resize counts.
	// +optional
	Workloads WorkloadStatus `json:"workloads,omitempty"`

	// Recommendations contains per-workload resource recommendations.
	// +kubebuilder:validation:MaxItems=500
	// +optional
	Recommendations []WorkloadRecommendation `json:"recommendations,omitempty"`

	// Savings summarizes estimated resource savings.
	// +optional
	Savings SavingsStatus `json:"savings,omitempty"`

	// ResizeHistory records past resize operations.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	ResizeHistory []ResizeHistoryEntry `json:"resizeHistory,omitempty"`

	// Canary tracks the canary rollout phase when autoPromote is enabled.
	// +optional
	Canary *CanaryStatus `json:"canary,omitempty"`

	// LastReconcileTime is the timestamp of the most recent reconciliation.
	// Serves as a heartbeat to confirm the operator is actively evaluating
	// this policy, even when no state changes occur.
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
}

// CanaryStatus tracks the canary rollout progression.
type CanaryStatus struct {
	// Phase indicates the current canary state.
	// CanaryInProgress: canary pods resized, observing for safety violations.
	// FullRollout: observation passed with no violations, all pods are being resized.
	// +optional
	Phase CanaryPhase `json:"phase,omitempty"`

	// StartTime is when the canary subset was first resized.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// ObservedGeneration is the policy generation when this canary cycle
	// started. If the policy spec changes (generation increments), the
	// canary observation resets so the new configuration is re-validated.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// WorkloadStatus summarizes workload counts.
type WorkloadStatus struct {
	// Discovered is the number of workloads matching the target selector.
	Discovered int32 `json:"discovered"`

	// WithRecommendations is the number of workloads with active recommendations.
	WithRecommendations int32 `json:"withRecommendations"`

	// Resized is the number of workloads that have been resized.
	Resized int32 `json:"resized"`

	// Pending is the number of workloads awaiting resize.
	Pending int32 `json:"pending"`

	// DataPointsCollected is the maximum number of data points collected across
	// all containers in the discovered workloads.
	// +optional
	DataPointsCollected int32 `json:"dataPointsCollected,omitempty"`

	// DataPointsRequired is the minimum number of data points needed before
	// generating recommendations (from metricsSource.minimumDataPoints).
	// +optional
	DataPointsRequired int32 `json:"dataPointsRequired,omitempty"`
}

// WorkloadRecommendation contains recommendations for a single workload.
type WorkloadRecommendation struct {
	// Workload is the name of the workload.
	Workload string `json:"workload"`

	// Kind is the kind of the workload (e.g. Deployment, StatefulSet).
	Kind string `json:"kind"`

	// Containers contains per-container recommendations.
	Containers []ContainerRecommendation `json:"containers"`
}

// ContainerRecommendation contains recommendations for a single container.
type ContainerRecommendation struct {
	// Name is the container name.
	Name string `json:"name"`

	// Current contains the current resource values.
	Current ResourceValues `json:"current"`

	// Recommended contains the recommended resource values.
	Recommended ResourceValues `json:"recommended"`

	// Explanation contains the reasoning chain behind the recommendation.
	// +optional
	Explanation *ContainerRecommendationExplanation `json:"explanation,omitempty"`

	// Confidence is the confidence score of the recommendation (0-1).
	Confidence float64 `json:"confidence"`

	// DataPoints is the number of data points used to generate the recommendation.
	DataPoints int32 `json:"dataPoints"`

	// LastUpdated is the timestamp of the last recommendation update.
	LastUpdated metav1.Time `json:"lastUpdated"`
}

// ContainerRecommendationExplanation stores per-resource recommendation reasoning.
type ContainerRecommendationExplanation struct {
	// CPU contains the CPU recommendation reasoning.
	// +optional
	CPU *ResourceRecommendationExplanation `json:"cpu,omitempty"`

	// Memory contains the memory recommendation reasoning.
	// +optional
	Memory *ResourceRecommendationExplanation `json:"memory,omitempty"`
}

// ResourceRecommendationExplanation captures the estimator chain output for one resource.
type ResourceRecommendationExplanation struct {
	// RawPercentile is the selected percentile before any adjustments.
	RawPercentile resource.Quantity `json:"rawPercentile"`

	// SafetyMargin is the configured safety multiplier applied to the raw percentile.
	SafetyMargin float64 `json:"safetyMargin"`

	// AfterSafetyMargin is the value after applying the safety margin.
	AfterSafetyMargin resource.Quantity `json:"afterSafetyMargin"`

	// BurstFactor is the multiplier applied when burst is detected (max > 3x p95).
	// 1.0 when no burst. Uses logarithmic scaling to avoid excessive inflation.
	// +optional
	BurstFactor float64 `json:"burstFactor,omitempty"`

	// AfterBurst is the value after applying the burst factor.
	// +optional
	AfterBurst resource.Quantity `json:"afterBurst,omitempty"`

	// Confidence is the profile confidence score used for adjustment.
	Confidence float64 `json:"confidence"`

	// ConfidenceFactor is the multiplier derived from the confidence score.
	ConfidenceFactor float64 `json:"confidenceFactor"`

	// AfterConfidence is the value after applying the confidence adjustment.
	AfterConfidence resource.Quantity `json:"afterConfidence"`

	// Bounds are the configured minimum and maximum limits.
	Bounds ResourceBounds `json:"bounds"`

	// BoundsApplied indicates whether the value was clamped to min, max, or neither.
	// +optional
	BoundsApplied string `json:"boundsApplied,omitempty"`

	// AfterBounds is the value after bounds clamping.
	AfterBounds resource.Quantity `json:"afterBounds"`

	// MinChangePercent is the minimum change threshold required to alter the current value.
	MinChangePercent float64 `json:"minChangePercent"`

	// MaxChangePercent is the maximum allowed change threshold.
	MaxChangePercent float64 `json:"maxChangePercent"`

	// ChangeFilterApplied indicates whether the result was filtered or capped.
	// +optional
	ChangeFilterApplied string `json:"changeFilterApplied,omitempty"`

	// AfterChangeFilter is the value after change filtering.
	AfterChangeFilter resource.Quantity `json:"afterChangeFilter"`

	// Final is the final resource recommendation after any post-processing.
	Final resource.Quantity `json:"final"`

	// FinalAdjustment describes any controller-level adjustment after the estimator chain.
	// +optional
	FinalAdjustment string `json:"finalAdjustment,omitempty"`
}

// ResourceValues represents CPU and memory resource values.
type ResourceValues struct {
	// CPURequest is the CPU request value.
	CPURequest resource.Quantity `json:"cpuRequest"`

	// CPULimit is the CPU limit value.
	CPULimit resource.Quantity `json:"cpuLimit"`

	// MemoryRequest is the memory request value.
	MemoryRequest resource.Quantity `json:"memoryRequest"`

	// MemoryLimit is the memory limit value.
	MemoryLimit resource.Quantity `json:"memoryLimit"`
}

// ResizeHistoryEntry records a single resize operation.
type ResizeHistoryEntry struct {
	// Timestamp is when the resize operation occurred.
	Timestamp metav1.Time `json:"timestamp"`

	// Workload is the name of the resized workload.
	Workload string `json:"workload"`

	// Container is the name of the resized container.
	Container string `json:"container"`

	// Resource is the resource type that was resized.
	// +kubebuilder:validation:Enum=cpu;memory;cpu+memory
	Resource string `json:"resource"`

	// From is the previous resource value.
	From string `json:"from"`

	// To is the new resource value.
	To string `json:"to"`

	// Method is the resize method used.
	// +kubebuilder:validation:Enum=InPlace;Eviction
	Method string `json:"method"`

	// Result is the outcome of the resize operation.
	// +kubebuilder:validation:Enum=Success;Failed;Reverted;Evicted
	Result ResizeResult `json:"result"`
}

// SavingsStatus summarizes estimated resource savings.
type SavingsStatus struct {
	// CPURequestReduction is the total CPU request reduction (e.g. "200m").
	CPURequestReduction string `json:"cpuRequestReduction,omitempty"`

	// CPURequestTotal is the total current CPU requests across all workloads (e.g. "2000m").
	// +optional
	CPURequestTotal string `json:"cpuRequestTotal,omitempty"`

	// MemoryRequestReduction is the total memory request reduction (e.g. "256Mi").
	MemoryRequestReduction string `json:"memoryRequestReduction,omitempty"`

	// MemoryRequestTotal is the total current memory requests across all workloads (e.g. "2Gi").
	// +optional
	MemoryRequestTotal string `json:"memoryRequestTotal,omitempty"`

	// EstimatedMonthlySavings is the estimated monthly cost savings based on
	// configured or default pricing (e.g. "$12.50").
	EstimatedMonthlySavings string `json:"estimatedMonthlySavings,omitempty"`

	// CPURequestIncrease is the total CPU request increase for under-provisioned
	// workloads (e.g. "500m"). Empty when all recommendations are decreases.
	// +optional
	CPURequestIncrease string `json:"cpuRequestIncrease,omitempty"`

	// MemoryRequestIncrease is the total memory request increase for under-provisioned
	// workloads (e.g. "512Mi"). Empty when all recommendations are decreases.
	// +optional
	MemoryRequestIncrease string `json:"memoryRequestIncrease,omitempty"`

	// EstimatedMonthlyCostIncrease is the estimated monthly cost increase for
	// under-provisioned workloads based on configured or default pricing (e.g. "$5.00").
	// +optional
	EstimatedMonthlyCostIncrease string `json:"estimatedMonthlyCostIncrease,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rsp,categories={rightsize}
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateStrategy.mode`
// +kubebuilder:printcolumn:name="Workloads",type=integer,JSONPath=`.status.workloads.discovered`
// +kubebuilder:printcolumn:name="Recs",type=integer,JSONPath=`.status.workloads.withRecommendations`
// +kubebuilder:printcolumn:name="Resized",type=integer,JSONPath=`.status.workloads.resized`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="CPU Saved",type=string,JSONPath=`.status.savings.cpuRequestReduction`,priority=1
// +kubebuilder:printcolumn:name="Mem Saved",type=string,JSONPath=`.status.savings.memoryRequestReduction`,priority=1

// RightSizePolicy is the Schema for the rightsizepolicies API.
type RightSizePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RightSizePolicySpec   `json:"spec,omitempty"`
	Status RightSizePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RightSizePolicyList contains a list of RightSizePolicy.
type RightSizePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RightSizePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RightSizePolicy{}, &RightSizePolicyList{})
}
