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

package resize

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// QoSPlan carries the same knobs ResizePod uses when it builds the
// object sent to UpdateResize. Zero values match a cluster that still
// clamps NotRequired memory-limit decreases and does not write pod-level
// resources.
type QoSPlan struct {
	AllowInPlaceMemoryLimitDecrease bool
	InPlacePodLevelResources        bool
}

// PreservesQoS reports whether the spec ResizePod would send stays in
// the pod's current QoS class. A class change is skipped before
// UpdateResize so InPlaceOrRecreate does not evict.
func PreservesQoS(pod *corev1.Pod, container string, target corev1.ResourceRequirements, opts QoSPlan) bool {
	if pod == nil {
		return true
	}
	from, to := QoSClasses(pod, container, target, opts)
	return from == to
}

// QoSClasses returns the current class and the class of the spec
// ResizePod would send. An empty status class is inferred from the live
// spec, before the planned merge.
func QoSClasses(pod *corev1.Pod, container string, target corev1.ResourceRequirements, opts QoSPlan) (from, to corev1.PodQOSClass) {
	if pod == nil {
		return "", ""
	}
	from = pod.Status.QOSClass
	if from == "" {
		from = podQOS(pod, opts.InPlacePodLevelResources)
	}
	to = podQOS(plannedPod(pod, container, target, opts), opts.InPlacePodLevelResources)
	return from, to
}

func plannedPod(pod *corev1.Pod, container string, target corev1.ResourceRequirements, opts QoSPlan) *corev1.Pod {
	planned := pod.DeepCopy()
	idx, isInit := findContainer(planned, container)
	if idx < 0 {
		return planned
	}
	clamped := ClampMemoryLimitForPolicy(planned, container, target, opts.AllowInPlaceMemoryLimitDecrease)
	if isInit {
		live := planned.Spec.InitContainers[idx].Resources
		planned.Spec.InitContainers[idx].Resources = MergeResources(live, clamped)
	} else {
		live := planned.Spec.Containers[idx].Resources
		planned.Spec.Containers[idx].Resources = MergeResources(live, clamped)
	}
	if opts.InPlacePodLevelResources && planned.Spec.Resources != nil {
		if raised := RaiseToCover(planned); raised != nil {
			planned.Spec.Resources = raised
		}
	}
	return planned
}

// podQOS classifies CPU and memory only. Zero quantities are unset.
// Init containers are included, including completed one-shot inits,
// because Kubernetes counts them in the QoS class.
func podQOS(pod *corev1.Pod, usePodLevel bool) corev1.PodQOSClass {
	if pod == nil {
		return corev1.PodQOSBestEffort
	}
	if usePodLevel && pod.Spec.Resources != nil {
		return requirementsQOS(*pod.Spec.Resources)
	}
	containers := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	containers = append(containers, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	if len(containers) == 0 {
		return corev1.PodQOSBestEffort
	}
	anyPositive := false
	allGuaranteed := true
	for _, c := range containers {
		guaranteed, positive := containerQoS(c.Resources)
		if positive {
			anyPositive = true
		}
		if !guaranteed {
			allGuaranteed = false
		}
	}
	if !anyPositive {
		return corev1.PodQOSBestEffort
	}
	if allGuaranteed {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

func requirementsQOS(rr corev1.ResourceRequirements) corev1.PodQOSClass {
	guaranteed, positive := containerQoS(rr)
	if !positive {
		return corev1.PodQOSBestEffort
	}
	if guaranteed {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

// containerQoS reports whether this container is a Guaranteed pair and
// whether it has any positive CPU or memory request or limit.
func containerQoS(rr corev1.ResourceRequirements) (guaranteed, anyPositive bool) {
	cpuReq, hasCPUReq := positiveQuantity(rr.Requests, corev1.ResourceCPU)
	cpuLim, hasCPULim := positiveQuantity(rr.Limits, corev1.ResourceCPU)
	memReq, hasMemReq := positiveQuantity(rr.Requests, corev1.ResourceMemory)
	memLim, hasMemLim := positiveQuantity(rr.Limits, corev1.ResourceMemory)
	anyPositive = hasCPUReq || hasCPULim || hasMemReq || hasMemLim
	if !hasCPUReq || !hasCPULim || !hasMemReq || !hasMemLim {
		return false, anyPositive
	}
	return cpuReq.Cmp(cpuLim) == 0 && memReq.Cmp(memLim) == 0, anyPositive
}

func positiveQuantity(list corev1.ResourceList, name corev1.ResourceName) (resource.Quantity, bool) {
	if len(list) == 0 {
		return resource.Quantity{}, false
	}
	q, ok := list[name]
	if !ok || q.Cmp(resource.Quantity{}) <= 0 {
		return resource.Quantity{}, false
	}
	return q, true
}
