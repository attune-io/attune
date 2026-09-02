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
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/recommendation"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/safety"
)

const (
	// degradedWindowSize is the number of recent resize history entries
	// inspected when evaluating the Degraded condition.
	degradedWindowSize = 5
	// degradedRevertThreshold is the number of reverts in the window that
	// triggers the Degraded condition.
	degradedRevertThreshold = 3
	// maxBackoffDoublings caps exponential cooldown at 2^N x base.
	maxBackoffDoublings = 4
	// maxHistoryEntries is the maximum number of resize history entries
	// kept per policy. Must match the kubebuilder MaxItems marker on
	// AttunePolicyStatus.ResizeHistory.
	maxHistoryEntries = 50
)

// isResizeMode returns true if the policy mode performs actual pod resizes.
func isResizeMode(mode attunev1alpha1.UpdateType) bool {
	return mode == attunev1alpha1.UpdateTypeOneShot || mode == attunev1alpha1.UpdateTypeCanary || mode == attunev1alpha1.UpdateTypeAuto
}

// newHistoryEntry creates a ResizeHistoryEntry from a resize result.
func newHistoryEntry(now metav1.Time, workload, container string, res resize.ResizeResult, result attunev1alpha1.ResizeResult) attunev1alpha1.ResizeHistoryEntry {
	return attunev1alpha1.ResizeHistoryEntry{
		Timestamp: now,
		Workload:  workload,
		Container: container,
		Resource:  res.Resource,
		From:      res.From.String(),
		To:        res.To.String(),
		Method:    resize.MethodInPlace,
		Result:    result,
	}
}

// removeTrackingAnnotations removes the resize-tracking annotations from a pod.
func removeTrackingAnnotations(pod *corev1.Pod) {
	// Remove per-container annotations for each tracked container.
	if names, ok := pod.Annotations[annotationResizedContainers]; ok {
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			delete(pod.Annotations, annotationOriginalCPUPrefix+name)
			delete(pod.Annotations, annotationOriginalMemoryPrefix+name)
			delete(pod.Annotations, annotationOriginalCPULimitPrefix+name)
			delete(pod.Annotations, annotationOriginalMemoryLimitPrefix+name)
			delete(pod.Annotations, annotationOriginalRestartCountPrefix+name)
		}
	}
	delete(pod.Annotations, annotationResizedAt)
	delete(pod.Annotations, annotationResizedContainers)
	delete(pod.Annotations, annotationResizedWorkload)
	delete(pod.Annotations, annotationPolicy)
	delete(pod.Labels, labelTracked)
}

// appendResizedContainer adds a container name to the comma-separated
// resized-containers annotation, avoiding duplicates.
func appendResizedContainer(pod *corev1.Pod, containerName string) {
	existing := pod.Annotations[annotationResizedContainers]
	if existing == "" {
		pod.Annotations[annotationResizedContainers] = containerName
		return
	}
	for _, name := range strings.Split(existing, ",") {
		if strings.TrimSpace(name) == containerName {
			return
		}
	}
	pod.Annotations[annotationResizedContainers] = existing + "," + containerName
}

// setFailedCondition sets a Ready=False condition on the policy and updates
// the status subresource. Errors from the status update are logged but not
// returned, since the caller typically returns a requeue result regardless.
func (r *AttunePolicyReconciler) setFailedCondition(ctx context.Context, policy *attunev1alpha1.AttunePolicy, reason, message string) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}

	for attempt := range 3 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: policy.Generation,
		})
		err := r.Status().Update(ctx, policy)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Error(err, "Failed to update status")
			return
		}
		logger.Info("setFailedCondition conflict, retrying", "attempt", attempt+1)
		if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
			logger.Error(fetchErr, "Failed to re-fetch policy for status retry")
			return
		}
	}
	logger.Error(fmt.Errorf("exhausted retries"), "Failed to set failed condition after retries", "reason", reason)
}

// annotationSuppressWarnings is the annotation key for suppressing specific
// warning event reasons. Value is a comma-separated list of event reason strings
// (e.g., "HPAConflict,ConfigClamped,CooldownActive"). Suppressed warnings are
// still logged at V(1) but do not emit K8s events.
const annotationSuppressWarnings = "attune.io/suppress-warnings"

// isSuppressed returns true if the given event reason is listed in the
// attune.io/suppress-warnings annotation.
func isSuppressed(annotations map[string]string, reason string) bool {
	val, ok := annotations[annotationSuppressWarnings]
	if !ok || val == "" {
		return false
	}
	for _, suppressed := range strings.Split(val, ",") {
		if strings.TrimSpace(suppressed) == reason {
			return true
		}
	}
	return false
}

// eventDedup tracks which events have been emitted per policy to avoid flooding
// the K8s event stream on every reconcile. Events are suppressed until the TTL
// expires or the condition changes. The map is in-memory only; after operator
// restart events re-emit once (acceptable).
type eventDedup struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	ttl   time.Duration
	calls int
}

func newEventDedup(ttl time.Duration) *eventDedup {
	return &eventDedup{
		seen: make(map[string]time.Time),
		ttl:  ttl,
	}
}

// shouldEmit returns true if the event should be emitted (not recently seen).
// key should be "policyUID/reason" or "policyUID/reason/detail" for uniqueness.
// Every 1000 calls, expired entries are pruned to prevent unbounded map growth
// from messages containing unique identifiers (e.g., pod names).
func (d *eventDedup) shouldEmit(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls%1000 == 0 {
		now := time.Now()
		for k, t := range d.seen {
			if now.Sub(t) >= d.ttl {
				delete(d.seen, k)
			}
		}
	}
	if last, ok := d.seen[key]; ok && time.Since(last) < d.ttl {
		return false
	}
	d.seen[key] = time.Now()
	return true
}

