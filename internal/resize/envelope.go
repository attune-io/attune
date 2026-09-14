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

// ReasonEnvelopeConstraint is the resize history reason when a container
// increase is skipped because of pod-level spec.resources.
const ReasonEnvelopeConstraint = "envelope_constraint"

// EnvelopeSkipMessage is the ResizeSkipped event detail for an envelope skip.
const EnvelopeSkipMessage = "pod-level resource envelope would be exceeded"

// EnvelopeInput is the live pod, planned container target, and cluster/policy
// flags used to decide raise vs skip.
type EnvelopeInput struct {
	Pod             *corev1.Pod
	Container       string
	Target          corev1.ResourceRequirements
	InPlace         bool
	RequestsOnlyCPU bool
	RequestsOnlyMem bool
}

// EnvelopeDecision is the raise-or-skip result. Raised is a copy of the
// existing envelope (never a newly invented spec.resources).
type EnvelopeDecision struct {
	Skip   bool
	Reason string
	Raised *corev1.ResourceRequirements
}

// EvaluateEnvelope decides whether a planned container resize must be skipped
// or whether an existing pod-level envelope should be raised to cover it.
// A nil pod.Spec.Resources is left untouched.
func EvaluateEnvelope(in EnvelopeInput) EnvelopeDecision {
	if in.Pod == nil || in.Pod.Spec.Resources == nil {
		return EnvelopeDecision{}
	}
	planned := applyPlannedContainer(in.Pod, in.Container, in.Target)
	raised := RaiseToCover(planned)
	if requestsOnlyBurstableWouldLiftLimit(in, raised) {
		return EnvelopeDecision{Skip: true, Reason: ReasonEnvelopeConstraint}
	}
	if !in.InPlace && increaseExceedsCurrentEnvelope(in.Pod, planned, in.Container, in.Target) {
		return EnvelopeDecision{Skip: true, Reason: ReasonEnvelopeConstraint}
	}
	return EnvelopeDecision{Raised: raised}
}

// RaiseToCover returns a copy of pod.Spec.Resources whose requests cover the
// sum of container requests and whose existing limits cover the max of the
// current limit, the needed request, and the max single container limit.
// It never shrinks the envelope and never invents a missing envelope or limit.
func RaiseToCover(pod *corev1.Pod) *corev1.ResourceRequirements {
	if pod == nil || pod.Spec.Resources == nil {
		return nil
	}
	raised := pod.Spec.Resources.DeepCopy()
	if raised == nil {
		return nil
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		sumReq := sumContainerRequests(pod, res)
		needed := sumReq
		if cur, ok := envelopeQuantity(pod.Spec.Resources.Requests, res); ok && cur.Cmp(needed) > 0 {
			needed = cur.DeepCopy()
		}
		_, hadReq := envelopeQuantity(pod.Spec.Resources.Requests, res)
		if hadReq || !sumReq.IsZero() {
			if raised.Requests == nil {
				raised.Requests = corev1.ResourceList{}
			}
			raised.Requests[res] = needed
		}
		curLim, hasLim := envelopeQuantity(pod.Spec.Resources.Limits, res)
		if !hasLim {
			continue
		}
		if raised.Limits == nil {
			raised.Limits = corev1.ResourceList{}
		}
		newLim := qtyMax(curLim, needed)
		if maxCL, ok := maxContainerLimit(pod, res); ok {
			newLim = qtyMax(newLim, maxCL)
		}
		if req, ok := envelopeQuantity(raised.Requests, res); ok && req.Cmp(newLim) > 0 {
			newLim = req.DeepCopy()
		}
		raised.Limits[res] = newLim
	}
	return raised
}

func requestsOnlyBurstableWouldLiftLimit(in EnvelopeInput, raised *corev1.ResourceRequirements) bool {
	if in.Pod.Status.QOSClass != corev1.PodQOSBurstable || raised == nil {
		return false
	}
	env := in.Pod.Spec.Resources
	if env == nil {
		return false
	}
	return wouldLiftLimit(env, raised, corev1.ResourceCPU, in.RequestsOnlyCPU) ||
		wouldLiftLimit(env, raised, corev1.ResourceMemory, in.RequestsOnlyMem)
}

func wouldLiftLimit(current, raised *corev1.ResourceRequirements, res corev1.ResourceName, requestsOnly bool) bool {
	if !requestsOnly {
		return false
	}
	curLim, has := envelopeQuantity(current.Limits, res)
	if !has {
		return false
	}
	newLim, ok := envelopeQuantity(raised.Limits, res)
	if !ok {
		return false
	}
	return newLim.Cmp(curLim) > 0
}

