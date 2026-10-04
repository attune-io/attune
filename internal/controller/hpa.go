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
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// retuneHPAAfterResize rebases auto-tune HPA CPU and memory targets from
// this-cycle successful in-place applies (history To), not the raw
// recommendation. Resource metrics use one pod's request total.
// ContainerResource metrics use only the named container.
func (r *AttunePolicyReconciler) retuneHPAAfterResize(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	mode attunev1alpha1.UpdateType,
	history []attunev1alpha1.ResizeHistoryEntry,
	recommendations []attunev1alpha1.WorkloadRecommendation,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	podsByWorkload map[string][]corev1.Pod,
) {
	if r == nil || len(hpas) == 0 {
		return
	}
	for _, rec := range recommendations {
		if mode == attunev1alpha1.UpdateTypeCanary && policy != nil && !policy.Status.Canary.AllowsHPARetune(rec.Workload) {
			continue
		}
		rows, badCPU := hpaCPURowsFromHistory(history, rec.Workload)
		memRows, badMem := hpaMemoryRowsFromHistory(history, rec.Workload)
		if !hpaCPURowsChanged(rows) && !hpaMemoryRowsChanged(memRows) {
			continue
		}
		var pods []corev1.Pod
		if podsByWorkload != nil {
			pods = podsByWorkload[rec.Workload]
		}
		pod := firstPodWithPositiveCPURequest(pods)
		if pod == nil {
			pod = firstPodWithPositiveMemoryRequest(pods)
		}
		r.tuneHPAs(ctx, hpas, rec.Workload, rec.Kind, hpaTuneScope{
			rows:            rows,
			badCPU:          badCPU,
			memRows:         memRows,
			badMem:          badMem,
			pod:             pod,
			initNames:       initContainerNames(pods),
			requestsOnly:    resourceControlledRequestsOnly(policy, corev1.ResourceCPU),
			memRequestsOnly: resourceControlledRequestsOnly(policy, corev1.ResourceMemory),
			rec:             rec,
			policy:          policy,
		})
	}
}

// hpaCPUFromResizeHistory sums From/To on this-cycle successful in-place
// CPU rows for workload. ok is false when no parseable rows exist.
func hpaCPUFromResizeHistory(history []attunev1alpha1.ResizeHistoryEntry, workload string) (oldCPU, newCPU resource.Quantity, ok bool) {
	type pair struct{ old, neu int64 }
	byContainer := map[string]pair{}
	for _, h := range history {
		if h.Workload != workload || h.Resource != "cpu" || !isSuccessfulInPlaceHistory(h) {
			continue
		}
		from, fromErr := resource.ParseQuantity(h.From)
		to, toErr := resource.ParseQuantity(h.To)
		if fromErr != nil || toErr != nil {
			continue
		}
		if _, seen := byContainer[h.Container]; seen {
			continue
		}
		byContainer[h.Container] = pair{from.MilliValue(), to.MilliValue()}
	}
	if len(byContainer) == 0 {
		return resource.Quantity{}, resource.Quantity{}, false
	}
	var oldMilli, newMilli int64
	for _, p := range byContainer {
		oldMilli += p.old
		newMilli += p.neu
	}
	return *resource.NewMilliQuantity(oldMilli, resource.DecimalSI),
		*resource.NewMilliQuantity(newMilli, resource.DecimalSI), true
}

// destCPULimitFromPods is one pod's leftover dest CPU (sum of that pod's
// containers). Fleet sums would make the HPA cap depend on replica count.
func destCPULimitFromPods(pods []corev1.Pod) resource.Quantity {
	for i := range pods {
		var total int64
		for _, c := range pods[i].Spec.Containers {
			if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok && !lim.IsZero() {
				total += lim.MilliValue()
			}
		}
		if total > 0 {
			return *resource.NewMilliQuantity(total, resource.DecimalSI)
		}
	}
	return resource.Quantity{}
}