// emitEventOnce emits a K8s event only if it hasn't been emitted recently
// (within the dedup TTL, default 1 hour). This prevents flooding the event
// stream when a condition persists across reconciles.
func (r *AttunePolicyReconciler) emitEventOnce(
	obj runtime.Object, eventType, reason, action, messageFmt string, args ...interface{},
) {
	if r.Recorder == nil {
		return
	}
	var uid string
	if accessor, ok := obj.(metav1.ObjectMetaAccessor); ok {
		uid = string(accessor.GetObjectMeta().GetUID())
		// Check if this warning reason is suppressed via annotation.
		if isSuppressed(accessor.GetObjectMeta().GetAnnotations(), reason) {
			return
		}
	}
	msg := fmt.Sprintf(messageFmt, args...)
	key := uid + "/" + reason + "/" + msg
	if !r.eventDedup.shouldEmit(key) {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, action, messageFmt, args...)
}

// warnConfigClamping emits K8s events for any config values that are silently
// clamped to valid ranges. Called once per reconcile, early, so users see
// feedback in kubectl get events instead of wondering why their values differ.
func (r *AttunePolicyReconciler) warnConfigClamping(policy *attunev1alpha1.AttunePolicy) {
	if hw := policy.Spec.MetricsSource.HistoryWindow; hw != nil {
		if hw.Duration < time.Hour {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"historyWindow %s clamped to 1h (minimum)", hw.Duration)
		} else if hw.Duration > 720*time.Hour {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"historyWindow %s clamped to 720h (maximum)", hw.Duration)
		}
	}
	if qs := policy.Spec.MetricsSource.QueryStep; qs != nil {
		if qs.Duration < 10*time.Second {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"queryStep %s clamped to 10s (minimum)", qs.Duration)
		} else if qs.Duration > time.Hour {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"queryStep %s clamped to 1h (maximum)", qs.Duration)
		}
	}
	if rw := policy.Spec.MetricsSource.RateWindow; rw != nil {
		if rw.Duration < 30*time.Second {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"rateWindow %s clamped to 30s (minimum)", rw.Duration)
		}
	}
	if policy.Spec.UpdateStrategy != nil && policy.Spec.UpdateStrategy.Cooldown != nil {
		cd := policy.Spec.UpdateStrategy.Cooldown
		minCooldown := r.MinCooldown
		if minCooldown == 0 {
			minCooldown = time.Minute
		}
		if cd.Duration > 0 && cd.Duration < minCooldown {
			r.emitEventOnce(policy, corev1.EventTypeWarning, "ConfigClamped", "config",
				"cooldown %s raised to operator minimum %s", cd.Duration, minCooldown)
		}
	}
}

// parseHistoryWindow parses the history window duration from the policy.
// Defense-in-depth: clamps to [1h, 720h] even if webhook validation is bypassed.
// When MaxHistoryWindow is set on the reconciler, also clamps to that ceiling
// (tier-aware operator defaults for large fleets).
func (r *AttunePolicyReconciler) parseHistoryWindow(policy *attunev1alpha1.AttunePolicy) time.Duration {
	var hw time.Duration
	if policy.Spec.MetricsSource.HistoryWindow != nil {
		hw = policy.Spec.MetricsSource.HistoryWindow.Duration
	} else {
		hw = defaultHistoryWindow
	}
	if hw < time.Hour {
		hw = time.Hour
	}
	if hw > 720*time.Hour {
		hw = 720 * time.Hour
	}
	if r != nil && r.MaxHistoryWindow > 0 && hw > r.MaxHistoryWindow {
		hw = r.MaxHistoryWindow
	}
	return hw
}

// getMinimumDataPoints returns the minimum data points threshold from the policy.
func (r *AttunePolicyReconciler) getMinimumDataPoints(policy *attunev1alpha1.AttunePolicy) int32 {
	if policy.Spec.MetricsSource.MinimumDataPoints != nil && *policy.Spec.MetricsSource.MinimumDataPoints > 0 {
		return *policy.Spec.MetricsSource.MinimumDataPoints
	}
	return defaultMinimumDataPoints
}

// recommendationFreshnessBound is how recent the newest finite sample must
// be for a recommendation to count as fresh. Three query steps covers a
// couple of missed scrapes without treating the whole historyWindow as current.
func recommendationFreshnessBound(queryStep time.Duration) time.Duration {
	if queryStep <= 0 {
		queryStep = attunev1alpha1.DefaultQueryStep
	}
	return 3 * queryStep
}

// getQueryStep returns the query step interval from the policy or the default (5m).
// When MinQueryStep is set on the reconciler, enforces that floor (tier-aware).
func (r *AttunePolicyReconciler) getQueryStep(policy *attunev1alpha1.AttunePolicy) time.Duration {
	var qs time.Duration
	if policy.Spec.MetricsSource.QueryStep != nil {
		qs = policy.Spec.MetricsSource.QueryStep.Duration
	} else {
		qs = attunev1alpha1.DefaultQueryStep
	}
	if qs < 10*time.Second {
		qs = 10 * time.Second
	}
	if qs > time.Hour {
		qs = time.Hour
	}
	if r != nil && r.MinQueryStep > 0 && qs < r.MinQueryStep {
		qs = r.MinQueryStep
	}
	return qs
}

