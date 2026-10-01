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
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/safety"
)

// oomBumpMaxAllowed returns nil when the pointer is omitted (uncapped).
// A non-nil quantity is a real ceiling, including zero.
func oomBumpMaxAllowed(q *resource.Quantity) *int64 {
	if q == nil {
		return nil
	}
	v := q.Value()
	return &v
}

// oomBumpMaxBumps is the configured cap, or 3 when the field is omitted.
func oomBumpMaxBumps(policy *attunev1alpha1.AttunePolicy) int {
	if policy == nil || policy.Spec.Memory.OOMBump == nil || policy.Spec.Memory.OOMBump.MaxBumps == nil {
		return int(attunev1alpha1.DefaultOOMBumpMaxBumps)
	}
	n := int(*policy.Spec.Memory.OOMBump.MaxBumps)
	if n < 1 {
		return int(attunev1alpha1.DefaultOOMBumpMaxBumps)
	}
	return n
}

// planContainerOOMBump plans one container and emits skipped or capped now.
// Applied and clamped wait until the resize annotation is stored.
// Recommend mode publishes the floor and does not keep a stamp.
func (r *AttunePolicyReconciler) planContainerOOMBump(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	container string,
	excluded bool,
	percentileBytes int64,
	percentileOK bool,
	pods []corev1.Pod,
	now time.Time,
) oomBumpWorkloadPlan {
	if r == nil || policy == nil || policy.Spec.Memory.OOMBump == nil {
		return oomBumpWorkloadPlan{}
	}
	key, ok := oomBumpKey(container)
	if !ok {
		log.FromContext(ctx).V(1).Info("Skipping OOM bump; container name does not fit the annotation key",
			"container", container)
		return oomBumpWorkloadPlan{}
	}
	var workloadRaw string
	if workload != nil {
		if anns := workload.GetAnnotations(); anns != nil {
			workloadRaw = anns[key]
		}
	}
	var consumed []oomBumpConsumed
	if r.oomBumps != nil {
		pods, workloadRaw, consumed = stripClearedOOMBumps(r.oomBumps.Clears(string(policy.UID)), container, pods, workloadRaw)
	}
	plan := planWorkloadOOMBump(
		policy.Spec.Memory.OOMBump,
		container,
		excluded,
		percentileBytes,
		percentileOK,
		oomBumpMaxAllowed(effectiveMemoryMaxAllowed(policy, container)),
		workloadRaw,
		pods,
		now,
		consumed,
	)
	for _, result := range plan.MetricNow {
		operatormetrics.OOMBumpTotal.WithLabelValues(policy.Namespace, policy.Name, result).Inc()
	}
	if plan.Event != "" && r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, plan.Event, "resize",
			"OOM bump for container %s is capped at maxBumps", container)
	}
	var mode attunev1alpha1.UpdateType
	if policy.Spec.UpdateStrategy != nil {
		mode = policy.Spec.UpdateStrategy.Type
	}
	if r.oomBumps != nil && workload != nil && isResizeMode(mode) && len(plan.Stamps) > 0 {
		r.oomBumps.Put(
			string(policy.UID), policy.Namespace, policy.Name,
			workload.GetNamespace(), workload.GetName(), container,
			plan.Stamps, plan.BaseHeld,
		)
	}
	return plan
}

// applyOOMBumpToRecommendation overwrites the memory request when the plan
// publishes a floor. The note is independent of that overwrite.
// True means the request was replaced. Limits are scaled by the caller.
func applyOOMBumpToRecommendation(
	rec *attunev1alpha1.ContainerRecommendation,
	explanation *attunev1alpha1.ContainerRecommendationExplanation,
	plan oomBumpWorkloadPlan,
) bool {
	if rec == nil {
		return false
	}
	if plan.Note && explanation != nil {
		if explanation.Memory == nil {
			explanation.Memory = &attunev1alpha1.ResourceRecommendationExplanation{}
		}
		explanation.Memory.FinalAdjustment = appendNote(explanation.Memory.FinalAdjustment, "oomBump")
	}
	if !plan.UsePublish || plan.PublishBytes <= 0 {
		return false
	}
	rec.Recommended.MemoryRequest = *resource.NewQuantity(plan.PublishBytes, resource.BinarySI)
	return true
}