// recCPULimitFromRecommendation is per-pod dest (sum of container rec dests).
func recCPULimitFromRecommendation(rec attunev1alpha1.WorkloadRecommendation) resource.Quantity {
	var total int64
	for _, c := range rec.Containers {
		if !c.Recommended.CPULimit.IsZero() {
			total += c.Recommended.CPULimit.MilliValue()
		}
	}
	if total == 0 {
		return resource.Quantity{}
	}
	return *resource.NewMilliQuantity(total, resource.DecimalSI)
}

// hpaCPURow is the first successful in-place CPU history row for one container.
// ok is false when that first row does not parse.
type hpaCPURow struct {
	old, neu int64
	ok       bool
}

// hpaTuneScope is per-metric ratios from history plus one live pod.
// initNames holds the init container names of every pod of the workload.
type hpaTuneScope struct {
	rows            map[string]hpaCPURow
	badCPU          bool
	memRows         map[string]hpaCPURow
	badMem          bool
	pod             *corev1.Pod
	initNames       map[string]struct{}
	requestsOnly    bool
	memRequestsOnly bool
	rec             attunev1alpha1.WorkloadRecommendation
	policy          *attunev1alpha1.AttunePolicy
}

type hpaMetricBasis struct {
	ok                 bool
	longKey            bool
	resource           bool
	containerMemory    bool
	container          string
	resName            string
	oldMilli, newMilli int64
	limit              resource.Quantity
	targetKey, baseKey string
}

type hpaPendingTarget struct {
	index     int
	resource  bool
	container string
	resName   string
	target    int32
}

// hpaCPURowsFromHistory keeps the first successful in-place CPU row per
// container. bad is true when any such row has an unparseable From or To.
func hpaCPURowsFromHistory(history []attunev1alpha1.ResizeHistoryEntry, workload string) (map[string]hpaCPURow, bool) {
	rows := make(map[string]hpaCPURow)
	bad := false
	for _, h := range history {
		if h.Workload != workload || h.Resource != "cpu" || !isSuccessfulInPlaceHistory(h) {
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

func hpaCPURowsChanged(rows map[string]hpaCPURow) bool {
	for _, row := range rows {
		if row.ok && row.old != row.neu {
			return true
		}
	}
	return false
}

// firstPodWithPositiveCPURequest is the first pod whose Spec.Containers CPU
// requests sum to more than zero. One pod, not a replica sum. Init containers
// are not included.
func firstPodWithPositiveCPURequest(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		var total int64
		for _, c := range pods[i].Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				total += q.MilliValue()
			}
		}
		if total > 0 {
			return &pods[i]
		}
	}
	return nil
}

func milliQty(milli int64) resource.Quantity {
	return *resource.NewMilliQuantity(milli, resource.DecimalSI)
}