// getRateWindow returns the rate window from the policy or falls back to queryStep.
func (r *AttunePolicyReconciler) getRateWindow(policy *attunev1alpha1.AttunePolicy) time.Duration {
	if policy.Spec.MetricsSource.RateWindow != nil {
		rw := policy.Spec.MetricsSource.RateWindow.Duration
		if rw < 30*time.Second {
			rw = 30 * time.Second
		}
		hw := r.parseHistoryWindow(policy)
		if rw > hw {
			rw = hw
		}
		return rw
	}
	return r.getQueryStep(policy)
}

// parseCooldown returns the cooldown duration from the policy's update strategy.
func (r *AttunePolicyReconciler) parseCooldown(policy *attunev1alpha1.AttunePolicy) time.Duration {
	if policy.Spec.UpdateStrategy != nil && policy.Spec.UpdateStrategy.Cooldown != nil {
		cd := policy.Spec.UpdateStrategy.Cooldown.Duration
		// Defense-in-depth: enforce minimum floor even if webhook validation is bypassed.
		minCooldown := r.MinCooldown
		if minCooldown == 0 {
			minCooldown = time.Minute
		}
		if cd > 0 && cd < minCooldown {
			cd = minCooldown
		}
		return cd
	}
	return defaultCooldown
}

// lastResizeAnnotationKey returns the per-workload last-resize annotation.
func lastResizeAnnotationKey(workload string) string {
	return lastResizeAnnotationPrefix + workload
}

// parseLastResizeTime parses an RFC3339 last-resize annotation.
// Malformed values are treated as no previous resize.
func parseLastResizeTime(ann map[string]string, key string) (time.Time, bool) {
	if ann == nil {
		return time.Time{}, false
	}
	lastStr, ok := ann[key]
	if !ok {
		return time.Time{}, false
	}
	last, err := time.Parse(time.RFC3339, lastStr)
	if err != nil {
		return time.Time{}, false
	}
	return last, true
}

// lastResizeTimeForWorkload returns the last resize time for one workload.
// Prefers attune.io/last-resize-time.<workload>, then the policy-wide
// attune.io/last-resize-time key (upgrade fallback).
func hasPerWorkloadResizeKeys(ann map[string]string) bool {
	for k := range ann {
		if strings.HasPrefix(k, lastResizeAnnotationPrefix) {
			return true
		}
	}
	return false
}

func lastResizeTimeForWorkload(policy *attunev1alpha1.AttunePolicy, workload string) (time.Time, bool) {
	ann := policy.Annotations
	if workload != "" && ann != nil {
		if _, exists := ann[lastResizeAnnotationKey(workload)]; exists {
			// Present but malformed: do not fall back to the policy-wide key.
			return parseLastResizeTime(ann, lastResizeAnnotationKey(workload))
		}
		// After the first per-app stamp, a missing key means this app
		// has never resized. Do not inherit the policy-wide last stamp.
		if hasPerWorkloadResizeKeys(ann) {
			return time.Time{}, false
		}
	}
	return parseLastResizeTime(ann, lastResizeAnnotation)
}

// isCooldownActive reports whether the policy-wide last-resize annotation
// is still inside the cooldown window (legacy single-workload / upgrade path).
func (r *AttunePolicyReconciler) isCooldownActive(policy *attunev1alpha1.AttunePolicy) bool {
	return r.isWorkloadCooldownActive(policy, "")
}

// isWorkloadCooldownActive reports whether one workload is still inside
// its cooldown (base duration times 2^N consecutive reverts for that app).
func (r *AttunePolicyReconciler) isWorkloadCooldownActive(policy *attunev1alpha1.AttunePolicy, workload string) bool {
	last, ok := lastResizeTimeForWorkload(policy, workload)
	if !ok {
		return false
	}
	return r.now().Sub(last) < r.getEffectiveCooldown(policy, workload)
}

// allWorkloadsCooling is true when every named workload is still cooling
// down. Empty input is false (nothing to block).
func (r *AttunePolicyReconciler) allWorkloadsCooling(policy *attunev1alpha1.AttunePolicy, workloads []string) bool {
	if len(workloads) == 0 {
		return false
	}
	for _, w := range workloads {
		if !r.isWorkloadCooldownActive(policy, w) {
			return false
		}
	}
	return true
}

// minCooldownRemaining is the soonest remaining cooldown among the named
// workloads. Expired or never-resized apps are ignored. Zero means nobody
// is still cooling (do not use that as RequeueAfter; it busy-loops).
func (r *AttunePolicyReconciler) minCooldownRemaining(policy *attunev1alpha1.AttunePolicy, workloads []string) time.Duration {
	var soonest time.Duration
	found := false
	now := r.now()
	for _, w := range workloads {
		last, ok := lastResizeTimeForWorkload(policy, w)
		if !ok {
			continue
		}
		rem := r.getEffectiveCooldown(policy, w) - now.Sub(last)
		if rem <= 0 {
			continue
		}
		if !found || rem < soonest {
			soonest = rem
			found = true
		}
	}
	return soonest
}

// getEffectiveCooldown returns the cooldown with exponential backoff applied
// based on consecutive reverts for the named workload (empty = whole history).
func (r *AttunePolicyReconciler) getEffectiveCooldown(policy *attunev1alpha1.AttunePolicy, workload string) time.Duration {
	base := r.parseCooldown(policy)
	reverts := consecutiveRevertsForWorkload(policy.Status.ResizeHistory, workload)
	if reverts == 0 {
		return base
	}
	if reverts > maxBackoffDoublings {
		reverts = maxBackoffDoublings
	}
	multiplier := 1 << reverts // 2^N
	return base * time.Duration(multiplier)
}