// oomBumpFallbackRec builds a recommendation when samples are not ready
// and a new OOM still needs a floor. Limits are not scaled here.
func (r *AttunePolicyReconciler) oomBumpFallbackRec(
	ctx context.Context,
	in recommendContainerInput,
) (attunev1alpha1.ContainerRecommendation, bool) {
	plan := r.planContainerOOMBump(ctx, in.policy, in.workload, in.container.Name, false, 0, false, in.pods, in.now)
	if !plan.UsePublish {
		return attunev1alpha1.ContainerRecommendation{}, false
	}
	rec := newContainerRecommendation(in.container, 0, 0, in.now)
	explanation := &attunev1alpha1.ContainerRecommendationExplanation{}
	applyOOMBumpToRecommendation(&rec, explanation, plan)
	if explanation.Memory != nil || explanation.CPU != nil {
		rec.Explanation = explanation
	}
	return rec, true
}

// finishOOMBumpFallback scales limits once from the bumped request and
// publishes gauges. The caller returns before the shared scale site.
func (r *AttunePolicyReconciler) finishOOMBumpFallback(
	in recommendContainerInput,
	planRec attunev1alpha1.ContainerRecommendation,
) attunev1alpha1.ContainerRecommendation {
	if in.policy != nil {
		scaleControlledLimits(in.policy, &planRec,
			planRec.Current.CPURequest, planRec.Current.CPULimit,
			planRec.Current.MemoryRequest, planRec.Current.MemoryLimit)
	}
	ns, workloadName := "", ""
	if in.policy != nil {
		ns = in.policy.Namespace
	}
	if in.workload != nil {
		workloadName = in.workload.GetName()
	}
	setRecommendationGauges(ns, workloadName, in.container.Name, &planRec)
	return planRec
}

// oomBumpRevertGate reports whether RevertPod must be skipped.
// A positive floor with suppress false means memory stops at that floor
// and CPU still reverts. The safety gate does not emit the capped metric.
func (r *AttunePolicyReconciler) oomBumpRevertGate(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	record safety.ResizeRecord,
	reason string,
	now time.Time,
) (safety.ResizeRecord, bool) {
	live := r.livePodForBumpGate(ctx, pod)
	if policy == nil || policy.Spec.Memory.OOMBump == nil {
		return record, false
	}
	decision := oomBumpRevertDecisionFor(live, record.Container, reason, record.ResizedAt, now, oomBumpMaxBumps(policy))
	if decision.Suppress {
		return record, true
	}
	if decision.Floor > 0 {
		adjusted := record
		adjusted.OriginalResources = raiseMemoryFloor(record.OriginalResources, decision.Floor)
		return adjusted, false
	}
	return record, false
}

// raiseMemoryFloor copies requirements and raises a lower memory request.
// A missing or zero limit stays unset. A positive limit below the floor is raised.
func raiseMemoryFloor(src corev1.ResourceRequirements, floorBytes int64) corev1.ResourceRequirements {
	out := src.DeepCopy()
	floor := resource.NewQuantity(floorBytes, resource.BinarySI)
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	cur := out.Requests[corev1.ResourceMemory]
	if cur.Cmp(*floor) < 0 {
		out.Requests[corev1.ResourceMemory] = floor.DeepCopy()
	}
	if out.Limits != nil {
		if lim, ok := out.Limits[corev1.ResourceMemory]; ok && !lim.IsZero() && lim.Cmp(*floor) < 0 {
			out.Limits[corev1.ResourceMemory] = floor.DeepCopy()
		}
	}
	return *out
}

// livePodForBumpGate reads the pod from the API server when a clientset is
// set, so a stale cache cannot revert an in-hold bump. Tests pass a nil
// clientset and the pod they built.
func (r *AttunePolicyReconciler) livePodForBumpGate(ctx context.Context, pod *corev1.Pod) *corev1.Pod {
	if pod == nil || r == nil || r.Clientset == nil {
		return pod
	}
	fresh, err := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil || fresh == nil {
		return pod
	}
	return fresh
}

