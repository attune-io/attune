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

package transform

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// LabelTracked is the pod label Attune sets on resized pods for safety
// observation. Always retained fully in the informer cache.
const LabelTracked = "attune.io/tracked"

// PodCacheFilter is a Transform for Pods that keeps full (stripped) objects
// only when they match active policy selectors or a static selector, and
// otherwise stores a minimal stub. This reduces informer memory without
// requiring a single static label selector that covers every policy.
//
// Note: Kubernetes watch still delivers all pod events in the watched
// namespaces; this filter only shrinks what is retained in the cache.
type PodCacheFilter struct {
	mu sync.RWMutex
	// static is optional operator-wide label selector (--pod-label-selector).
	static labels.Selector
	// dynamic is the union of MatchLabels selectors from active policies'
	// target workloads (refreshed by the controller).
	dynamic []labels.Selector
	// enabled when true applies filtering; when false, only StripPodFields.
	enabled bool
}

// NewPodCacheFilter builds a filter. static may be nil. enabled starts false
// until the first successful selector refresh (keeps all pods until then).
func NewPodCacheFilter(static labels.Selector) *PodCacheFilter {
	return &PodCacheFilter{static: static}
}

// SetEnabled turns dynamic filtering on or off.
func (f *PodCacheFilter) SetEnabled(on bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.enabled = on
	f.mu.Unlock()
}

// UpdateDynamic replaces the policy-derived selectors (call after listing
// policies and resolving workload pod selectors).
func (f *PodCacheFilter) UpdateDynamic(sels []labels.Selector) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.dynamic = sels
	if len(sels) > 0 || f.static != nil {
		f.enabled = true
	}
	// Keep enabled if already on with static selector even when dynamic is empty.
	if f.static != nil {
		f.enabled = true
	}
	f.mu.Unlock()
}

// Transform implements cache.TransformFunc for Pods.
func (f *PodCacheFilter) Transform(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	if f == nil {
		return StripPodFields(pod)
	}
	if f.shouldKeep(pod) {
		return StripPodFields(pod)
	}
	return minimalPodStub(pod), nil
}

func (f *PodCacheFilter) shouldKeep(pod *corev1.Pod) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.enabled {
		return true
	}
	// Always keep safety-tracked pods (full fields needed for observation).
	if pod.Labels != nil && pod.Labels[LabelTracked] == "true" {
		return true
	}
	ls := labels.Set(pod.Labels)
	if f.static != nil && f.static.Matches(ls) {
		return true
	}
	for _, sel := range f.dynamic {
		if sel != nil && sel.Matches(ls) {
			return true
		}
	}
	// Static-only mode: if static is set and no dynamic yet, drop non-matches.
	// Dynamic empty with static nil and enabled: keep all (safety).
	if f.static != nil && len(f.dynamic) == 0 {
		return false
	}
	if len(f.dynamic) == 0 {
		return true
	}
	return false
}

// minimalPodStub keeps identity and labels so MatchingLabels Lists can still
// find the object, but drops heavy Spec/Status. Callers that need resources
// must only match kept pods (active policy selectors).
func minimalPodStub(pod *corev1.Pod) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
			Labels:          pod.Labels,
			Annotations:     nil,
			OwnerReferences: pod.OwnerReferences,
		},
	}
}

// SelectorFromMap builds an equality-based labels.Selector from match labels.
func SelectorFromMap(m map[string]string) labels.Selector {
	if len(m) == 0 {
		return nil
	}
	return labels.SelectorFromSet(labels.Set(m))
}