// setCooldownStatus populates the CooldownStatus summary. ConsecutiveReverts
// and backoff are the maximum across workloads so a hot app does not look
// like the whole policy is blocked.
func (r *AttunePolicyReconciler) setCooldownStatus(policy *attunev1alpha1.AttunePolicy) {
	base := r.parseCooldown(policy)
	reverts := maxConsecutiveReverts(policy.Status.ResizeHistory)
	capped := reverts
	if capped > maxBackoffDoublings {
		capped = maxBackoffDoublings
	}
	multiplier := int32(1 << capped) // 2^N
	effective := base * time.Duration(multiplier)
	policy.Status.Cooldown = &attunev1alpha1.CooldownStatus{
		EffectiveCooldown:  &metav1.Duration{Duration: effective},
		BackoffMultiplier:  multiplier,
		ConsecutiveReverts: safeInt32(reverts),
	}
}

// markResizeTime sets last-resize-time on the policy (policy-wide plus each
// named workload) using a merge patch to avoid 409 Conflict with spec updates.
func (r *AttunePolicyReconciler) markResizeTime(ctx context.Context, policy *attunev1alpha1.AttunePolicy, workloads ...string) error {
	patch := client.MergeFrom(policy.DeepCopy())
	if policy.Annotations == nil {
		policy.Annotations = make(map[string]string)
	}
	ts := r.now().UTC().Format(time.RFC3339)
	policy.Annotations[lastResizeAnnotation] = ts
	for _, w := range workloads {
		if w == "" {
			continue
		}
		policy.Annotations[lastResizeAnnotationKey(w)] = ts
	}
	return r.Patch(ctx, policy, patch)
}

// appendHistory appends new entries to existing history, capping at maxEntries.
func appendHistory(existing []attunev1alpha1.ResizeHistoryEntry,
	newEntries []attunev1alpha1.ResizeHistoryEntry, maxEntries int, //nolint:unparam // parameter kept for configurability
) []attunev1alpha1.ResizeHistoryEntry {
	result := append(existing, newEntries...)
	if len(result) > maxEntries {
		result = result[len(result)-maxEntries:]
	}
	return result
}

func resizeHistoryMethod(entry attunev1alpha1.ResizeHistoryEntry) string {
	if entry.Method != "" {
		return entry.Method
	}
	if entry.Result == attunev1alpha1.ResizeResultEvicted {
		return "Eviction"
	}
	return resize.MethodInPlace
}

func normalizeResizeHistoryMethods(history []attunev1alpha1.ResizeHistoryEntry) bool {
	changed := false
	for i := range history {
		method := resizeHistoryMethod(history[i])
		if method == history[i].Method {
			continue
		}
		history[i].Method = method
		changed = true
	}
	return changed
}

func isSuccessfulInPlaceHistory(entry attunev1alpha1.ResizeHistoryEntry) bool {
	return resizeHistoryMethod(entry) == resize.MethodInPlace && entry.Result == attunev1alpha1.ResizeResultSuccess
}

func removeSuccessfulInPlaceHistory(entries []attunev1alpha1.ResizeHistoryEntry) []attunev1alpha1.ResizeHistoryEntry {
	return slices.DeleteFunc(entries, isSuccessfulInPlaceHistory)
}

// setResizingCondition sets the Resizing condition based on current state.
func (r *AttunePolicyReconciler) setResizingCondition(policy *attunev1alpha1.AttunePolicy, cooldownActive bool) {
	if !isResizeMode(policy.Spec.UpdateStrategy.Type) {
		// Non-resize modes: clear the condition.
		meta.RemoveStatusCondition(&policy.Status.Conditions, attunev1alpha1.ConditionResizing)
		return
	}

	if policy.Status.Workloads.Resized > 0 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionResizing,
			Status:             metav1.ConditionTrue,
			Reason:             attunev1alpha1.ReasonInProgress,
			Message:            fmt.Sprintf("%d workload(s) resized this cycle", policy.Status.Workloads.Resized),
			ObservedGeneration: policy.Generation,
		})
	} else if cooldownActive {
		cooldownMsg := "Waiting for cooldown period to expire"
		if cs := policy.Status.Cooldown; cs != nil && cs.EffectiveCooldown != nil {
			if cs.BackoffMultiplier > 1 {
				cooldownMsg = fmt.Sprintf("Waiting for cooldown to expire (effective: %s, backoff: %dx)", cs.EffectiveCooldown.Duration, cs.BackoffMultiplier)
			} else {
				cooldownMsg = fmt.Sprintf("Waiting for cooldown to expire (effective: %s)", cs.EffectiveCooldown.Duration)
			}
		}
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionResizing,
			Status:             metav1.ConditionFalse,
			Reason:             attunev1alpha1.ReasonCooldownActive,
			Message:            cooldownMsg,
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionResizing,
			Status:             metav1.ConditionFalse,
			Reason:             attunev1alpha1.ReasonIdle,
			Message:            "No resizes needed",
			ObservedGeneration: policy.Generation,
		})
	}
}

// setScheduleBlockedCondition sets or removes the ScheduleBlocked condition
// based on whether a resize schedule is configured and whether the current
// time falls within the allowed window.
func (r *AttunePolicyReconciler) setScheduleBlockedCondition(policy *attunev1alpha1.AttunePolicy, withinWindow bool) {
	if policy.Spec.UpdateStrategy == nil || policy.Spec.UpdateStrategy.Schedule == nil || len(policy.Spec.UpdateStrategy.Schedule.Windows) == 0 {
		meta.RemoveStatusCondition(&policy.Status.Conditions, attunev1alpha1.ConditionScheduleBlocked)
		return
	}

	if !withinWindow {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionScheduleBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             attunev1alpha1.ReasonOutsideWindow,
			Message:            "Resizes deferred: current time is outside the configured schedule window",
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionScheduleBlocked,
			Status:             metav1.ConditionFalse,
			Reason:             attunev1alpha1.ReasonInsideWindow,
			Message:            "Current time is within the resize schedule window",
			ObservedGeneration: policy.Generation,
		})
	}
}

