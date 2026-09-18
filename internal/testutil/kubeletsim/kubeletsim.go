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

// Package kubeletsim fakes the kubelet side of Pod /resize for unit tests.
// client-go's fake UpdateResize only stores spec; this reactor also writes
// status.containerStatuses resources and PodResizePending / InProgress.
package kubeletsim

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Outcome is the scripted kubelet reaction to UpdateResize.
type Outcome string

const (
	Accepted   Outcome = "accepted"
	Deferred   Outcome = "deferred"
	Infeasible Outcome = "infeasible"
	InProgress Outcome = "inprogress"
)

// Options control how Install mutates the pod after a resize write.
type Options struct {
	Outcome Outcome
	// Now stamps LastTransitionTime. Nil uses time.Now.
	Now func() time.Time
	// InProgressAge is subtracted from Now when Outcome is InProgress so
	// callers can make the condition look older than the one-hour timeout.
	InProgressAge time.Duration
}

// Install registers a resize-subresource reactor on cs.
func Install(cs *fake.Clientset, opts Options) {
	if opts.Outcome == "" {
		opts.Outcome = Accepted
	}
	cs.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		pod := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod).DeepCopy()
		applyOutcome(pod, opts)
		err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace)
		return true, pod, err
	})
}

func applyOutcome(pod *corev1.Pod, opts Options) {
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	switch opts.Outcome {
	case Deferred:
		setResizeCondition(pod, corev1.PodResizePending, "Deferred", now)
	case Infeasible:
		setResizeCondition(pod, corev1.PodResizePending, "Infeasible", now)
	case InProgress:
		setResizeCondition(pod, corev1.PodResizeInProgress, "", now.Add(-opts.InProgressAge))
	default:
		copySpecResourcesToStatus(pod)
		clearResizeConditions(pod)
	}
}

func copySpecResourcesToStatus(pod *corev1.Pod) {
	apply := func(containers []corev1.Container, statuses *[]corev1.ContainerStatus) {
		for i := range containers {
			c := containers[i]
			idx := -1
			for j := range *statuses {
				if (*statuses)[j].Name == c.Name {
					idx = j
					break
				}
			}
			res := c.Resources.DeepCopy()
			alloc := corev1.ResourceList{}
			for k, v := range c.Resources.Requests {
				alloc[k] = v.DeepCopy()
			}
			if idx >= 0 {
				(*statuses)[idx].Resources = res
				(*statuses)[idx].AllocatedResources = alloc
				continue
			}
			*statuses = append(*statuses, corev1.ContainerStatus{
				Name:               c.Name,
				Resources:          res,
				AllocatedResources: alloc,
			})
		}
	}
	apply(pod.Spec.Containers, &pod.Status.ContainerStatuses)
	apply(pod.Spec.InitContainers, &pod.Status.InitContainerStatuses)
}

func setResizeCondition(pod *corev1.Pod, condType corev1.PodConditionType, reason string, at time.Time) {
	clearResizeConditions(pod)
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               condType,
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(at),
	})
}

func clearResizeConditions(pod *corev1.Pod) {
	out := pod.Status.Conditions[:0]
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodResizePending || c.Type == corev1.PodResizeInProgress {
			continue
		}
		out = append(out, c)
	}
	pod.Status.Conditions = out
}