func (r *AttunePolicyReconciler) peekOOMBump(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	container string,
	pod *corev1.Pod,
) (oomBumpPodStamp, bool) {
	if r == nil || r.oomBumps == nil || policy == nil || workload == nil || pod == nil {
		return oomBumpPodStamp{}, false
	}
	return r.oomBumps.PeekPod(
		string(policy.UID), policy.Namespace, policy.Name,
		workload.GetNamespace(), workload.GetName(), container,
		pod.Namespace, pod.Name,
	)
}

// dropOOMBumpStamp removes a real bump that must not increment count.
// An annotation-only stamp stays so a capped signal is written once.
// countSkipped emits skipped once for a real bump that was removed.
func (r *AttunePolicyReconciler) dropOOMBumpStamp(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	container, podNS, podName string,
	countSkipped bool,
) {
	if r == nil || r.oomBumps == nil || policy == nil {
		return
	}
	wns, wname := "", ""
	if workload != nil {
		wns = workload.GetNamespace()
		wname = workload.GetName()
	}
	uid := string(policy.UID)
	stamp, ok := r.oomBumps.PeekPod(uid, policy.Namespace, policy.Name, wns, wname, container, podNS, podName)
	if !ok || stamp.AnnotationOnly {
		return
	}
	if _, dropped := r.oomBumps.DropPod(uid, policy.Namespace, policy.Name, wns, wname, container, podNS, podName); !dropped {
		return
	}
	if countSkipped {
		operatormetrics.OOMBumpTotal.WithLabelValues(policy.Namespace, policy.Name, oomBumpSkipped).Inc()
	}
}

// persistAnnotationOnlyOOMBump writes the oom-bump key and nothing else.
// A conflict leaves the stamp unmarked so the next reconcile retries.
// A real bump whose live request is already at the target is left unmarked.
func (r *AttunePolicyReconciler) persistAnnotationOnlyOOMBump(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	workload client.Object,
	container string,
) {
	if r == nil || r.oomBumps == nil || r.Clientset == nil || policy == nil || pod == nil {
		return
	}
	stamp, ok := r.peekOOMBump(policy, workload, container, pod)
	if !ok || !stamp.AnnotationOnly {
		return
	}
	raw, err := formatOOMBumpRecord(stamp.Stamp)
	if err != nil {
		return
	}
	key, ok := oomBumpKey(container)
	if !ok {
		return
	}
	logger := log.FromContext(ctx)
	fresh, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if getErr != nil {
		logger.Error(getErr, "Failed to fetch pod for OOM bump timestamp",
			"pod", pod.Name, "container", container)
		return
	}
	if fresh.Annotations[key] == raw {
		*pod = *fresh
		r.markOOMBumpApplied(policy, workload, container, pod)
		return
	}
	fresh.Annotations = ensureAnnotations(fresh.Annotations)
	fresh.Annotations[key] = raw
	if updateErr := r.Update(ctx, fresh); updateErr != nil {
		logger.Info("OOM bump timestamp update did not land",
			"pod", pod.Name, "container", container, "error", updateErr.Error())
		return
	}
	*pod = *fresh
	r.markOOMBumpApplied(policy, workload, container, pod)
}

// persistPendingAnnotationOnlyOOMBumps writes capped and timestamp-fill
// stamps before selection and before a skip. A real bump stays in the book.
// A second call sees the stored raw and marks the stamp applied again.
func (r *AttunePolicyReconciler) persistPendingAnnotationOnlyOOMBumps(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
) {
	if r == nil || r.oomBumps == nil || policy == nil || workload == nil {
		return
	}
	pending := r.oomBumps.annotationOnlyStamps(
		string(policy.UID), policy.Namespace, policy.Name,
		workload.GetNamespace(), workload.GetName(),
	)
	for _, item := range pending {
		pod := &corev1.Pod{}
		pod.Namespace = item.stamp.Namespace
		pod.Name = item.stamp.PodName
		r.persistAnnotationOnlyOOMBump(ctx, policy, pod, workload, item.container)
	}
}