// maxBlockedPodNames is the max pod names listed in ResizeBlocked messages.
const maxBlockedPodNames = 5

// resizeBlockerSummary aggregates Deferred/Infeasible pods for status and metrics.
type resizeBlockerSummary struct {
	DeferredCount   int
	InfeasibleCount int
	DeferredNames   []string
	InfeasibleNames []string
	DeferredAges    []time.Duration
}

// summarizeResizeBlockers scans discovered pods for kubelet Deferred/Infeasible
// resize conditions. Sample names are capped for status message size.
func summarizeResizeBlockers(podsByWorkload map[string][]corev1.Pod, now time.Time) resizeBlockerSummary {
	var s resizeBlockerSummary
	seenDeferred := make(map[string]struct{})
	seenInfeasible := make(map[string]struct{})
	for _, pods := range podsByWorkload {
		for i := range pods {
			pod := &pods[i]
			key := pod.Namespace + "/" + pod.Name
			if resize.IsResizeDeferred(pod) {
				if _, ok := seenDeferred[key]; !ok {
					seenDeferred[key] = struct{}{}
					s.DeferredCount++
					if len(s.DeferredNames) < maxBlockedPodNames {
						s.DeferredNames = append(s.DeferredNames, pod.Name)
					}
					if since := resize.ResizeDeferredSince(pod); !since.IsZero() && now.After(since) {
						s.DeferredAges = append(s.DeferredAges, now.Sub(since))
					}
				}
			}
			if resize.IsResizeInfeasible(pod) {
				if _, ok := seenInfeasible[key]; !ok {
					seenInfeasible[key] = struct{}{}
					s.InfeasibleCount++
					if len(s.InfeasibleNames) < maxBlockedPodNames {
						s.InfeasibleNames = append(s.InfeasibleNames, pod.Name)
					}
				}
			}
		}
	}
	return s
}

// setResizeBlockedCondition sets ResizeBlocked when pods are Deferred or Infeasible.
func (r *AttunePolicyReconciler) setResizeBlockedCondition(policy *attunev1alpha1.AttunePolicy, summary resizeBlockerSummary) {
	if summary.DeferredCount == 0 && summary.InfeasibleCount == 0 {
		meta.RemoveStatusCondition(&policy.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
		return
	}

	var reason, message string
	switch {
	case summary.DeferredCount > 0 && summary.InfeasibleCount > 0:
		reason = attunev1alpha1.ReasonPodsDeferredAndInfeasible
		message = fmt.Sprintf(
			"%d deferred pod(s) (retry when kubelet clears Pending; e.g. %s); %d infeasible pod(s) (use InPlaceOrRecreate to evict, or free node capacity; e.g. %s)",
			summary.DeferredCount, joinSampleNames(summary.DeferredNames),
			summary.InfeasibleCount, joinSampleNames(summary.InfeasibleNames),
		)
	case summary.DeferredCount > 0:
		reason = attunev1alpha1.ReasonPodsDeferred
		message = fmt.Sprintf(
			"%d pod(s) have Deferred in-place resize; operator retries each reconcile once the condition clears (e.g. %s)",
			summary.DeferredCount, joinSampleNames(summary.DeferredNames),
		)
	default:
		reason = attunev1alpha1.ReasonPodsInfeasible
		message = fmt.Sprintf(
			"%d pod(s) have Infeasible in-place resize on their node; set resizeMethod: InPlaceOrRecreate for eviction fallback or free capacity (e.g. %s)",
			summary.InfeasibleCount, joinSampleNames(summary.InfeasibleNames),
		)
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               attunev1alpha1.ConditionResizeBlocked,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
}

func joinSampleNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// setDegradedCondition checks recent resize history for high revert rates.
// If 3+ of the last 5 history entries are reverted, the condition is set.
func (r *AttunePolicyReconciler) setDegradedCondition(policy *attunev1alpha1.AttunePolicy) {
	history := policy.Status.ResizeHistory
	if len(history) == 0 {
		meta.RemoveStatusCondition(&policy.Status.Conditions, attunev1alpha1.ConditionDegraded)
		return
	}

	window := degradedWindowSize
	if len(history) < window {
		window = len(history)
	}
	recent := history[len(history)-window:]
	reverts := 0
	for _, entry := range recent {
		if entry.Result == attunev1alpha1.ResizeResultReverted {
			reverts++
		}
	}

	if reverts >= degradedRevertThreshold {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               attunev1alpha1.ConditionDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             attunev1alpha1.ReasonHighRevertRate,
			Message:            fmt.Sprintf("%d of last %d resizes were reverted; consider adjusting overheads", reverts, window),
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&policy.Status.Conditions, attunev1alpha1.ConditionDegraded)
	}
}

// checkQuotaCompatibility verifies that the target resources don't violate
// LimitRange or ResourceQuota constraints in the namespace.
func (r *AttunePolicyReconciler) checkQuotaCompatibility(ctx context.Context, namespace string, currentResources, target corev1.ResourceRequirements) error {
	logger := log.FromContext(ctx)
	requestIncrease := resourcesIncreaseRequests(currentResources, target)

	// Check LimitRange per-container min/max.
	var limitRangeList corev1.LimitRangeList
	if err := r.List(ctx, &limitRangeList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list LimitRanges", "error", err)
		if requestIncrease {
			return fmt.Errorf("LimitRange list unavailable; skipping request increase")
		}
	}

	var quotaList corev1.ResourceQuotaList
	if err := r.List(ctx, &quotaList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list ResourceQuotas", "error", err)
		if requestIncrease {
			return fmt.Errorf("ResourceQuota list unavailable; skipping request increase")
		}
	}

	return checkQuotaCompatibilityFromLists(limitRangeList.Items, quotaList.Items, currentResources, target)
}

// checkQuotaCompatibilityFromLists validates that the target resources respect
// pre-fetched LimitRange and ResourceQuota constraints. This avoids redundant
// API calls when multiple pods are checked in the same namespace.
func checkQuotaCompatibilityFromLists(limitRanges []corev1.LimitRange, quotas []corev1.ResourceQuota, currentResources, target corev1.ResourceRequirements) error {
	for _, lr := range limitRanges {
		for _, item := range lr.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer {
				continue
			}
			if minCPU, ok := item.Min[corev1.ResourceCPU]; ok {
				if target.Requests.Cpu().Cmp(minCPU) < 0 {
					return fmt.Errorf("CPU request %s below LimitRange minimum %s", target.Requests.Cpu().String(), minCPU.String())
				}
			}
			if minMem, ok := item.Min[corev1.ResourceMemory]; ok {
				if target.Requests.Memory().Cmp(minMem) < 0 {
					return fmt.Errorf("memory request %s below LimitRange minimum %s", target.Requests.Memory().String(), minMem.String())
				}
			}
			if maxCPU, ok := item.Max[corev1.ResourceCPU]; ok {
				if target.Requests.Cpu().Cmp(maxCPU) > 0 {
					return fmt.Errorf("CPU request %s exceeds LimitRange maximum %s", target.Requests.Cpu().String(), maxCPU.String())
				}
			}
			if maxMem, ok := item.Max[corev1.ResourceMemory]; ok {
				if target.Requests.Memory().Cmp(maxMem) > 0 {
					return fmt.Errorf("memory request %s exceeds LimitRange maximum %s", target.Requests.Memory().String(), maxMem.String())
				}
			}
			// Also validate limits against LimitRange maximums. When
			// ControlledValues=RequestsAndLimits the controller scales
			// limits proportionally, which can exceed LimitRange bounds.
			if target.Limits != nil {
				if maxCPU, ok := item.Max[corev1.ResourceCPU]; ok {
					if limCPU := target.Limits.Cpu(); limCPU != nil && limCPU.Cmp(maxCPU) > 0 {
						return fmt.Errorf("CPU limit %s exceeds LimitRange maximum %s", limCPU.String(), maxCPU.String())
					}
				}
				if maxMem, ok := item.Max[corev1.ResourceMemory]; ok {
					if limMem := target.Limits.Memory(); limMem != nil && limMem.Cmp(maxMem) > 0 {
						return fmt.Errorf("memory limit %s exceeds LimitRange maximum %s", limMem.String(), maxMem.String())
					}
				}
			}
		}
	}

	for _, quota := range quotas {
		if err := checkQuotaHeadroom(quota, currentResources, target); err != nil {
			return err
		}
	}

	return nil
}

