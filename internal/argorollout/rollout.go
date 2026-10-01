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

package argorollout

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the Argo Rollouts API group.
	Group = "argoproj.io"
	// Version is the Argo Rollouts API version Attune understands.
	Version = "v1alpha1"
	// Kind is the Rollout kind string.
	Kind = "Rollout"
	// Resource is the plural resource name used in RBAC.
	Resource = "rollouts"
)

// SchemeGroupVersion is the GVK group/version for Rollout objects.
var SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// WorkloadRef names another workload whose pod template the Rollout uses.
// A non-nil WorkloadRef means Attune must not patch spec.template.
type WorkloadRef struct {
	// Name is the referenced workload name.
	Name string `json:"name,omitempty"`
	// Kind is the referenced workload kind.
	Kind string `json:"kind,omitempty"`
	// APIVersion is the referenced workload apiVersion.
	APIVersion string `json:"apiVersion,omitempty"`
}

// RolloutSpec is the subset of an Argo Rollout spec that Attune reads.
type RolloutSpec struct {
	// Replicas is the desired replica count. Nil counts as 1 for rollout gates.
	Replicas *int32 `json:"replicas,omitempty"`
	// Selector selects pods managed by this Rollout.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Template is the pod template Attune may patch when WorkloadRef is unset.
	Template corev1.PodTemplateSpec `json:"template,omitempty"`
	// WorkloadRef, when set, means the Rollout does not own spec.template.
	WorkloadRef *WorkloadRef `json:"workloadRef,omitempty"`
}

// RolloutStatus is the subset of an Argo Rollout status that Attune reads.
type RolloutStatus struct {
	// Replicas is the observed replica count.
	Replicas int32 `json:"replicas,omitempty"`
	// UpdatedReplicas is the number of pods on the current template.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// ReadyReplicas is the number of ready pods.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Phase is the Rollout phase (Healthy, Progressing, Paused, Degraded, and others).
	Phase string `json:"phase,omitempty"`
	// Abort is true when the Rollout has been aborted.
	Abort bool `json:"abort,omitempty"`
}

// Rollout is a local view of argoproj.io/v1alpha1 Rollout.
// DeepCopy stays hand-written in this file. See doc.go for why this
// package is excluded from CRD generation.
type Rollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutSpec   `json:"spec,omitempty"`
	Status RolloutStatus `json:"status,omitempty"`
}

// RolloutList is a list of Rollout objects.
type RolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Rollout `json:"items"`
}

// DeepCopyInto copies the receiver into out.
func (in *WorkloadRef) DeepCopyInto(out *WorkloadRef) {
	*out = *in
}

// DeepCopy returns a copy of the receiver.
func (in *WorkloadRef) DeepCopy() *WorkloadRef {
	if in == nil {
		return nil
	}
	out := new(WorkloadRef)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *RolloutSpec) DeepCopyInto(out *RolloutSpec) {
	*out = *in
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
	if in.Selector != nil {
		in, out := &in.Selector, &out.Selector
		*out = new(metav1.LabelSelector)
		(*in).DeepCopyInto(*out)
	}
	in.Template.DeepCopyInto(&out.Template)
	if in.WorkloadRef != nil {
		in, out := &in.WorkloadRef, &out.WorkloadRef
		*out = new(WorkloadRef)
		**out = **in
	}
}

// DeepCopy returns a copy of the receiver.
func (in *RolloutSpec) DeepCopy() *RolloutSpec {
	if in == nil {
		return nil
	}
	out := new(RolloutSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *RolloutStatus) DeepCopyInto(out *RolloutStatus) {
	*out = *in
}

// DeepCopy returns a copy of the receiver.
func (in *RolloutStatus) DeepCopy() *RolloutStatus {
	if in == nil {
		return nil
	}
	out := new(RolloutStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *Rollout) DeepCopyInto(out *Rollout) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

// DeepCopy returns a copy of the receiver.
func (in *Rollout) DeepCopy() *Rollout {
	if in == nil {
		return nil
	}
	out := new(Rollout)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a copy of the receiver as a runtime.Object.
func (in *Rollout) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *RolloutList) DeepCopyInto(out *RolloutList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Rollout, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy returns a copy of the receiver.
func (in *RolloutList) DeepCopy() *RolloutList {
	if in == nil {
		return nil
	}
	out := new(RolloutList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a copy of the receiver as a runtime.Object.
func (in *RolloutList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// AddToScheme registers Rollout and RolloutList with the scheme.
// Registration does not require the CRD and does not start an informer.
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &Rollout{}, &RolloutList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
