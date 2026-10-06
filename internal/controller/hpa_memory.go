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
	"encoding/json"
	"strconv"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// hpaContainerMemoryEntry is one ContainerResource baseline inside
// attune.io/original-container-memory. encoding/json sorts map keys.
type hpaContainerMemoryEntry struct {
	Target  string `json:"target"`
	Request string `json:"request"`
}

type hpaClampNote struct {
	message string
}

// hpaMemoryRowsFromHistory keeps the first successful in-place memory row
// per container. bad is true when any such row has an unparseable From or To.
func hpaMemoryRowsFromHistory(history []attunev1alpha1.ResizeHistoryEntry, workload string) (map[string]hpaCPURow, bool) {
	rows := make(map[string]hpaCPURow)
	bad := false
	for _, h := range history {
		if h.Workload != workload || h.Resource != "memory" || !isSuccessfulInPlaceHistory(h) {
			continue
		}
		_, seen := rows[h.Container]
		from, fromErr := resource.ParseQuantity(h.From)
		to, toErr := resource.ParseQuantity(h.To)
		if fromErr != nil || toErr != nil {
			bad = true
			if !seen {
				rows[h.Container] = hpaCPURow{}
			}
			continue
		}
		if seen {
			continue
		}
		rows[h.Container] = hpaCPURow{old: from.MilliValue(), neu: to.MilliValue(), ok: true}
	}
	return rows, bad
}

func hpaMemoryRowsChanged(rows map[string]hpaCPURow) bool {
	for _, row := range rows {
		if row.ok && row.old != row.neu {
			return true
		}
	}
	return false
}

// firstPodWithPositiveMemoryRequest is the fallback when no pod has a
// positive CPU request sum. Init containers are not included.
func firstPodWithPositiveMemoryRequest(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		var total int64
		for _, c := range pods[i].Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				total += q.MilliValue()
			}
		}
		if total > 0 {
			return &pods[i]
		}
	}
	return nil
}

// memoryQty formats a MilliValue() sum as BinarySI so 1Gi stays 1Gi.
func memoryQty(milli int64) resource.Quantity {
	if milli <= 0 {
		return resource.Quantity{}
	}
	return *resource.NewQuantity(milli/1000, resource.BinarySI)
}

func podMemoryMillis(pod *corev1.Pod, rows map[string]hpaCPURow) (oldMilli, newMilli int64, ok bool) {
	if pod == nil {
		return 0, 0, false
	}
	seen := make(map[string]struct{}, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.Containers {
		seen[c.Name] = struct{}{}
		if row, exists := rows[c.Name]; exists && row.ok {
			oldMilli += row.old
			newMilli += row.neu
			continue
		}
		if q, has := c.Resources.Requests[corev1.ResourceMemory]; has {
			v := q.MilliValue()
			oldMilli += v
			newMilli += v
		}
	}
	for _, c := range pod.Spec.InitContainers {
		seen[c.Name] = struct{}{}
		if !nativeSidecar(c) {
			continue
		}
		if row, exists := rows[c.Name]; exists && row.ok {
			oldMilli += row.old
			newMilli += row.neu
			continue
		}
		if q, has := c.Resources.Requests[corev1.ResourceMemory]; has {
			v := q.MilliValue()
			oldMilli += v
			newMilli += v
		}
	}
	for name, row := range rows {
		if _, onPod := seen[name]; onPod || !row.ok {
			continue
		}
		oldMilli += row.old
		newMilli += row.neu
	}
	return oldMilli, newMilli, true
}

func liveContainerMemory(pod *corev1.Pod, name string) (reqMilli, limitMilli int64, found bool) {
	if pod == nil || name == "" {
		return 0, 0, false
	}
	for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range list {
			if c.Name != name {
				continue
			}
			if q, has := c.Resources.Requests[corev1.ResourceMemory]; has {
				reqMilli = q.MilliValue()
			}
			if q, has := c.Resources.Limits[corev1.ResourceMemory]; has {
				limitMilli = q.MilliValue()
			}
			return reqMilli, limitMilli, true
		}
	}
	return 0, 0, false
}