// checkQuotaHeadroom verifies that the increase from current to target
// resources fits within the remaining headroom of a ResourceQuota.
func checkQuotaHeadroom(quota corev1.ResourceQuota, current, target corev1.ResourceRequirements) error {
	cpuDelta := target.Requests.Cpu().MilliValue() - current.Requests.Cpu().MilliValue()
	memDelta := target.Requests.Memory().Value() - current.Requests.Memory().Value()

	if cpuDelta > 0 {
		if hardCPU, usedCPU, ok := quotaHardUsed(quota, corev1.ResourceRequestsCPU, corev1.ResourceCPU); ok {
			headroom := hardCPU.MilliValue() - usedCPU.MilliValue()
			if cpuDelta > headroom {
				return fmt.Errorf("CPU increase of %dm would exceed ResourceQuota %s (headroom: %dm)",
					cpuDelta, quota.Name, headroom)
			}
		}
	}

	if memDelta > 0 {
		if hardMem, usedMem, ok := quotaHardUsed(quota, corev1.ResourceRequestsMemory, corev1.ResourceMemory); ok {
			headroom := hardMem.Value() - usedMem.Value()
			if memDelta > headroom {
				return fmt.Errorf("memory increase of %s would exceed ResourceQuota %s (headroom: %s)",
					resource.NewQuantity(memDelta, resource.BinarySI).String(),
					quota.Name,
					resource.NewQuantity(headroom, resource.BinarySI).String())
			}
		}
	}

	if target.Limits != nil {
		cpuLimDelta := target.Limits.Cpu().MilliValue() - current.Limits.Cpu().MilliValue()
		memLimDelta := target.Limits.Memory().Value() - current.Limits.Memory().Value()
		if cpuLimDelta > 0 {
			hardCPU, hasHard := quota.Status.Hard[corev1.ResourceLimitsCPU]
			usedCPU, hasUsed := quota.Status.Used[corev1.ResourceLimitsCPU]
			if hasHard && hasUsed {
				headroom := hardCPU.MilliValue() - usedCPU.MilliValue()
				if cpuLimDelta > headroom {
					return fmt.Errorf("CPU limit increase of %dm would exceed ResourceQuota %s (headroom: %dm)",
						cpuLimDelta, quota.Name, headroom)
				}
			}
		}
		if memLimDelta > 0 {
			hardMem, hasHard := quota.Status.Hard[corev1.ResourceLimitsMemory]
			usedMem, hasUsed := quota.Status.Used[corev1.ResourceLimitsMemory]
			if hasHard && hasUsed {
				headroom := hardMem.Value() - usedMem.Value()
				if memLimDelta > headroom {
					return fmt.Errorf("memory limit increase of %s would exceed ResourceQuota %s (headroom: %s)",
						resource.NewQuantity(memLimDelta, resource.BinarySI).String(),
						quota.Name,
						resource.NewQuantity(headroom, resource.BinarySI).String())
				}
			}
		}
	}

	return nil
}