func (r *AttunePolicyReconciler) markOOMBumpApplied(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	container string,
	pod *corev1.Pod,
) {
	if r == nil || r.oomBumps == nil || policy == nil || workload == nil || pod == nil {
		return
	}
	r.oomBumps.MarkApplied(
		string(policy.UID), policy.Namespace, policy.Name,
		workload.GetNamespace(), workload.GetName(), container,
		pod.Namespace, pod.Name,
	)
}

// finishStoredOOMBump marks the stamp stored with the resize annotations.
// Annotation-only fills do not increment the metric. Clamped emits after
// the annotation is stored.
func (r *AttunePolicyReconciler) finishStoredOOMBump(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	container string,
	pod *corev1.Pod,
) {
	if r == nil || r.oomBumps == nil || policy == nil || pod == nil {
		return
	}
	stamp, ok := r.peekOOMBump(policy, workload, container, pod)
	if !ok {
		return
	}
	r.markOOMBumpApplied(policy, workload, container, pod)
	if !stamp.AnnotationOnly && r.oomBumps != nil {
		r.oomBumps.dropOlderClears(
			string(policy.UID), pod.Namespace, pod.Name, container,
			stamp.Stamp.OOMAt, stamp.Stamp.Restart,
		)
	}
	if stamp.AnnotationOnly {
		return
	}
	result := stamp.Result
	if result == "" {
		result = oomBumpApplied
	}
	operatormetrics.OOMBumpTotal.WithLabelValues(policy.Namespace, policy.Name, result).Inc()
	if stamp.Event != "" && r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, stamp.Event, "resize",
			"OOM bump for container %s on pod %s was clamped to maxAllowed", container, pod.Name)
	}
}

// storeAppliedOOMBumps writes the workload annotation from stamps that were
// stored plus the in-hold records from before this cycle. An empty value
// does not delete the existing annotation.
func (r *AttunePolicyReconciler) storeAppliedOOMBumps(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	rec attunev1alpha1.WorkloadRecommendation,
) {
	if r == nil || r.oomBumps == nil || policy == nil || policy.Spec.Memory.OOMBump == nil || workload == nil {
		return
	}
	now := r.now()
	logger := log.FromContext(ctx)
	for _, containerRec := range rec.Containers {
		applied, base := r.oomBumps.Applied(
			string(policy.UID), policy.Namespace, policy.Name,
			workload.GetNamespace(), workload.GetName(), containerRec.Name,
		)
		value := heldWorkloadValue(base, applied, now)
		if value == "" {
			continue
		}
		key, ok := oomBumpKey(containerRec.Name)
		if !ok {
			continue
		}
		if err := r.patchWorkloadOOMBump(ctx, workload, key, value); err != nil {
			logger.Error(err, "Failed to store OOM bump workload annotation",
				"workload", workload.GetName(), "container", containerRec.Name)
		}
	}
}

// patchWorkloadOOMBump merge-patches one annotation on a copy of the workload.
func (r *AttunePolicyReconciler) patchWorkloadOOMBump(ctx context.Context, workload client.Object, key, value string) error {
	if r == nil || r.Client == nil || workload == nil || key == "" || value == "" {
		return nil
	}
	baseObj := workload.DeepCopyObject()
	base, ok := baseObj.(client.Object)
	if !ok {
		return nil
	}
	patchObj := workload.DeepCopyObject()
	toPatch, ok := patchObj.(client.Object)
	if !ok {
		return nil
	}
	anns := toPatch.GetAnnotations()
	copied := make(map[string]string, len(anns)+1)
	for k, v := range anns {
		copied[k] = v
	}
	if copied[key] == value {
		return nil
	}
	copied[key] = value
	toPatch.SetAnnotations(copied)
	return r.Patch(ctx, toPatch, client.MergeFrom(base))
}