func increaseExceedsCurrentEnvelope(pod, planned *corev1.Pod, container string, target corev1.ResourceRequirements) bool {
	env := pod.Spec.Resources
	if env == nil {
		return false
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if !resourceIncreases(pod, container, target, res) {
			continue
		}
		plannedReq := containerRequest(planned, container, res)
		sumReq := sumContainerRequests(planned, res)
		if envReq, ok := envelopeQuantity(env.Requests, res); ok {
			if plannedReq.Cmp(envReq) > 0 || sumReq.Cmp(envReq) > 0 {
				return true
			}
		}
		if envLim, ok := envelopeQuantity(env.Limits, res); ok {
			if plannedReq.Cmp(envLim) > 0 || sumReq.Cmp(envLim) > 0 {
				return true
			}
			if plannedLim, hasLim := containerLimit(planned, container, res); hasLim && plannedLim.Cmp(envLim) > 0 {
				return true
			}
		}
	}
	return false
}

func resourceIncreases(pod *corev1.Pod, container string, target corev1.ResourceRequirements, res corev1.ResourceName) bool {
	current := containerResources(pod, container)
	if tReq, ok := envelopeQuantity(target.Requests, res); ok {
		if cReq, cok := envelopeQuantity(current.Requests, res); !cok || tReq.Cmp(cReq) > 0 {
			return true
		}
	}
	if tLim, ok := envelopeQuantity(target.Limits, res); ok {
		if cLim, cok := envelopeQuantity(current.Limits, res); !cok || tLim.Cmp(cLim) > 0 {
			return true
		}
	}
	return false
}

func applyPlannedContainer(pod *corev1.Pod, container string, target corev1.ResourceRequirements) *corev1.Pod {
	planned := pod.DeepCopy()
	idx, isInit := findContainer(planned, container)
	if idx < 0 {
		return planned
	}
	if isInit {
		planned.Spec.InitContainers[idx].Resources = mergeResources(planned.Spec.InitContainers[idx].Resources, target)
	} else {
		planned.Spec.Containers[idx].Resources = mergeResources(planned.Spec.Containers[idx].Resources, target)
	}
	return planned
}

func sumContainerRequests(pod *corev1.Pod, res corev1.ResourceName) resource.Quantity {
	sum := zeroQuantity(res)
	eachContainerResources(pod, func(rr corev1.ResourceRequirements) {
		if q, ok := envelopeQuantity(rr.Requests, res); ok {
			sum.Add(q)
		}
	})
	return sum
}

func maxContainerLimit(pod *corev1.Pod, res corev1.ResourceName) (resource.Quantity, bool) {
	var max resource.Quantity
	found := false
	eachContainerResources(pod, func(rr corev1.ResourceRequirements) {
		q, ok := envelopeQuantity(rr.Limits, res)
		if !ok {
			return
		}
		if !found || q.Cmp(max) > 0 {
			max = q.DeepCopy()
			found = true
		}
	})
	return max, found
}

func eachContainerResources(pod *corev1.Pod, fn func(corev1.ResourceRequirements)) {
	if pod == nil {
		return
	}
	for i := range pod.Spec.InitContainers {
		fn(pod.Spec.InitContainers[i].Resources)
	}
	for i := range pod.Spec.Containers {
		fn(pod.Spec.Containers[i].Resources)
	}
}

func containerResources(pod *corev1.Pod, name string) corev1.ResourceRequirements {
	if pod == nil {
		return corev1.ResourceRequirements{}
	}
	idx, isInit := findContainer(pod, name)
	if idx < 0 {
		return corev1.ResourceRequirements{}
	}
	if isInit {
		return pod.Spec.InitContainers[idx].Resources
	}
	return pod.Spec.Containers[idx].Resources
}

func containerRequest(pod *corev1.Pod, name string, res corev1.ResourceName) resource.Quantity {
	q, _ := envelopeQuantity(containerResources(pod, name).Requests, res)
	return q
}

func containerLimit(pod *corev1.Pod, name string, res corev1.ResourceName) (resource.Quantity, bool) {
	return envelopeQuantity(containerResources(pod, name).Limits, res)
}

func envelopeQuantity(list corev1.ResourceList, res corev1.ResourceName) (resource.Quantity, bool) {
	if list == nil {
		return resource.Quantity{}, false
	}
	q, ok := list[res]
	return q, ok
}

func qtyMax(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) >= 0 {
		return a.DeepCopy()
	}
	return b.DeepCopy()
}

func zeroQuantity(res corev1.ResourceName) resource.Quantity {
	if res == corev1.ResourceCPU {
		return resource.Quantity{Format: resource.DecimalSI}
	}
	return resource.Quantity{Format: resource.BinarySI}
}

func envelopeIsGuaranteed(env *corev1.ResourceRequirements) bool {
	if env == nil {
		return false
	}
	cpuReq, hasCPUReq := envelopeQuantity(env.Requests, corev1.ResourceCPU)
	cpuLim, hasCPULim := envelopeQuantity(env.Limits, corev1.ResourceCPU)
	memReq, hasMemReq := envelopeQuantity(env.Requests, corev1.ResourceMemory)
	memLim, hasMemLim := envelopeQuantity(env.Limits, corev1.ResourceMemory)
	if !hasCPUReq || !hasCPULim || !hasMemReq || !hasMemLim {
		return false
	}
	return cpuReq.Equal(cpuLim) && memReq.Equal(memLim)
}