// quotaHardUsed resolves a ResourceQuota hard/used pair, preferring the
// requests.* name and falling back to the cpu/memory alias.
func quotaHardUsed(quota corev1.ResourceQuota, preferred, alias corev1.ResourceName) (hard, used resource.Quantity, ok bool) {
	hard, hasHard := quota.Status.Hard[preferred]
	if !hasHard {
		hard, hasHard = quota.Status.Hard[alias]
	}
	used, hasUsed := quota.Status.Used[preferred]
	if !hasUsed {
		used, hasUsed = quota.Status.Used[alias]
	}
	return hard, used, hasHard && hasUsed
}

// resourcesIncreaseRequests reports whether target raises CPU or memory
// requests relative to current.
func resourcesIncreaseRequests(current, target corev1.ResourceRequirements) bool {
	if target.Requests.Cpu().MilliValue() > current.Requests.Cpu().MilliValue() {
		return true
	}
	return target.Requests.Memory().Value() > current.Requests.Memory().Value()
}

// markLatestCycleReverted marks history entries from the most recent resize
// cycle as reverted. It walks the history in reverse and marks all matching
// entries whose timestamp equals the first (newest) match. Entries from
// earlier resize cycles (different timestamps) are left untouched, preventing
// consecutiveReverts from being inflated by a single revert event.
func markLatestCycleReverted(history []attunev1alpha1.ResizeHistoryEntry, workload, container, reason string) {
	var matched bool
	var matchTS time.Time
	for i := len(history) - 1; i >= 0; i-- {
		h := &history[i]
		if h.Workload == workload && h.Container == container && h.Result == attunev1alpha1.ResizeResultSuccess {
			if !matched {
				matchTS = h.Timestamp.Time
				matched = true
			}
			if h.Timestamp.Time.Equal(matchTS) {
				h.Result = attunev1alpha1.ResizeResultReverted
				h.Reason = reason
			} else {
				break // older cycle
			}
		} else if matched {
			break
		}
	}
}

// consecutiveReverts returns the number of consecutive reverted entries at the
// end of the resize history (policy-wide, for tests and degraded-rate helpers).
func consecutiveReverts(history []attunev1alpha1.ResizeHistoryEntry) int {
	return consecutiveRevertsForWorkload(history, "")
}

// consecutiveRevertsForWorkload counts trailing Reverted rows for one
// workload, skipping other apps. Empty workload uses the whole history.
func consecutiveRevertsForWorkload(history []attunev1alpha1.ResizeHistoryEntry, workload string) int {
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		if workload != "" && history[i].Workload != workload {
			continue
		}
		if history[i].Result == attunev1alpha1.ResizeResultReverted {
			count++
			continue
		}
		break
	}
	return count
}

// maxConsecutiveReverts is the highest per-workload trailing revert streak.
func maxConsecutiveReverts(history []attunev1alpha1.ResizeHistoryEntry) int {
	if len(history) == 0 {
		return 0
	}
	seen := map[string]struct{}{}
	max := 0
	for i := range history {
		w := history[i].Workload
		if _, ok := seen[w]; ok {
			continue
		}
		seen[w] = struct{}{}
		if n := consecutiveRevertsForWorkload(history, w); n > max {
			max = n
		}
	}
	return max
}

// updateStatusWithRetry performs a status update with up to 4 attempts
// (3 retries + 1 final) on conflict. On each conflict it re-fetches the
// policy and re-applies the saved status fields, preserving the higher
// Resized count from concurrent reconciles.
func (r *AttunePolicyReconciler) updateStatusWithRetry(ctx context.Context, policy *attunev1alpha1.AttunePolicy, key types.NamespacedName) error {
	const maxRetries = 3
	logger := log.FromContext(ctx)

	for attempt := range maxRetries {
		err := r.Status().Update(ctx, policy)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}

		// Conflict: re-fetch and retry, preserving the higher Resized count.
		// A concurrent reconcile may have already set Resized > 0; we must not
		// overwrite it with 0 from our stale snapshot.
		logger.Info("Status update conflict, retrying", "attempt", attempt+1, "maxRetries", maxRetries)
		savedStatus := policy.Status.DeepCopy()
		if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
			return fetchErr
		}
		fetchedResized := policy.Status.Workloads.Resized
		policy.Status = *savedStatus
		if fetchedResized > policy.Status.Workloads.Resized {
			policy.Status.Workloads.Resized = fetchedResized
		}
	}
	return r.Status().Update(ctx, policy)
}

// newSafetyMonitor creates a safety.Monitor with optional throttle checking
// if the metrics collector supports it, and optional SLO guardrail checking
// if the policy has SLO guardrails configured.
func (r *AttunePolicyReconciler) newSafetyMonitor(logger logr.Logger, collector rsmetrics.MetricsCollector, guardrails ...[]attunev1alpha1.SLOGuardrail) *safety.Monitor {
	monitor := safety.NewMonitor(r.Clientset, logger)
	if tc, ok := collector.(safety.ThrottleChecker); ok {
		// RateLimitedCollector always satisfies ThrottleChecker, but the
		// inner collector may not support throttle queries (e.g. Datadog,
		// CloudWatch). Check before registering to avoid false safety
		// clearance from the silent 0.0 return.
		register := true
		if rl, ok := collector.(*rsmetrics.RateLimitedCollector); ok && !rl.SupportsThrottle() {
			register = false
			logger.V(1).Info("Throttle safety check disabled: metrics backend does not support CPU throttle queries")
		}
		if register {
			monitor.WithThrottleChecker(tc, safety.DefaultThrottleThreshold)
		}
	}
	if len(guardrails) > 0 && len(guardrails[0]) > 0 {
		if sq, ok := collector.(safety.SLOQuerier); ok {
			monitor.WithSLOChecker(sq, guardrails[0])
		}
	}
	return monitor
}