// preferPendingOOMBumpPods puts pods with a pending non-annotation bump first.
// No pending stamp returns the same slice.
func preferPendingOOMBumpPods(
	r *AttunePolicyReconciler,
	policy *attunev1alpha1.AttunePolicy,
	pods []corev1.Pod,
	rec attunev1alpha1.WorkloadRecommendation,
) []corev1.Pod {
	if r == nil || r.oomBumps == nil || policy == nil || policy.Spec.Memory.OOMBump == nil || len(pods) < 2 {
		return pods
	}
	preferred := make([]corev1.Pod, 0, len(pods))
	rest := make([]corev1.Pod, 0, len(pods))
	found := false
	for i := range pods {
		if podHasPendingOOMBump(r, policy, rec, &pods[i]) {
			preferred = append(preferred, pods[i])
			found = true
			continue
		}
		rest = append(rest, pods[i])
	}
	if !found {
		return pods
	}
	return append(preferred, rest...)
}

// staleOOMBumpRecommendation keeps containers whose OOM target is strictly
// above some pod's live memory request. The target is a pending bump's
// origin-math bytes, or the highest in-hold annotation floor when no new
// bump is pending. Recommended memory is rewritten to that target so a
// higher stale percentile is not applied. ok is false when nothing qualifies.
func (r *AttunePolicyReconciler) staleOOMBumpRecommendation(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	pods []corev1.Pod,
	rec attunev1alpha1.WorkloadRecommendation,
) (attunev1alpha1.WorkloadRecommendation, bool) {
	if r == nil || policy == nil || policy.Spec.Memory.OOMBump == nil || workload == nil {
		return rec, false
	}
	var kept []attunev1alpha1.ContainerRecommendation
	for _, c := range rec.Containers {
		target, ok := r.staleOOMBumpTarget(policy, workload, pods, c.Name)
		if !ok || !oomBumpAboveLiveMemory(pods, c.Name, target) {
			continue
		}
		c.Recommended.MemoryRequest = *resource.NewQuantity(target, resource.BinarySI)
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return rec, false
	}
	out := rec
	out.Containers = kept
	return out, true
}

// staleOOMBumpTarget prefers a non-annotation stamp's origin-math bytes.
// Otherwise it is the highest in-hold floor on the pods or the workload.
func (r *AttunePolicyReconciler) staleOOMBumpTarget(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	pods []corev1.Pod,
	container string,
) (int64, bool) {
	if bytes, ok := r.pendingOOMBumpBytes(policy, workload, container); ok {
		return bytes, true
	}
	return r.heldOOMBumpFloor(policy, workload, pods, container)
}

func (r *AttunePolicyReconciler) pendingOOMBumpBytes(policy *attunev1alpha1.AttunePolicy, workload client.Object, container string) (int64, bool) {
	if r == nil || r.oomBumps == nil || policy == nil || workload == nil {
		return 0, false
	}
	stamps := r.oomBumps.Stamps(
		string(policy.UID), policy.Namespace, policy.Name,
		workload.GetNamespace(), workload.GetName(), container,
	)
	var maxBytes int64
	found := false
	for _, stamp := range stamps {
		if stamp.AnnotationOnly || stamp.bumpBytes <= 0 {
			continue
		}
		if !found || stamp.bumpBytes > maxBytes {
			maxBytes = stamp.bumpBytes
			found = true
		}
	}
	return maxBytes, found
}

func (r *AttunePolicyReconciler) heldOOMBumpFloor(
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	pods []corev1.Pod,
	container string,
) (int64, bool) {
	key, ok := oomBumpKey(container)
	if !ok || policy == nil {
		return 0, false
	}
	workloadRaw := ""
	if workload != nil && workload.GetAnnotations() != nil {
		workloadRaw = workload.GetAnnotations()[key]
	}
	var clears []oomBumpClear
	if r != nil && r.oomBumps != nil {
		clears = r.oomBumps.Clears(string(policy.UID))
	}
	stripped, raw, _ := stripClearedOOMBumps(clears, container, pods, workloadRaw)
	now := time.Now()
	if r != nil {
		now = r.now()
	}
	var maxFloor int64
	found := false
	consider := func(value string) {
		rec, parsed := parseOOMBumpRecord(value)
		if !parsed || rec.Floor <= 0 || !now.Before(rec.HoldUntil) {
			return
		}
		if !found || rec.Floor > maxFloor {
			maxFloor = rec.Floor
			found = true
		}
	}
	for i := range stripped {
		if stripped[i].Annotations == nil {
			continue
		}
		consider(stripped[i].Annotations[key])
	}
	consider(raw)
	return maxFloor, found
}

