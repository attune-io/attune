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
	resourcehelper "k8s.io/component-helpers/resource"
)

// PodRequestTotals returns the pod's effective CPU (millicores) and memory
// (bytes) requests, as the scheduler sizes the pod from its spec: the larger
// of the running containers plus native sidecars and each regular init
// container plus the sidecars declared before it, pod-level
// spec.resources.requests in place of that sum when set, plus spec.overhead.
// There is no phase check, so a running pod's finished init container still
// counts, as it does for the scheduler and for kubelet resize admission.
//
// Known limit: kubelet sizes a pod with a pending resize from its allocated
// (status) resources, not its spec. This reads the spec only, so a pod in
// the middle of a decrease is under-counted and a Deferred increase is
// over-counted until kubelet catches up.
func PodRequestTotals(pod *corev1.Pod) (cpuMilli, memBytes int64) {
	if pod == nil {
		return 0, 0
	}
	reqs := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
	return reqs.Cpu().MilliValue(), reqs.Memory().Value()
}

// PlannedPod returns a copy of pod as a resize of container to target would
// leave it, the shape PodResizer writes: target merged into the container
// (MergeResources, so a resource the target omits keeps its value) and, when
// inPlacePodLevel is set and the pod has spec.resources, the envelope raised
// with RaiseToCover. The live pod is not modified.
func PlannedPod(pod *corev1.Pod, container string, target corev1.ResourceRequirements, inPlacePodLevel bool) *corev1.Pod {
	if pod == nil {
		return nil
	}
	planned := applyPlannedContainer(pod, container, target)
	if inPlacePodLevel && planned.Spec.Resources != nil {
		if raised := RaiseToCover(planned); raised != nil {
			planned.Spec.Resources = raised
		}
	}
	return planned
}