// appendUnique appends value to the slice if it is not already present.
func appendUnique(slice []string, value string) []string {
	if slices.Contains(slice, value) {
		return slice
	}
	return append(slice, value)
}

// autoRevertEnabled returns true when the policy's AutoRevert setting is nil
// (defaulting to true) or explicitly set to true.
func autoRevertEnabled(s *attunev1alpha1.UpdateStrategy) bool {
	return s == nil || s.AutoRevert == nil || *s.AutoRevert
}

// getObservationPeriod returns the safety observation period using the
// precedence: safetyObservationPeriod > canary.observationPeriod > default (5m).
func getObservationPeriod(policy *attunev1alpha1.AttunePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.SafetyObservationPeriod != nil && policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration
	}
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
	}
	return defaultObservationPeriod
}

// scaleControlledLimits scales CPU and memory limits proportionally when the
// policy's ControlledValues is RequestsAndLimits. This avoids duplicating the
// ControlledValues resolution logic across prometheus.go and vpa.go.
func scaleControlledLimits(policy *attunev1alpha1.AttunePolicy, rec *attunev1alpha1.ContainerRecommendation, currentCPUReq, currentCPULim, currentMemReq, currentMemLim resource.Quantity) {
	cpuControlled := attunev1alpha1.ControlledRequestsOnly
	if policy.Spec.CPU.ControlledValues != nil {
		cpuControlled = *policy.Spec.CPU.ControlledValues
	}
	memControlled := attunev1alpha1.ControlledRequestsOnly
	if policy.Spec.Memory.ControlledValues != nil {
		memControlled = *policy.Spec.Memory.ControlledValues
	}
	if cpuControlled == attunev1alpha1.ControlledRequestsAndLimits {
		rec.Recommended.CPULimit = scaleLimits(currentCPUReq, currentCPULim, rec.Recommended.CPURequest)
	}
	if memControlled == attunev1alpha1.ControlledRequestsAndLimits {
		rec.Recommended.MemoryLimit = scaleLimits(currentMemReq, currentMemLim, rec.Recommended.MemoryRequest)
	}
}

// newContainerRecommendation initializes a ContainerRecommendation with current
// resources copied into both Current and Recommended. The Recommended values
// serve as defaults that are overwritten by the CPU/memory engine outputs.
// This is shared between the Prometheus and VPA recommendation paths.
func newContainerRecommendation(container corev1.Container, dataPoints int32, confidence float64, now time.Time) attunev1alpha1.ContainerRecommendation {
	currentCPUReq := container.Resources.Requests.Cpu().DeepCopy()
	currentCPULim := container.Resources.Limits.Cpu().DeepCopy()
	currentMemReq := container.Resources.Requests.Memory().DeepCopy()
	currentMemLim := container.Resources.Limits.Memory().DeepCopy()

	return attunev1alpha1.ContainerRecommendation{
		Name:       container.Name,
		DataPoints: dataPoints,
		Confidence: confidence,
		LastUpdated: metav1.Time{
			Time: now,
		},
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    currentCPUReq,
			CPULimit:      currentCPULim,
			MemoryRequest: currentMemReq,
			MemoryLimit:   currentMemLim,
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    currentCPUReq.DeepCopy(),
			CPULimit:      currentCPULim.DeepCopy(),
			MemoryRequest: currentMemReq.DeepCopy(),
			MemoryLimit:   currentMemLim.DeepCopy(),
		},
	}
}

// setRecommendationGauges updates Prometheus gauges for the recommendation.
func setRecommendationGauges(namespace, workloadName, containerName string, rec *attunev1alpha1.ContainerRecommendation) {
	operatormetrics.RecommendationCPU.WithLabelValues(namespace, workloadName, containerName).Set(float64(rec.Recommended.CPURequest.MilliValue()) / 1000.0)
	operatormetrics.RecommendationMemory.WithLabelValues(namespace, workloadName, containerName).Set(float64(rec.Recommended.MemoryRequest.Value()))
	operatormetrics.Confidence.WithLabelValues(namespace, workloadName, containerName).Set(rec.Confidence)
}

// enforceAllowDecrease clamps a recommendation to the current value when
// decreases are not allowed. It emits a DecreaseSuppressed event and
// updates the explanation. Returns the (possibly clamped) recommendation.
func (r *AttunePolicyReconciler) enforceAllowDecrease(
	allowDecrease bool,
	rec resource.Quantity,
	current resource.Quantity,
	explain *recommendation.RecommendationExplanation,
	policy *attunev1alpha1.AttunePolicy,
	containerName string,
	resourceType string,
) resource.Quantity {
	if allowDecrease || rec.Cmp(current) >= 0 {
		return rec
	}
	unclamped := rec.String()
	if r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "DecreaseSuppressed", "recommend",
			"%s decrease blocked by allowDecrease=false for container %s (current: %s)",
			resourceType, containerName, current.String())
	}
	clamped := current.DeepCopy()
	explain.Final = clamped.DeepCopy()
	explain.FinalAdjustment = fmt.Sprintf("%s decrease from %s to %s blocked by allowDecrease=false",
		resourceType, current.String(), unclamped)
	return clamped
}