// oomBumpAboveLiveMemory is true when target is strictly above at least one
// pod's live memory request. A missing request counts as zero.
func oomBumpAboveLiveMemory(pods []corev1.Pod, container string, target int64) bool {
	if target <= 0 {
		return false
	}
	want := resource.NewQuantity(target, resource.BinarySI)
	for i := range pods {
		c := findContainerByName(&pods[i], container)
		if c == nil {
			continue
		}
		var live resource.Quantity
		if c.Resources.Requests != nil {
			live = c.Resources.Requests[corev1.ResourceMemory]
		}
		if want.Cmp(live) > 0 {
			return true
		}
	}
	return false
}

// settleUnchangedOOMBump writes an annotation-only stamp when the request
// did not change. A real bump whose applied memory is not strictly above
// the live request is dropped and counted as skipped.
func (r *AttunePolicyReconciler) settleUnchangedOOMBump(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	pod *corev1.Pod,
	workload client.Object,
	container string,
	target corev1.ResourceRequirements,
) {
	r.persistAnnotationOnlyOOMBump(ctx, policy, pod, workload, container)
	if pod == nil || memoryTargetRaisesLive(pod, container, target) {
		return
	}
	stamp, ok := r.peekOOMBump(policy, workload, container, pod)
	if !ok || stamp.AnnotationOnly {
		return
	}
	r.dropOOMBumpStamp(policy, workload, container, pod.Namespace, pod.Name, true)
}

func memoryTargetRaisesLive(pod *corev1.Pod, container string, target corev1.ResourceRequirements) bool {
	var live resource.Quantity
	if c := findContainerByName(pod, container); c != nil && c.Resources.Requests != nil {
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			live = q
		}
	}
	var want resource.Quantity
	if target.Requests != nil {
		if q, ok := target.Requests[corev1.ResourceMemory]; ok {
			want = q
		}
	}
	return want.Cmp(live) > 0
}

func isPersistRevertReason(reason string) bool {
	switch reason {
	case "re-fetch-failed", "annotation-persist-failed", "annotation-persist-conflict":
		return true
	default:
		return false
	}
}

// oomBumpShouldClearAfterRevert is true when a non-OOM full revert must
// drop the bump record. Hold expiry, suppress, and a memory floor stay.
// A failed annotation persist reverts resources and keeps the record.
func oomBumpShouldClearAfterRevert(pod *corev1.Pod, container, reason string, resizedAt, now time.Time, maxBumps int) bool {
	if isPersistRevertReason(reason) || pod == nil {
		return false
	}
	decision := oomBumpRevertDecisionFor(pod, container, reason, resizedAt, now, maxBumps)
	if decision.Suppress || decision.Floor > 0 {
		return false
	}
	key, ok := oomBumpKey(container)
	if !ok || pod.Annotations == nil {
		return false
	}
	rec, parsed := parseOOMBumpRecord(pod.Annotations[key])
	return parsed && now.Before(rec.HoldUntil)
}

