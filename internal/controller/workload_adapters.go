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

package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadAdapter provides kind-specific behavior for a concrete workload instance.
// This eliminates scattered type-switches across workload helper functions.
type WorkloadAdapter interface {
	// Object returns the underlying Kubernetes object.
	Object() client.Object

	// PodSelectorLabels returns the labels used to select pods owned by this workload.
	PodSelectorLabels() map[string]string

	// PodSpec returns the pod template spec from the workload.
	PodSpec() *corev1.PodSpec

	// IsRollingOut returns true if the workload is mid-rollout.
	IsRollingOut() bool

	// PodNameRegexSuffix returns the PromQL regex suffix that matches pods for this kind.
	PodNameRegexSuffix() string

	// IsBatch returns true for Job and CronJob workloads.
	IsBatch() bool
}

// workloadKind holds factory functions for creating and listing workload objects by kind string.
type workloadKind struct {
	newObject func() client.Object
	newList   func() client.ObjectList
	extract   func(client.ObjectList) []client.Object
}

// extractItems converts a typed item slice into a []client.Object slice.
// This eliminates the boilerplate extract closure in each workloadKinds entry.
func extractItems[T any, PT interface {
	*T
	client.Object
}](items []T) []client.Object {
	out := make([]client.Object, len(items))
	for i := range items {
		out[i] = PT(&items[i])
	}
	return out
}

// workloadKinds maps kind strings to their factory functions.
var workloadKinds = map[string]workloadKind{
	"Deployment": {
		newObject: func() client.Object { return &appsv1.Deployment{} },
		newList:   func() client.ObjectList { return &appsv1.DeploymentList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[appsv1.Deployment](list.(*appsv1.DeploymentList).Items)
		},
	},
	"StatefulSet": {
		newObject: func() client.Object { return &appsv1.StatefulSet{} },
		newList:   func() client.ObjectList { return &appsv1.StatefulSetList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[appsv1.StatefulSet](list.(*appsv1.StatefulSetList).Items)
		},
	},
	"DaemonSet": {
		newObject: func() client.Object { return &appsv1.DaemonSet{} },
		newList:   func() client.ObjectList { return &appsv1.DaemonSetList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[appsv1.DaemonSet](list.(*appsv1.DaemonSetList).Items)
		},
	},
	"CronJob": {
		newObject: func() client.Object { return &batchv1.CronJob{} },
		newList:   func() client.ObjectList { return &batchv1.CronJobList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[batchv1.CronJob](list.(*batchv1.CronJobList).Items)
		},
	},
	"Job": {
		newObject: func() client.Object { return &batchv1.Job{} },
		newList:   func() client.ObjectList { return &batchv1.JobList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[batchv1.Job](list.(*batchv1.JobList).Items)
		},
	},
	"ReplicaSet": {
		newObject: func() client.Object { return &appsv1.ReplicaSet{} },
		newList:   func() client.ObjectList { return &appsv1.ReplicaSetList{} },
		extract: func(list client.ObjectList) []client.Object {
			return extractItems[appsv1.ReplicaSet](list.(*appsv1.ReplicaSetList).Items)
		},
	},
}

// newWorkloadAdapter wraps a client.Object in the appropriate WorkloadAdapter.
// Returns nil for unsupported types.
func newWorkloadAdapter(obj client.Object) WorkloadAdapter {
	switch w := obj.(type) {
	case *appsv1.Deployment:
		return &deploymentAdapter{Deployment: w}
	case *appsv1.StatefulSet:
		return &statefulSetAdapter{StatefulSet: w}
	case *appsv1.DaemonSet:
		return &daemonSetAdapter{DaemonSet: w}
	case *batchv1.CronJob:
		return &cronJobAdapter{CronJob: w}
	case *batchv1.Job:
		return &jobAdapter{Job: w}
	case *appsv1.ReplicaSet:
		return &replicaSetAdapter{ReplicaSet: w}
	default:
		return nil
	}
}

// --- Deployment ---

type deploymentAdapter struct{ *appsv1.Deployment }

func (a *deploymentAdapter) Object() client.Object { return a.Deployment }

func (a *deploymentAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.Selector != nil {
		return a.Spec.Selector.MatchLabels
	}
	return nil
}

func (a *deploymentAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.Template.Spec
}

func (a *deploymentAdapter) IsRollingOut() bool {
	if a.Spec.Replicas != nil && a.Status.UpdatedReplicas < *a.Spec.Replicas {
		return true
	}
	if a.Spec.Replicas != nil && a.Status.AvailableReplicas < *a.Spec.Replicas {
		return true
	}
	return false
}