// resourceHistoryOldMilli is the pre-resize sum of history rows that
// podCPUMillis counts. Init containers, including native sidecars, are
// left out. Containers with no row are not included.
func resourceHistoryOldMilli(pod *corev1.Pod, rows map[string]hpaCPURow) int64 {
	if pod == nil {
		var sum int64
		for _, row := range rows {
			if row.ok {
				sum += row.old
			}
		}
		return sum
	}
	seen := make(map[string]struct{}, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	var sum int64
	for _, c := range pod.Spec.Containers {
		seen[c.Name] = struct{}{}
		if row, ok := rows[c.Name]; ok && row.ok {
			sum += row.old
		}
	}
	for _, c := range pod.Spec.InitContainers {
		seen[c.Name] = struct{}{}
	}
	for name, row := range rows {
		if _, onPod := seen[name]; onPod || !row.ok {
			continue
		}
		sum += row.old
	}
	return sum
}

func podCPUMillis(pod *corev1.Pod, rows map[string]hpaCPURow) (oldMilli, newMilli int64, ok bool) {
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
		if q, has := c.Resources.Requests[corev1.ResourceCPU]; has {
			v := q.MilliValue()
			oldMilli += v
			newMilli += v
		}
	}
	// Init containers are on the pod, so their history is not a removed
	// container. They stay out of the pod-level Resource total.
	for _, c := range pod.Spec.InitContainers {
		seen[c.Name] = struct{}{}
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

// initContainerNames is the set of init container names, native sidecars
// included, across pods that have not finished. A Succeeded or Failed pod
// left behind by an eviction does not speak for the current template.
func initContainerNames(pods []corev1.Pod) map[string]struct{} {
	names := map[string]struct{}{}
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodSucceeded || pods[i].Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pods[i].Spec.InitContainers {
			names[c.Name] = struct{}{}
		}
	}
	return names
}

// cpuBaseContainerMillis is the pre-resize CPU per container for the set
// podCPUMillis sums: spec.containers (row old, else live request, else 0)
// and off-pod rows that are not init containers (row old). A name in
// initNames, an init container on some pod of the workload, is left out
// even where podCPUMillis counts it: a native sidecar seen as a row of
// another pod, or a container that is regular on the sampled pod and an
// init container on another.
func cpuBaseContainerMillis(pod *corev1.Pod, rows map[string]hpaCPURow, initNames map[string]struct{}) map[string]int64 {
	if pod == nil {
		return nil
	}
	out := make(map[string]int64, len(pod.Spec.Containers)+len(rows))
	seen := make(map[string]struct{}, len(pod.Spec.Containers)+len(pod.Spec.InitContainers)+len(initNames))
	for name := range initNames {
		seen[name] = struct{}{}
	}
	for _, c := range pod.Spec.Containers {
		if _, isInit := seen[c.Name]; isInit {
			continue
		}
		seen[c.Name] = struct{}{}
		if row, exists := rows[c.Name]; exists && row.ok {
			out[c.Name] = row.old
			continue
		}
		var v int64
		if q, has := c.Resources.Requests[corev1.ResourceCPU]; has {
			v = q.MilliValue()
		}
		out[c.Name] = v
	}
	for _, c := range pod.Spec.InitContainers {
		seen[c.Name] = struct{}{}
	}
	for name, row := range rows {
		if _, onPod := seen[name]; onPod || !row.ok {
			continue
		}
		out[name] = row.old
	}
	return out
}

// parseCPUBaseContainers reads the CPU base container list. Every element
// must be a container name (DNS-1123 label); otherwise the whole list is
// malformed and ok is false.
func parseCPUBaseContainers(v string) (map[string]struct{}, bool) {
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ",")
	set := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		if len(validation.IsDNS1123Label(p)) > 0 {
			return nil, false
		}
		set[p] = struct{}{}
	}
	return set, true
}

func sumMillis(m map[string]int64) int64 {
	var sum int64
	for _, v := range m {
		sum += v
	}
	return sum
}

// formatCPUBaseContainers is the sorted, comma-separated list of the keys.
func formatCPUBaseContainers[V any](set map[string]V) string {
	return strings.Join(slices.Sorted(maps.Keys(set)), ",")
}

func liveContainerCPU(pod *corev1.Pod, name string) (reqMilli, limitMilli int64, found bool) {
	if pod == nil || name == "" {
		return 0, 0, false
	}
	// ContainerResource can name an init container or a native sidecar.
	for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range list {
			if c.Name != name {
				continue
			}
			if q, has := c.Resources.Requests[corev1.ResourceCPU]; has {
				reqMilli = q.MilliValue()
			}
			if q, has := c.Resources.Limits[corev1.ResourceCPU]; has {
				limitMilli = q.MilliValue()
			}
			return reqMilli, limitMilli, true
		}
	}
	return 0, 0, false
}

func containerLimitMilli(pod *corev1.Pod, name string) int64 {
	_, lim, found := liveContainerCPU(pod, name)
	if !found {
		return 0
	}
	return lim
}

