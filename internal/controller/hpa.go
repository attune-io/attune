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
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// retuneHPAAfterResize rebases auto-tune HPA CPU targets from this-cycle
// successful in-place CPU apply (history To), not the raw recommendation.
// Resource metrics use one pod's CPU total. ContainerResource metrics use
// only the named container.
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
		if !hpaCPURowsChanged(rows) {
			continue
		}
		var pods []corev1.Pod
		if podsByWorkload != nil {
			pods = podsByWorkload[rec.Workload]
		}
		r.tuneHPAs(ctx, hpas, rec.Workload, rec.Kind, hpaTuneScope{
			rows:         rows,
			badCPU:       badCPU,
			pod:          firstPodWithPositiveCPURequest(pods),
			requestsOnly: resourceControlledRequestsOnly(policy, corev1.ResourceCPU),
			rec:          rec,
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

// hpaTuneScope is either one precomputed pair (scalar) or per-metric ratios
// from history plus one live pod (retune).
type hpaTuneScope struct {
	scalar       bool
	old, neu     resource.Quantity
	limit        resource.Quantity
	rows         map[string]hpaCPURow
	badCPU       bool
	pod          *corev1.Pod
	requestsOnly bool
	rec          attunev1alpha1.WorkloadRecommendation
}

type hpaMetricBasis struct {
	ok                 bool
	longKey            bool
	resource           bool
	container          string
	oldMilli, newMilli int64
	limit              resource.Quantity
	targetKey, baseKey string
}

type hpaPendingTarget struct {
	index     int
	resource  bool
	container string
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

func (s hpaTuneScope) capLimit(newMilli, liveLimitMilli int64, recLimit resource.Quantity) resource.Quantity {
	if s.scalar {
		return s.limit
	}
	if !s.requestsOnly {
		if !recLimit.IsZero() {
			return recLimit
		}
		return milliQty(newMilli)
	}
	if liveLimitMilli > 0 {
		return milliQty(liveLimitMilli)
	}
	return resource.Quantity{}
}

func (s hpaTuneScope) metricBasis(m *autoscalingv2.MetricSpec) (recognized bool, b hpaMetricBasis) {
	switch {
	case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
		m.Resource.Name == corev1.ResourceCPU &&
		m.Resource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.Resource.Target.AverageUtilization != nil:
		b.resource = true
		b.targetKey = annotationHPAOriginalCPU
		b.baseKey = annotationHPAOriginalCPURequest
		if s.scalar {
			b.ok = true
			b.oldMilli = s.old.MilliValue()
			b.newMilli = s.neu.MilliValue()
			b.limit = s.limit
			return true, b
		}
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
		b.limit = s.capLimit(newMilli, liveLimit, recCPULimitFromRecommendation(s.rec))
		return true, b
	case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil &&
		m.ContainerResource.Name == corev1.ResourceCPU &&
		m.ContainerResource.Target.Type == autoscalingv2.UtilizationMetricType &&
		m.ContainerResource.Target.AverageUtilization != nil:
		b.container = m.ContainerResource.Container
		if s.scalar {
			b.ok = true
			b.oldMilli = s.old.MilliValue()
			b.newMilli = s.neu.MilliValue()
			b.limit = s.limit
			b.targetKey = annotationHPAOriginalCPU
			b.baseKey = annotationHPAOriginalCPURequest
			return true, b
		}
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
		b.limit = s.capLimit(newMilli, liveLimit, recContainerCPULimit(s.rec, b.container))
		b.targetKey = targetKey
		b.baseKey = baseKey
		return true, b
	default:
		return false, b
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

func cpuMetricMatches(m *autoscalingv2.MetricSpec, resource bool, container string) bool {
	if resource {
		return m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
			m.Resource.Name == corev1.ResourceCPU && m.Resource.Target.AverageUtilization != nil
	}
	if m.Type != autoscalingv2.ContainerResourceMetricSourceType || m.ContainerResource == nil ||
		m.ContainerResource.Name != corev1.ResourceCPU || m.ContainerResource.Target.AverageUtilization == nil {
		return false
	}
	if container != "" && m.ContainerResource.Container != container {
		return false
	}
	return true
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

func applyHPAMetricTarget(hpa *autoscalingv2.HorizontalPodAutoscaler, index int, resource bool, container string, target int32) {
	if index >= 0 && index < len(hpa.Spec.Metrics) && cpuMetricMatches(&hpa.Spec.Metrics[index], resource, container) {
		setAverageUtilization(&hpa.Spec.Metrics[index], target)
		return
	}
	for i := range hpa.Spec.Metrics {
		if cpuMetricMatches(&hpa.Spec.Metrics[i], resource, container) {
			setAverageUtilization(&hpa.Spec.Metrics[i], target)
			return
		}
	}
}

func copyHPATuneAnnotations(dst, src map[string]string) {
	if src == nil {
		return
	}
	for _, k := range []string{annotationHPAAutoTune, annotationHPAOriginalCPU, annotationHPAOriginalCPURequest} {
		if v, ok := src[k]; ok {
			dst[k] = v
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

		foundCPU := false
		pending := make([]hpaPendingTarget, 0, len(hpa.Spec.Metrics))
		for j := range hpa.Spec.Metrics {
			m := &hpa.Spec.Metrics[j]
			recognized, basis := scope.metricBasis(m)
			if !recognized {
				continue
			}
			foundCPU = true
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
			if storedTarget, storedMilli, ok := storedHPABase(hpa.Annotations, basis.targetKey, basis.baseKey); ok {
				baseTarget = storedTarget
				baseRequestMilli = storedMilli
			}
			newTarget := int32(float64(baseTarget) * float64(baseRequestMilli) / float64(basis.newMilli))
			oldQ := milliQty(basis.oldMilli)
			newQ := milliQty(basis.newMilli)
			newTarget = capHPATarget(newTarget, basis.limit, newQ)
			if newTarget == currentTarget {
				continue
			}
			// First write only. A stored base that differs from this cycle's
			// sum is the usual second resize, not a cue to rewrite it.
			if hpa.Annotations[basis.targetKey] == "" {
				if hpa.Annotations == nil {
					hpa.Annotations = make(map[string]string)
				}
				hpa.Annotations[basis.targetKey] = strconv.FormatInt(int64(currentTarget), 10)
				hpa.Annotations[basis.baseKey] = oldQ.String()
			}
			logger.Info("Auto-tuning HPA CPU target after resize",
				"hpa", hpa.Name, "workload", workloadName, "container", basis.container,
				"currentTarget", currentTarget, "newTarget", newTarget,
				"oldRequest", oldQ.String(), "newRequest", newQ.String())
			applyHPAMetricTarget(hpa, j, basis.resource, basis.container, newTarget)
			pending = append(pending, hpaPendingTarget{
				index:     j,
				resource:  basis.resource,
				container: basis.container,
				target:    newTarget,
			})
		}
		if len(pending) == 0 {
			if !foundCPU {
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
			applyHPAMetricTarget(&fresh, upd.index, upd.resource, upd.container, upd.target)
		}
		if err := r.Update(ctx, &fresh); err != nil {
			logger.Error(err, "Failed to update HPA target", "hpa", hpa.Name)
			continue
		}
	}
}