func containerMemoryMillis(pod *corev1.Pod, name string, rows map[string]hpaCPURow) (oldMilli, newMilli, limitMilli int64, ok bool) {
	if row, exists := rows[name]; exists {
		if !row.ok {
			return 0, 0, 0, false
		}
		_, lim, _ := liveContainerMemory(pod, name)
		return row.old, row.neu, lim, true
	}
	req, lim, found := liveContainerMemory(pod, name)
	if !found {
		return 0, 0, 0, false
	}
	return req, req, lim, true
}

func podMemoryLimit(pod *corev1.Pod) resource.Quantity {
	if pod == nil {
		return resource.Quantity{}
	}
	var total int64
	for _, c := range pod.Spec.Containers {
		if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && !lim.IsZero() {
			total += lim.MilliValue()
		}
	}
	return memoryQty(total)
}

func recMemoryLimit(rec attunev1alpha1.WorkloadRecommendation) resource.Quantity {
	var total int64
	for _, c := range rec.Containers {
		if !c.Recommended.MemoryLimit.IsZero() {
			total += c.Recommended.MemoryLimit.MilliValue()
		}
	}
	return memoryQty(total)
}

func recContainerMemoryLimit(rec attunev1alpha1.WorkloadRecommendation, container string) resource.Quantity {
	for _, c := range rec.Containers {
		if c.Name == container && !c.Recommended.MemoryLimit.IsZero() {
			return c.Recommended.MemoryLimit.DeepCopy()
		}
	}
	return resource.Quantity{}
}

// memoryCap is the memory twin of capLimit. requestsOnly is the flag for
// this metric: the named container, or any non-excluded app container on
// a pod-level Resource metric.
func (s hpaTuneScope) memoryCap(newMilli, liveLimitMilli int64, recLimit resource.Quantity, requestsOnly bool) resource.Quantity {
	return capAtLimit(requestsOnly, newMilli, liveLimitMilli, recLimit, memoryQty)
}

// memoryMetricBasis recognizes memory utilization metrics. A nil metric
// is not recognized. A pod with no memory request leaves old and new at
// zero, so tuneHPAs skips that metric.
func (s hpaTuneScope) memoryMetricBasis(m *autoscalingv2.MetricSpec) (recognized bool, b hpaMetricBasis) {
	if m == nil {
		return false, b
	}
	switch {
	case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
		m.Resource.Name == corev1.ResourceMemory &&
		m.Resource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.Resource.Target.AverageUtilization != nil:
		b.resource = true
		b.resName = string(corev1.ResourceMemory)
		b.targetKey = annotationHPAOriginalMemory
		b.baseKey = annotationHPAOriginalMemoryRequest
		if s.badMem || s.pod == nil {
			return true, b
		}
		oldMilli, newMilli, ok := podMemoryMillis(s.pod, s.memRows)
		if !ok {
			return true, b
		}
		var liveLimit int64
		if lim := podMemoryLimit(s.pod); !lim.IsZero() {
			liveLimit = lim.MilliValue()
		}
		b.ok = true
		b.oldMilli = oldMilli
		b.newMilli = newMilli
		b.limit = s.memoryCap(newMilli, liveLimit, recMemoryLimit(s.rec),
			podResourceRequestsOnly(s.policy, s.pod, corev1.ResourceMemory, s.memRequestsOnly))
		return true, b
	case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil &&
		m.ContainerResource.Name == corev1.ResourceMemory &&
		m.ContainerResource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.ContainerResource.Target.AverageUtilization != nil:
		b.container = m.ContainerResource.Container
		b.resName = string(corev1.ResourceMemory)
		b.containerMemory = true
		if b.container == "" {
			return true, b
		}
		oldMilli, newMilli, liveLimit, ok := containerMemoryMillis(s.pod, b.container, s.memRows)
		if !ok {
			return true, b
		}
		b.ok = true
		b.oldMilli = oldMilli
		b.newMilli = newMilli
		b.limit = s.memoryCap(newMilli, liveLimit, recContainerMemoryLimit(s.rec, b.container),
			containerControlledRequestsOnly(s.policy, b.container, corev1.ResourceMemory))
		return true, b
	default:
		return false, b
	}
}