// clearOOMBumpAfterFullRevert deletes the pod bump key and remembers the
// consumed OOM so a later plan does not treat that signal as new.
// deleteWorkload removes the workload key when no other pod still has
// the same value. The resize path passes false: baseHeld rewrites the
// previous hold after this stamp is dropped.
func (r *AttunePolicyReconciler) clearOOMBumpAfterFullRevert(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	pod *corev1.Pod,
	container string,
	siblings []corev1.Pod,
	deleteWorkload bool,
) {
	if r == nil || policy == nil || pod == nil || policy.Spec.Memory.OOMBump == nil {
		return
	}
	key, ok := oomBumpKey(container)
	if !ok {
		return
	}
	raw := ""
	if pod.Annotations != nil {
		raw = pod.Annotations[key]
	}
	if r.oomBumps != nil && raw != "" {
		r.oomBumps.NoteClear(string(policy.UID), oomBumpClear{
			policyNamespace: policy.Namespace,
			policyName:      policy.Name,
			namespace:       pod.Namespace,
			podName:         pod.Name,
			container:       container,
			raw:             raw,
		})
	}
	logger := log.FromContext(ctx)
	if raw != "" {
		if err := r.patchDeleteAnnotation(ctx, pod, key); err != nil {
			logger.Error(err, "Failed to clear OOM bump annotation after revert",
				"pod", pod.Name, "container", container)
		}
	}
	r.dropOOMBumpStamp(policy, workload, container, pod.Namespace, pod.Name, false)
	if !deleteWorkload || workload == nil || raw == "" {
		return
	}
	current := ""
	if anns := workload.GetAnnotations(); anns != nil {
		current = anns[key]
	}
	if current != raw || siblingHoldsOOMBumpRaw(siblings, pod.Namespace, pod.Name, key, raw) {
		return
	}
	if err := r.patchDeleteAnnotation(ctx, workload, key); err != nil {
		logger.Error(err, "Failed to clear OOM bump workload annotation after revert",
			"workload", workload.GetName(), "container", container)
	}
}

// maybeClearOOMBumpAfterRevert drops the bump after a successful
// observation revert that did not keep a memory floor.
// The live API pod is the one the revert gate reads. A list pod that
// has not caught up still loses the key when that GET shows it.
// A list pod that still has the key is cleared when the live object
// does not.
func (r *AttunePolicyReconciler) maybeClearOOMBumpAfterRevert(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workloads []client.Object,
	pod *corev1.Pod,
	siblings []corev1.Pod,
	record safety.ResizeRecord,
	reason, trackedWorkload string,
) {
	if r == nil || pod == nil {
		return
	}
	now := r.now()
	maxBumps := oomBumpMaxBumps(policy)
	live := r.livePodForBumpGate(ctx, pod)
	target := pod
	if oomBumpShouldClearAfterRevert(live, record.Container, reason, record.ResizedAt, now, maxBumps) {
		target = live
	} else if live == pod || !oomBumpShouldClearAfterRevert(pod, record.Container, reason, record.ResizedAt, now, maxBumps) {
		return
	}
	r.clearOOMBumpAfterFullRevert(ctx, policy, workloadByName(workloads, pod.Namespace, trackedWorkload), target, record.Container, siblings, true)
}

func workloadByName(workloads []client.Object, namespace, name string) client.Object {
	for _, w := range workloads {
		if w != nil && w.GetNamespace() == namespace && w.GetName() == name {
			return w
		}
	}
	return nil
}

// patchDeleteAnnotation merge-patches one key to null on a copy so the
// informer object is not edited in place.
func (r *AttunePolicyReconciler) patchDeleteAnnotation(ctx context.Context, obj client.Object, key string) error {
	if r == nil || r.Client == nil || obj == nil || key == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{key: nil},
		},
	})
	if err != nil {
		return err
	}
	copied, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	return r.Patch(ctx, copied, client.RawPatch(types.MergePatchType, payload))
}

func podHasPendingOOMBump(
	r *AttunePolicyReconciler,
	policy *attunev1alpha1.AttunePolicy,
	rec attunev1alpha1.WorkloadRecommendation,
	pod *corev1.Pod,
) bool {
	for _, containerRec := range rec.Containers {
		stamp, ok := r.oomBumps.PeekPod(
			string(policy.UID), policy.Namespace, policy.Name,
			pod.Namespace, rec.Workload, containerRec.Name,
			pod.Namespace, pod.Name,
		)
		if ok && !stamp.AnnotationOnly {
			return true
		}
	}
	return false
}
