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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttuneDefaultsSpec defines cluster-scoped default values for AttunePolicy resources.
type AttuneDefaultsSpec struct {
	// MetricsSource configures default metrics source settings.
	// +optional
	MetricsSource *MetricsSource `json:"metricsSource,omitempty"`

	// CPU configures default CPU resource recommendation parameters.
	// +optional
	CPU *ResourceConfig `json:"cpu,omitempty"`

	// Memory configures default memory resource recommendation parameters.
	// +optional
	Memory *ResourceConfig `json:"memory,omitempty"`

	// UpdateStrategy configures default update strategy settings.
	// +optional
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty"`

	// CostPricing configures the per-unit pricing used to compute
	// EstimatedMonthlySavings. If omitted, defaults to standard
	// on-demand Linux pricing ($0.031/vCPU-hour, $0.004/GiB-hour).
	// +optional
	CostPricing *CostPricing `json:"costPricing,omitempty"`
}

// CostPricing defines per-unit resource pricing for cost estimation.
type CostPricing struct {
	// CPUPerCoreHour is the cost per vCPU-hour (e.g. "0.031").
	// Defaults to 0.031 if not specified.
	// +optional
	CPUPerCoreHour string `json:"cpuPerCoreHour,omitempty"`

	// MemoryPerGiBHour is the cost per GiB-hour (e.g. "0.004").
	// Defaults to 0.004 if not specified.
	// +optional
	MemoryPerGiBHour string `json:"memoryPerGiBHour,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ad,categories={attune}
// +kubebuilder:printcolumn:name="Prometheus",type=string,JSONPath=`.spec.metricsSource.prometheus.address`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.updateStrategy.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AttuneDefaults is the Schema for the attunedefaults API.
// It defines cluster-scoped default values for AttunePolicy resources.
type AttuneDefaults struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AttuneDefaultsSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AttuneDefaultsList contains a list of AttuneDefaults.
type AttuneDefaultsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AttuneDefaults `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=and,categories={attune}
// +kubebuilder:printcolumn:name="Prometheus",type=string,JSONPath=`.spec.metricsSource.prometheus.address`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.updateStrategy.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AttuneNamespaceDefaults is the Schema for namespace-scoped defaults.
// Values here override cluster-scoped AttuneDefaults but are overridden
// by per-policy values. Precedence: policy > namespace defaults > cluster defaults.
type AttuneNamespaceDefaults struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AttuneDefaultsSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AttuneNamespaceDefaultsList contains a list of AttuneNamespaceDefaults.
type AttuneNamespaceDefaultsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AttuneNamespaceDefaults `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&AttuneDefaults{}, &AttuneDefaultsList{},
		&AttuneNamespaceDefaults{}, &AttuneNamespaceDefaultsList{},
	)
}