func containerCPUMillis(pod *corev1.Pod, name string, rows map[string]hpaCPURow) (oldMilli, newMilli, limitMilli int64, ok bool) {
	if row, exists := rows[name]; exists {
		if !row.ok {
			return 0, 0, 0, false
		}
		return row.old, row.neu, containerLimitMilli(pod, name), true
	}
	req, lim, found := liveContainerCPU(pod, name)
	if !found {
		return 0, 0, 0, false
	}
	return req, req, lim, true
}

func recContainerCPULimit(rec attunev1alpha1.WorkloadRecommendation, container string) resource.Quantity {
	for _, c := range rec.Containers {
		if c.Name == container && !c.Recommended.CPULimit.IsZero() {
			return c.Recommended.CPULimit.DeepCopy()
		}
	}
	return resource.Quantity{}
}

// hpaContainerAnnotationKeys returns the ContainerResource annotation pair.
// The name segment after attune.io/ must be at most 63 characters.
func hpaContainerAnnotationKeys(container string) (targetKey, baseKey string, ok bool) {
	if container == "" {
		return "", "", false
	}
	targetName := "hpa-cpu-target." + container
	baseName := "hpa-cpu-base." + container
	if len(targetName) > 63 || len(baseName) > 63 {
		return "", "", false
	}
	return annotationHPACPUTargetPrefix + container, annotationHPACPUBasePrefix + container, true
}

// capAtLimit is the utilization ceiling for one HPA metric. Requests-only
// uses the live limit. RequestsAndLimits uses the recommended limit, or
// the new request when that limit is unset.
func capAtLimit(requestsOnly bool, newMilli, liveLimitMilli int64, recLimit resource.Quantity, qty func(int64) resource.Quantity) resource.Quantity {
	if !requestsOnly {
		if !recLimit.IsZero() {
			return recLimit
		}
		return qty(newMilli)
	}
	if liveLimitMilli > 0 {
		return qty(liveLimitMilli)
	}
	return resource.Quantity{}
}

func (s hpaTuneScope) capLimit(newMilli, liveLimitMilli int64, recLimit resource.Quantity, requestsOnly bool) resource.Quantity {
	return capAtLimit(requestsOnly, newMilli, liveLimitMilli, recLimit, milliQty)
}

func (s hpaTuneScope) metricBasis(m *autoscalingv2.MetricSpec) (recognized bool, b hpaMetricBasis) {
	switch {
	case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
		m.Resource.Name == corev1.ResourceCPU &&
		m.Resource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.Resource.Target.AverageUtilization != nil:
		b.resource = true
		b.resName = string(corev1.ResourceCPU)
		b.targetKey = annotationHPAOriginalCPU
		b.baseKey = annotationHPAOriginalCPURequest
		if s.badCPU || s.pod == nil {
			return true, b
		}
		oldMilli, newMilli, ok := podCPUMillis(s.pod, s.rows)
		if !ok {
			return true, b
		}
		var liveLimit int64
		if lim := destCPULimitFromPods([]corev1.Pod{*s.pod}); !lim.IsZero() {
			liveLimit = lim.MilliValue()
		}
		b.ok = true
		b.oldMilli = oldMilli
		b.newMilli = newMilli
		b.limit = s.capLimit(newMilli, liveLimit, recCPULimitFromRecommendation(s.rec),
			podResourceRequestsOnly(s.policy, s.pod, corev1.ResourceCPU, s.requestsOnly))
		return true, b
	case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil &&
		m.ContainerResource.Name == corev1.ResourceCPU &&
		m.ContainerResource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.ContainerResource.Target.AverageUtilization != nil:
		b.container = m.ContainerResource.Container
		b.resName = string(corev1.ResourceCPU)
		if b.container == "" {
			return true, b
		}
		targetKey, baseKey, keyOK := hpaContainerAnnotationKeys(b.container)
		if !keyOK {
			b.longKey = true
			return true, b
		}
		oldMilli, newMilli, liveLimit, ok := containerCPUMillis(s.pod, b.container, s.rows)
		if !ok {
			return true, b
		}
		b.ok = true
		b.oldMilli = oldMilli
		b.newMilli = newMilli
		b.limit = s.capLimit(newMilli, liveLimit, recContainerCPULimit(s.rec, b.container),
			containerControlledRequestsOnly(s.policy, b.container, corev1.ResourceCPU))
		b.targetKey = targetKey
		b.baseKey = baseKey
		return true, b
	default:
		return s.memoryMetricBasis(m)
	}
}

