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

// RightSizeDefaultsSpec defines cluster-scoped default values for RightSizePolicy resources.
type RightSizeDefaultsSpec struct {
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
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rsd
// +kubebuilder:printcolumn:name="Prometheus",type=string,JSONPath=`.spec.metricsSource.prometheus.address`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateStrategy.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RightSizeDefaults is the Schema for the rightsizedefaults API.
// It defines cluster-scoped default values for RightSizePolicy resources.
type RightSizeDefaults struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RightSizeDefaultsSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// RightSizeDefaultsList contains a list of RightSizeDefaults.
type RightSizeDefaultsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RightSizeDefaults `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RightSizeDefaults{}, &RightSizeDefaultsList{})
}