func bandFor(policy *attunev1alpha1.AttunePolicy, resName string) *attunev1alpha1.HPATargetBound {
	if policy == nil || policy.Spec.UpdateStrategy == nil || policy.Spec.UpdateStrategy.HPATargetBounds == nil {
		return nil
	}
	bounds := policy.Spec.UpdateStrategy.HPATargetBounds
	if resName == string(corev1.ResourceMemory) {
		return bounds.Memory
	}
	return bounds.CPU
}

// publishHPATarget applies the limit cap, then an optional user band.
// When the band is nil or both ends are unset, the result is capHPATarget
// alone and emit is false. computed is the post-limit percent. emit is
// true only when the user band changes that percent.
func publishHPATarget(raw int32, limit, newRequest resource.Quantity, band *attunev1alpha1.HPATargetBound) (published, computed int32, emit bool) {
	if band == nil || (band.Min == nil && band.Max == nil) {
		published = capHPATarget(raw, limit, newRequest)
		return published, published, false
	}
	floored := raw
	if floored < 1 {
		floored = 1
	}
	computed = capHPATarget(floored, limit, newRequest)
	published = computed
	if band.Max != nil && published > *band.Max {
		published = *band.Max
	}
	if band.Min != nil && published < *band.Min {
		published = *band.Min
	}
	published = capHPATarget(published, limit, newRequest)
	return published, computed, published != computed
}

func hpaRequestQuantity(resName string, milli int64) resource.Quantity {
	if resName == string(corev1.ResourceMemory) {
		return memoryQty(milli)
	}
	return milliQty(milli)
}

func hpaClampMessage(namespace, name, resName, container string, computed, published int32) string {
	if container != "" {
		return "HPA " + namespace + "/" + name + " " + resName + " container " + container +
			" target " + strconv.FormatInt(int64(computed), 10) + " clamped to " + strconv.FormatInt(int64(published), 10)
	}
	return "HPA " + namespace + "/" + name + " " + resName +
		" target " + strconv.FormatInt(int64(computed), 10) + " clamped to " + strconv.FormatInt(int64(published), 10)
}

func parseContainerMemory(ann map[string]string) (map[string]hpaContainerMemoryEntry, bool) {
	if len(ann) == 0 {
		return map[string]hpaContainerMemoryEntry{}, true
	}
	raw, exists := ann[annotationHPAOriginalContainerMemory]
	if !exists || raw == "" {
		return map[string]hpaContainerMemoryEntry{}, true
	}
	var entries map[string]hpaContainerMemoryEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil {
		return nil, false
	}
	return entries, true
}

func storedContainerMemory(ann map[string]string, container string) (int32, int64, bool) {
	entries, ok := parseContainerMemory(ann)
	if !ok {
		return 0, 0, false
	}
	entry, exists := entries[container]
	if !exists || entry.Target == "" || entry.Request == "" {
		return 0, 0, false
	}
	target, err := strconv.ParseInt(entry.Target, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	qty, qtyErr := resource.ParseQuantity(entry.Request)
	if qtyErr != nil || qty.IsZero() {
		return 0, 0, false
	}
	return int32(target), qty.MilliValue(), true
}

// rememberContainerMemory merges one container into the JSON object.
// Corrupt JSON is left unchanged. An existing target and request pair
// is not rewritten.
func rememberContainerMemory(ann map[string]string, container string, target int32, oldMilli int64) {
	if ann == nil || container == "" {
		return
	}
	entries, ok := parseContainerMemory(ann)
	if !ok {
		return
	}
	if entry, exists := entries[container]; exists && entry.Target != "" && entry.Request != "" {
		return
	}
	request := memoryQty(oldMilli)
	entries[container] = hpaContainerMemoryEntry{
		Target:  strconv.FormatInt(int64(target), 10),
		Request: request.String(),
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return
	}
	ann[annotationHPAOriginalContainerMemory] = string(encoded)
}