func storedHPABase(ann map[string]string, targetKey, baseKey string) (int32, int64, bool) {
	if len(ann) == 0 || targetKey == "" || baseKey == "" {
		return 0, 0, false
	}
	storedTarget := ann[targetKey]
	storedRequest := ann[baseKey]
	if storedTarget == "" || storedRequest == "" {
		return 0, 0, false
	}
	v, err := strconv.ParseInt(storedTarget, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	q, qErr := resource.ParseQuantity(storedRequest)
	if qErr != nil || q.IsZero() {
		return 0, 0, false
	}
	return int32(v), q.MilliValue(), true
}

// storedPairReproducesTarget reports whether currentTarget is exactly what
// the stored target and base publish for this cycle's pre-resize request,
// after the band and the limit cap. An unclamped match means this operator
// made the last write at a new request equal to this cycle's old one: that
// write used the same int32 truncation, so it reproduces exactly. A clamped
// match is ambiguous, since many stored pairs publish the same number, and
// it also reports true: under ambiguity the stored base is kept.
func storedPairReproducesTarget(currentTarget, storedTarget int32, storedMilli int64, basis hpaMetricBasis, band *attunev1alpha1.HPATargetBound) bool {
	raw := int32(float64(storedTarget) * float64(storedMilli) / float64(basis.oldMilli))
	want, _, _ := publishHPATarget(raw, basis.limit, hpaRequestQuantity(basis.resName, basis.oldMilli), band)
	return currentTarget == want
}

func capHPATarget(newTarget int32, limit, newRequest resource.Quantity) int32 {
	maxTarget := int32(100)
	if !newRequest.IsZero() && !limit.IsZero() && limit.Cmp(newRequest) > 0 {
		maxTarget = int32(float64(limit.MilliValue()) / float64(newRequest.MilliValue()) * 100)
	}
	if newTarget > maxTarget {
		newTarget = maxTarget
	}
	if newTarget < 1 {
		newTarget = 1
	}
	return newTarget
}

func setAverageUtilization(m *autoscalingv2.MetricSpec, target int32) {
	v := target
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource != nil {
			m.Resource.Target.AverageUtilization = &v
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource != nil {
			m.ContainerResource.Target.AverageUtilization = &v
		}
	}
}

func utilizationMetricMatches(m *autoscalingv2.MetricSpec, resName string, resource bool, container string) bool {
	if resName == "" {
		resName = string(corev1.ResourceCPU)
	}
	name := corev1.ResourceName(resName)
	if resource {
		return m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
			m.Resource.Name == name && m.Resource.Target.AverageUtilization != nil
	}
	if m.Type != autoscalingv2.ContainerResourceMetricSourceType || m.ContainerResource == nil ||
		m.ContainerResource.Name != name || m.ContainerResource.Target.AverageUtilization == nil {
		return false
	}
	if container != "" && m.ContainerResource.Container != container {
		return false
	}
	return true
}

func applyHPAMetricTarget(hpa *autoscalingv2.HorizontalPodAutoscaler, index int, resName string, resource bool, container string, target int32) {
	if index >= 0 && index < len(hpa.Spec.Metrics) && utilizationMetricMatches(&hpa.Spec.Metrics[index], resName, resource, container) {
		setAverageUtilization(&hpa.Spec.Metrics[index], target)
		return
	}
	for i := range hpa.Spec.Metrics {
		if utilizationMetricMatches(&hpa.Spec.Metrics[i], resName, resource, container) {
			setAverageUtilization(&hpa.Spec.Metrics[i], target)
			return
		}
	}
}

func copyHPATuneAnnotations(dst, src map[string]string) {
	if src == nil {
		return
	}
	for _, k := range []string{
		annotationHPAAutoTune,
		annotationHPAOriginalCPU,
		annotationHPAOriginalCPURequest,
		annotationHPAOriginalCPURequestContainers,
		annotationHPAOriginalMemory,
		annotationHPAOriginalMemoryRequest,
		annotationHPAOriginalContainerMemory,
	} {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
	// The CPU base and its container list are one unit: a source base
	// without a list must not keep a list that belongs to another base.
	if _, hasBase := src[annotationHPAOriginalCPURequest]; hasBase {
		if _, hasList := src[annotationHPAOriginalCPURequestContainers]; !hasList {
			delete(dst, annotationHPAOriginalCPURequestContainers)
		}
	}
	for k, v := range src {
		if strings.HasPrefix(k, annotationHPACPUTargetPrefix) || strings.HasPrefix(k, annotationHPACPUBasePrefix) {
			dst[k] = v
		}
	}
}

// tuneHPAs updates every matching auto-tune HPA. Changed metrics on one HPA
// are written with a single Update. Get or Update errors are logged and do
// not abort the next HPA.
func (r *AttunePolicyReconciler) tuneHPAs(
	ctx context.Context,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	workloadName, workloadKind string,
	scope hpaTuneScope,
) {
	logger := log.FromContext(ctx)
	for i := range hpas {
		hpa := &hpas[i]
		if hpa.Spec.ScaleTargetRef.Name != workloadName || hpa.Spec.ScaleTargetRef.Kind != workloadKind {
			continue
		}
		if hpa.Annotations == nil || hpa.Annotations[annotationHPAAutoTune] != "true" {
			continue
		}

		foundAdjustable := false
		pending := make([]hpaPendingTarget, 0, len(hpa.Spec.Metrics))
		var clamps []hpaClampNote
		for j := range hpa.Spec.Metrics {
			m := &hpa.Spec.Metrics[j]
			recognized, basis := scope.metricBasis(m)
			if !recognized {
				continue
			}
			foundAdjustable = true
			if basis.longKey {
				logger.Info("Skipping HPA ContainerResource auto-tune because the annotation name exceeds 63 characters",
					"hpa", hpa.Name, "workload", workloadName, "container", basis.container)
				continue
			}
			if !basis.ok || basis.oldMilli == 0 || basis.newMilli == 0 || basis.oldMilli == basis.newMilli {
				continue
			}
			var currentTarget int32
			if basis.resource {
				currentTarget = *m.Resource.Target.AverageUtilization
			} else {
				currentTarget = *m.ContainerResource.Target.AverageUtilization
			}
			// newTarget = originalTarget * (originalRequest / newRequest).
			// A stored percent with this cycle's old/new request drifts.
			// Legacy rows that only have the target annotation use
			// currentTarget * (old / new) and do not gain a request annotation.
			baseTarget := currentTarget
			baseRequestMilli := basis.oldMilli
			repairPartialCPU := false
			// writeCPUList and cpuList carry a list repair to the write below.
			writeCPUList := false
			var cpuList string
			podCPU := basis.resource && basis.resName == string(corev1.ResourceCPU)
			// perContainer is what a pod CPU base written in this cycle
			// holds; the written base is the sum of its values.
			var perContainer map[string]int64
			if podCPU {
				perContainer = cpuBaseContainerMillis(scope.pod, scope.rows, scope.initNames)
			}
			if basis.containerMemory {
				if storedTarget, storedMilli, ok := storedContainerMemory(hpa.Annotations, basis.container); ok {
					baseTarget = storedTarget
					baseRequestMilli = storedMilli
				}
			} else if storedTarget, storedMilli, ok := storedHPABase(hpa.Annotations, basis.targetKey, basis.baseKey); ok {
				baseTarget = storedTarget
				baseRequestMilli = storedMilli
				var listed map[string]struct{}
				listOK := false
				if podCPU {
					if v, has := hpa.Annotations[annotationHPAOriginalCPURequestContainers]; has {
						if listed, listOK = parseCPUBaseContainers(v); !listOK {
							logger.Info("HPA CPU base container list is malformed; using the rule for bases without a list",
								"hpa", hpa.Name, "workload", workloadName, "annotation", annotationHPAOriginalCPURequestContainers)
						}
					}
				}
				switch {
				case listOK:
					// The list records which containers the base holds.
					// A container missing from it is added at this cycle's
					// pre-resize CPU. A listed container that is not on the
					// pod keeps its share and its name: dropping it would
					// add it again on re-add, and ratchet when a rollout
					// alternates pods with and without it.
					var removed []string
					for name := range listed {
						if _, ok := perContainer[name]; !ok {
							removed = append(removed, name)
						}
					}
					var added int64
					for name, v := range perContainer {
						if _, ok := listed[name]; !ok {
							listed[name] = struct{}{}
							added += v
							writeCPUList = true
						}
					}
					if writeCPUList {
						baseRequestMilli = storedMilli + added
						cpuList = formatCPUBaseContainers(listed)
						repairPartialCPU = added > 0
					}
					if len(removed) > 0 {
						slices.Sort(removed)
						logger.Info("HPA CPU base lists containers that are not on the pod; stored base kept",
							"hpa", hpa.Name, "workload", workloadName, "containers", removed)
					}
				// Without a list, a stored pod CPU base below this cycle's
				// pre-resize sum can be missing containers, or it can be the
				// full original request after later growth; the stored
				// annotations alone cannot tell which containers the base
				// held. Repair to the pre-resize sum only when the stored base
				// equals the history old sum of the resized containers (no
				// room for the containers without a row) and the current
				// target is not exactly what the stored pair publishes for
				// that sum, clamped or not. The published target is used only
				// as a one-sided veto (see storedPairReproducesTarget). Any
				// other stored base is kept. ContainerResource bases stay per
				// container. The base written by the repair leaves out init
				// containers of other pods, so it must still exceed the
				// stored base.
				case podCPU && storedMilli < basis.oldMilli &&
					storedMilli == resourceHistoryOldMilli(scope.pod, scope.rows) &&
					!storedPairReproducesTarget(currentTarget, storedTarget, storedMilli, basis, bandFor(scope.policy, basis.resName)) &&
					sumMillis(perContainer) > storedMilli:
					baseRequestMilli = basis.oldMilli
					repairPartialCPU = true
				}
			}
			rawTarget := int32(float64(baseTarget) * float64(baseRequestMilli) / float64(basis.newMilli))
			oldQ := hpaRequestQuantity(basis.resName, basis.oldMilli)
			newQ := hpaRequestQuantity(basis.resName, basis.newMilli)
			newTarget, computed, emit := publishHPATarget(rawTarget, basis.limit, newQ, bandFor(scope.policy, basis.resName))
			if newTarget == currentTarget {
				continue
			}
			// Write the base only on the first write or a repair; a base that
			// differs from this cycle's sum is the usual second resize.
			if hpa.Annotations == nil {
				hpa.Annotations = make(map[string]string)
			}
			if basis.containerMemory {
				rememberContainerMemory(hpa.Annotations, basis.container, currentTarget, basis.oldMilli)
			} else if basis.targetKey != "" && (hpa.Annotations[basis.targetKey] == "" || repairPartialCPU || writeCPUList) {
				hpa.Annotations[basis.targetKey] = strconv.FormatInt(int64(baseTarget), 10)
				writeMilli := baseRequestMilli
				// The base is written with the containers it holds: the
				// extended list on a list repair, else the containers of
				// perContainer at their sum (first write, repair without a
				// list). That sum is podCPUMillis' old sum unless a name in
				// scope.initNames is in it; the target of this cycle still
				// uses podCPUMillis.
				if podCPU {
					if cpuList == "" {
						cpuList = formatCPUBaseContainers(perContainer)
						writeMilli = sumMillis(perContainer)
					}
					hpa.Annotations[annotationHPAOriginalCPURequestContainers] = cpuList
				}
				baseQ := hpaRequestQuantity(basis.resName, writeMilli)
				hpa.Annotations[basis.baseKey] = baseQ.String()
				if repairPartialCPU && r.Recorder != nil && scope.policy != nil {
					r.Recorder.Eventf(scope.policy, nil, corev1.EventTypeWarning, "HPABaseRepaired", "hpa",
						"Stored %s was below the pre-resize pod sum and was replaced", annotationHPAOriginalCPURequest)
				}
			}
			if basis.resName == string(corev1.ResourceMemory) {
				logger.Info("Auto-tuning HPA memory target after resize",
					"hpa", hpa.Name, "workload", workloadName, "container", basis.container,
					"currentTarget", currentTarget, "newTarget", newTarget,
					"oldRequest", oldQ.String(), "newRequest", newQ.String())
			} else {
				logger.Info("Auto-tuning HPA CPU target after resize",
					"hpa", hpa.Name, "workload", workloadName, "container", basis.container,
					"currentTarget", currentTarget, "newTarget", newTarget,
					"oldRequest", oldQ.String(), "newRequest", newQ.String())
			}
			applyHPAMetricTarget(hpa, j, basis.resName, basis.resource, basis.container, newTarget)
			pending = append(pending, hpaPendingTarget{
				index:     j,
				resource:  basis.resource,
				container: basis.container,
				resName:   basis.resName,
				target:    newTarget,
			})
			if emit {
				clamps = append(clamps, hpaClampNote{
					message: hpaClampMessage(hpa.Namespace, hpa.Name, basis.resName, basis.container, computed, newTarget),
				})
			}
		}
		if len(pending) == 0 {
			if !foundAdjustable {
				logger.Info("HPA has auto-tune annotation but no adjustable CPU utilization metric",
					"hpa", hpa.Name, "workload", workloadName)
			}
			continue
		}

		// Re-fetch the HPA to get a fresh resourceVersion. The HPA list
		// was fetched at the start of Reconcile and the HPA controller
		// may have updated it since then (e.g., during concurrent resizes).
		var fresh autoscalingv2.HorizontalPodAutoscaler
		// Live API read so strip-transformed cache entries cannot wipe metrics.
		if getErr := r.liveReader().Get(ctx, types.NamespacedName{Name: hpa.Name, Namespace: hpa.Namespace}, &fresh); getErr != nil {
			logger.Error(getErr, "Failed to re-fetch HPA for target update", "hpa", hpa.Name)
			continue
		}
		// Apply only our operator annotations to the fresh copy.
		// Copying ALL annotations from the stale hpa would overwrite
		// annotations set by other controllers (ArgoCD, Flux, etc.)
		// between the initial List and this re-fetch.
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}
		copyHPATuneAnnotations(fresh.Annotations, hpa.Annotations)
		for _, upd := range pending {
			applyHPAMetricTarget(&fresh, upd.index, upd.resName, upd.resource, upd.container, upd.target)
		}
		if err := r.Update(ctx, &fresh); err != nil {
			logger.Error(err, "Failed to update HPA target", "hpa", hpa.Name)
			continue
		}
		for _, note := range clamps {
			r.emitEventOnce(scope.policy, corev1.EventTypeNormal, "HPATargetClamped", "hpa", "%s", note.message)
		}
	}
}