func (a *deploymentAdapter) PodNameRegexSuffix() string { return "-[a-z0-9]+-[a-z0-9]{5}" }

func (a *deploymentAdapter) IsBatch() bool { return false }

// --- StatefulSet ---

type statefulSetAdapter struct{ *appsv1.StatefulSet }

func (a *statefulSetAdapter) Object() client.Object { return a.StatefulSet }

func (a *statefulSetAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.Selector != nil {
		return a.Spec.Selector.MatchLabels
	}
	return nil
}

func (a *statefulSetAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.Template.Spec
}

func (a *statefulSetAdapter) IsRollingOut() bool {
	if a.Spec.Replicas != nil && a.Status.UpdatedReplicas < *a.Spec.Replicas {
		return true
	}
	return false
}

func (a *statefulSetAdapter) PodNameRegexSuffix() string { return "-[0-9]+" }

func (a *statefulSetAdapter) IsBatch() bool { return false }

// --- DaemonSet ---

type daemonSetAdapter struct{ *appsv1.DaemonSet }

func (a *daemonSetAdapter) Object() client.Object { return a.DaemonSet }

func (a *daemonSetAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.Selector != nil {
		return a.Spec.Selector.MatchLabels
	}
	return nil
}

func (a *daemonSetAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.Template.Spec
}

func (a *daemonSetAdapter) IsRollingOut() bool {
	return a.Status.UpdatedNumberScheduled < a.Status.DesiredNumberScheduled
}

func (a *daemonSetAdapter) PodNameRegexSuffix() string { return "-[a-z0-9]{5}" }

func (a *daemonSetAdapter) IsBatch() bool { return false }

// --- CronJob ---

type cronJobAdapter struct{ *batchv1.CronJob }

func (a *cronJobAdapter) Object() client.Object { return a.CronJob }

func (a *cronJobAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.JobTemplate.Spec.Selector != nil {
		return a.Spec.JobTemplate.Spec.Selector.MatchLabels
	}
	// Fall back to pod template labels for CronJobs without explicit selector.
	return a.Spec.JobTemplate.Spec.Template.Labels
}

func (a *cronJobAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.JobTemplate.Spec.Template.Spec
}

func (a *cronJobAdapter) IsRollingOut() bool { return false }

func (a *cronJobAdapter) PodNameRegexSuffix() string {
	if a.Spec.JobTemplate.Spec.CompletionMode != nil && *a.Spec.JobTemplate.Spec.CompletionMode == batchv1.IndexedCompletion {
		return "-[0-9]{10}-[0-9]+-[a-z0-9]{5}"
	}
	return "-[0-9]{10}-[a-z0-9]{5}"
}

func (a *cronJobAdapter) IsBatch() bool { return true }

// --- Job ---

type jobAdapter struct{ *batchv1.Job }

func (a *jobAdapter) Object() client.Object { return a.Job }

func (a *jobAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.Selector != nil {
		return a.Spec.Selector.MatchLabels
	}
	return a.Spec.Template.Labels
}

func (a *jobAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.Template.Spec
}

func (a *jobAdapter) IsRollingOut() bool { return false }

func (a *jobAdapter) PodNameRegexSuffix() string {
	if a.Spec.CompletionMode != nil && *a.Spec.CompletionMode == batchv1.IndexedCompletion {
		return "-[0-9]+-[a-z0-9]{5}"
	}
	return "-[a-z0-9]{5}"
}

func (a *jobAdapter) IsBatch() bool { return true }

// --- ReplicaSet ---

type replicaSetAdapter struct{ *appsv1.ReplicaSet }

func (a *replicaSetAdapter) Object() client.Object { return a.ReplicaSet }

func (a *replicaSetAdapter) PodSelectorLabels() map[string]string {
	if a.Spec.Selector != nil {
		return a.Spec.Selector.MatchLabels
	}
	return nil
}

func (a *replicaSetAdapter) PodSpec() *corev1.PodSpec {
	return &a.Spec.Template.Spec
}

func (a *replicaSetAdapter) IsRollingOut() bool {
	if a.Spec.Replicas != nil && a.Status.ReadyReplicas < *a.Spec.Replicas {
		return true
	}
	return false
}

func (a *replicaSetAdapter) PodNameRegexSuffix() string { return "-[a-z0-9]{5}" }

func (a *replicaSetAdapter) IsBatch() bool { return false }
