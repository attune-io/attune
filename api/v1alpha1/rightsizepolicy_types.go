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
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
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
	// before generating recommendations.
	// +kubebuilder:default=168
	MinimumDataPoints int32 `json:"minimumDataPoints,omitempty"`
}

// PrometheusConfig configures a Prometheus metrics source.
type PrometheusConfig struct {
	// Address is the URL of the Prometheus server.
	Address string `json:"address"`
}

// ResourceConfig defines resource recommendation parameters.
type ResourceConfig struct {
	// Percentile is the usage percentile to target for recommendations.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=99
	// +optional
	Percentile int32 `json:"percentile,omitempty"`

	// SafetyMargin is a multiplier applied to the recommended value.
	// Expressed as a resource.Quantity-compatible string (e.g. "1.2").
	// +optional
	SafetyMargin string `json:"safetyMargin,omitempty"`

	// Bounds defines the minimum and maximum allowed resource values.
	// +optional
	Bounds *ResourceBounds `json:"bounds,omitempty"`

	// ControlledValues specifies which resource values to manage.
	// +kubebuilder:validation:Enum=RequestsOnly;RequestsAndLimits
	// +optional
	ControlledValues *string `json:"controlledValues,omitempty"`

	// AllowDecrease controls whether the resource value can be decreased.
	// Only applicable to memory resources.
	// +optional
	AllowDecrease *bool `json:"allowDecrease,omitempty"`
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
	// Mode determines the update behavior.
	// +kubebuilder:validation:Enum=Observe;Recommend;OneShot;Canary;Auto
	Mode string `json:"mode"`

	// Canary configures canary rollout behavior when Mode is Canary.
	// +optional
	Canary *CanaryConfig `json:"canary,omitempty"`

	// MaxCPUChangePercent is the maximum allowed CPU change percentage per operation.
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxCPUChangePercent int32 `json:"maxCpuChangePercent,omitempty"`

	// MaxMemoryChangePercent is the maximum allowed memory change percentage per operation.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxMemoryChangePercent int32 `json:"maxMemoryChangePercent,omitempty"`

	// Cooldown is the minimum time between successive resize operations.
	// Defaults to 1h if not specified.
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`

	// AutoRevert automatically reverts changes if degradation is detected.
	// +kubebuilder:default=true
	AutoRevert bool `json:"autoRevert,omitempty"`
}

// CanaryConfig defines canary rollout parameters.
type CanaryConfig struct {
	// Percentage is the percentage of pods to resize first.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percentage int32 `json:"percentage"`

	// ObservationPeriod is how long to observe canary pods before proceeding.
	ObservationPeriod metav1.Duration `json:"observationPeriod"`
}

// RightSizePolicyStatus defines the observed state of RightSizePolicy.
type RightSizePolicyStatus struct {
	// Conditions represent the latest available observations of the policy's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Workloads summarizes workload discovery and resize counts.
	// +optional
	Workloads WorkloadStatus `json:"workloads,omitempty"`

	// Recommendations contains per-workload resource recommendations.
	// +optional
	Recommendations []WorkloadRecommendation `json:"recommendations,omitempty"`

	// Savings summarizes estimated resource savings.
	// +optional
	Savings SavingsStatus `json:"savings,omitempty"`

	// ResizeHistory records past resize operations.
	// +optional
	ResizeHistory []ResizeHistoryEntry `json:"resizeHistory,omitempty"`
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

	// Confidence is the confidence score of the recommendation (0-1).
	Confidence float64 `json:"confidence"`

	// DataPoints is the number of data points used to generate the recommendation.
	DataPoints int32 `json:"dataPoints"`

	// LastUpdated is the timestamp of the last recommendation update.
	LastUpdated metav1.Time `json:"lastUpdated"`
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
	// +kubebuilder:validation:Enum=cpu;memory
	Resource string `json:"resource"`

	// From is the previous resource value.
	From string `json:"from"`

	// To is the new resource value.
	To string `json:"to"`

	// Method is the resize method used.
	// +kubebuilder:validation:Enum=InPlace;Recreate
	Method string `json:"method"`

	// Result is the outcome of the resize operation.
	// +kubebuilder:validation:Enum=Success;Failed;Reverted
	Result string `json:"result"`
}

// SavingsStatus summarizes estimated resource savings.
type SavingsStatus struct {
	// CPURequestReduction is the total CPU request reduction.
	CPURequestReduction string `json:"cpuRequestReduction,omitempty"`

	// MemoryRequestReduction is the total memory request reduction.
	MemoryRequestReduction string `json:"memoryRequestReduction,omitempty"`

	// EstimatedMonthlySavings is the estimated monthly cost savings based on
	// configured or default pricing (e.g. "$12.50").
	EstimatedMonthlySavings string `json:"estimatedMonthlySavings,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rsp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateStrategy.mode`
// +kubebuilder:printcolumn:name="Workloads",type=integer,JSONPath=`.status.workloads.discovered`
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
