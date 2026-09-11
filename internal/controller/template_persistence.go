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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/safety"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

// templatePersistenceEnabled returns whether template writes are on.
func templatePersistenceEnabled(us *attunev1alpha1.UpdateStrategy) bool {
	if us == nil || us.TemplatePersistence == nil || us.TemplatePersistence.Enabled == nil {
		return false
	}
	return *us.TemplatePersistence.Enabled
}

// templatePersistenceWhen returns the effective trigger mode.
func templatePersistenceWhen(us *attunev1alpha1.UpdateStrategy) attunev1alpha1.TemplatePersistenceWhen {
	if us == nil || us.TemplatePersistence == nil {
		return attunev1alpha1.TemplatePersistenceAfterSuccessfulResize
	}
	if us.TemplatePersistence.When == "" {
		return attunev1alpha1.TemplatePersistenceAfterSuccessfulResize
	}
	return us.TemplatePersistence.When
}

// materializeContainerResources builds ResourceRequirements from a recommendation,
// honoring controlledValues and allowDecrease.
func materializeContainerResources(
	policy *attunev1alpha1.AttunePolicy,
	c attunev1alpha1.ContainerRecommendation,
) corev1.ResourceRequirements {
	reqs := corev1.ResourceList{}
	limits := corev1.ResourceList{}

	cpuAllowDec := true
	if policy.Spec.CPU.AllowDecrease != nil {
		cpuAllowDec = *policy.Spec.CPU.AllowDecrease
	}
	memAllowDec := false
	if policy.Spec.Memory.AllowDecrease != nil {
		memAllowDec = *policy.Spec.Memory.AllowDecrease
	}

	cpuReq := c.Recommended.CPURequest.DeepCopy()
	if !cpuAllowDec && cpuReq.Cmp(c.Current.CPURequest) < 0 {
		cpuReq = c.Current.CPURequest.DeepCopy()
	}
	memReq := c.Recommended.MemoryRequest.DeepCopy()
	if !memAllowDec && memReq.Cmp(c.Current.MemoryRequest) < 0 {
		memReq = c.Current.MemoryRequest.DeepCopy()
	}
	if !cpuReq.IsZero() {
		reqs[corev1.ResourceCPU] = cpuReq
	}
	if !memReq.IsZero() {
		reqs[corev1.ResourceMemory] = memReq
	}

	cpuCV := attunev1alpha1.DefaultControlledValues
	if policy.Spec.CPU.ControlledValues != nil {
		cpuCV = *policy.Spec.CPU.ControlledValues
	}
	memCV := attunev1alpha1.DefaultControlledValues
	if policy.Spec.Memory.ControlledValues != nil {
		memCV = *policy.Spec.Memory.ControlledValues
	}

	// Include recommended limits for clamp parity with buildResizeTarget.
	// Under RequestsOnly, Recommended.*Limit is the current limit (see
	// newContainerRecommendation); under RequestsAndLimits it is scaled.
	if !c.Recommended.CPULimit.IsZero() {
		limits[corev1.ResourceCPU] = c.Recommended.CPULimit.DeepCopy()
	}
	if !c.Recommended.MemoryLimit.IsZero() {
		limits[corev1.ResourceMemory] = c.Recommended.MemoryLimit.DeepCopy()
	}

	out := corev1.ResourceRequirements{Requests: reqs}
	if len(limits) > 0 {
		out.Limits = limits
	}
	// Match resize path: requests must not exceed limits when both are set.
	_ = resize.ClampRequestsToLimits(&out)

	if _, ok := out.Limits[corev1.ResourceMemory]; ok {
		if usage, hasUsage := recentMemoryUsage(c); hasUsage {
			margin := float64(attunev1alpha1.DefaultDecreaseUsageMarginPercent)
			if policy.Spec.Memory.DecreaseUsageMarginPercent != nil {
				margin = float64(*policy.Spec.Memory.DecreaseUsageMarginPercent)
			}
			// rec.Current may be the stale template after in-place resize.
			floored, applied := resize.FloorMemoryLimitAgainstStaleCurrent(
				out, c.Current.MemoryLimit, usage, margin)
			if applied {
				out = floored
			}
		}
	}

	// Only write limits into the template when ControlledValues says so.
	// RequestsOnly must leave existing template limits untouched (merge keeps them).
	if cpuCV != attunev1alpha1.ControlledRequestsAndLimits && memCV != attunev1alpha1.ControlledRequestsAndLimits {
		out.Limits = nil
	} else if out.Limits != nil {
		if cpuCV != attunev1alpha1.ControlledRequestsAndLimits {
			delete(out.Limits, corev1.ResourceCPU)
		}
		if memCV != attunev1alpha1.ControlledRequestsAndLimits {
			delete(out.Limits, corev1.ResourceMemory)
		}
		if len(out.Limits) == 0 {
			out.Limits = nil
		}
	}
	// After RequestsOnly strips limits, raise is a no-op. Guaranteed is
	// detected from current request==limit (no live pod QoS on a template).
	guaranteed := resize.CurrentResourcesAreGuaranteed(
		c.Current.CPURequest, c.Current.CPULimit,
		c.Current.MemoryRequest, c.Current.MemoryLimit)
	out = resize.RaiseMemoryRequestToLimitIfGuaranteed(out, guaranteed)
	return out
}

// canaryBlocksTemplatePersistence returns true while a canary rollout is
// still partial. Patching the template mid-canary would roll out all pods
// and defeat the canary gate (D6).
func canaryBlocksTemplatePersistence(policy *attunev1alpha1.AttunePolicy) bool {
	if policy.Spec.UpdateStrategy.Type != attunev1alpha1.UpdateTypeCanary {
		return false
	}
	cs := policy.Status.Canary
	if cs == nil {
		// Canary mode with no status yet: treat as in-progress.
		return true
	}
	return cs.Phase != attunev1alpha1.CanaryPhaseFullRollout
}

// resourcesEqual compares requests/limits for CPU and memory only.
func resourcesEqual(a, b corev1.ResourceRequirements) bool {
	return quantityEqual(a.Requests, b.Requests, corev1.ResourceCPU) &&
		quantityEqual(a.Requests, b.Requests, corev1.ResourceMemory) &&
		quantityEqual(a.Limits, b.Limits, corev1.ResourceCPU) &&
		quantityEqual(a.Limits, b.Limits, corev1.ResourceMemory)
}

func quantityEqual(a, b corev1.ResourceList, name corev1.ResourceName) bool {
	qa, oka := a[name]
	qb, okb := b[name]
	if !oka && !okb {
		return true
	}
	if oka != okb {
		// Treat missing as zero for comparison when one side has zero quantity.
		if oka && qa.IsZero() && !okb {
			return true
		}
		if okb && qb.IsZero() && !oka {
			return true
		}
		return false
	}
	return qa.Equal(qb)
}

// restoreTemplateAfterSafetyRevert writes the pre-resize snapshot back onto
// the Deployment/StatefulSet template after a successful live-pod revert.
// AfterSuccessfulResize already patched the unsafe rec before observation;
// without this restore, new pods and rollouts start at the size that just
// failed safety. Returns nil when persist is disabled, When is not
// AfterSuccessfulResize, the workload is missing, or the template is
// already at the snapshot. Callers must keep tracking annotations when
// the returned error is non-nil so the next reconcile retries.
func (r *AttunePolicyReconciler) restoreTemplateAfterSafetyRevert(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workloads []client.Object,
	record safety.ResizeRecord,
) error {
	logger := log.FromContext(ctx)
	if !templatePersistenceEnabled(policy.Spec.UpdateStrategy) {
		return nil
	}
	if templatePersistenceWhen(policy.Spec.UpdateStrategy) != attunev1alpha1.TemplatePersistenceAfterSuccessfulResize {
		return nil
	}

	var workload client.Object
	for _, w := range workloads {
		if w.GetName() != record.WorkloadName {
			continue
		}
		switch workloadKindName(w) {
		case "Deployment", "StatefulSet":
			workload = w
		}
		break
	}
	if workload == nil {
		return nil
	}

	desired := map[string]corev1.ResourceRequirements{
		record.Container: record.OriginalResources,
	}
	changed, err := r.patchWorkloadTemplateResources(ctx, workload, desired, true)
	if err != nil {
		return fmt.Errorf("restoring template after safety revert for %s/%s: %w",
			record.WorkloadName, record.Container, err)
	}
	if changed {
		logger.Info("Restored template after safety revert",
			"workload", record.WorkloadName, "container", record.Container)
	}
	return nil
}

// applyTemplatePersistence patches Deployment/StatefulSet pod templates for
// the given recommendations. mode must match the policy's configured when.
func (r *AttunePolicyReconciler) applyTemplatePersistence(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workloads []client.Object,
	recommendations []attunev1alpha1.WorkloadRecommendation,
	mode attunev1alpha1.TemplatePersistenceWhen,
	onlyWorkloads map[string]bool, // if non-nil, only these workload names
) []attunev1alpha1.ResizeHistoryEntry {
	logger := log.FromContext(ctx)
	if !templatePersistenceEnabled(policy.Spec.UpdateStrategy) {
		return nil
	}
	if templatePersistenceWhen(policy.Spec.UpdateStrategy) != mode {
		return nil
	}
	// Observe never mutates cluster state (status, export, template).
	if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeObserve {
		return nil
	}
	if canaryBlocksTemplatePersistence(policy) {
		logger.V(1).Info("Skipping template persistence during canary phase",
			"policy", policy.Name)
		return nil
	}
	if len(recommendations) == 0 {
		return nil
	}

	excludeSet := pkgdefaults.EffectiveExcludedContainers(policy)
	workloadMap := make(map[string]client.Object, len(workloads))
	for _, w := range workloads {
		workloadMap[w.GetName()] = w
	}

	var history []attunev1alpha1.ResizeHistoryEntry
	now := metav1.NewTime(r.now())

	for _, rec := range recommendations {
		if onlyWorkloads != nil && !onlyWorkloads[rec.Workload] {
			continue
		}
		if rec.Stale {
			logger.V(1).Info("Skipping template persistence for stale recommendation",
				"workload", rec.Workload)
			continue
		}
		w := workloadMap[rec.Workload]
		if w == nil {
			continue
		}
		kind := workloadKindName(w)
		if kind == "" {
			kind = rec.Kind
		}
		switch kind {
		case "Deployment", "StatefulSet":
		default:
			logger.V(1).Info("Template persistence skips unsupported kind",
				"workload", rec.Workload, "kind", kind)
			continue
		}
		if r.isRollingOut(w) {
			logger.Info("Skipping template persistence mid-rollout",
				"workload", rec.Workload)
			continue
		}

		// Build desired resources per container (skip excluded).
		// Compare want to the cached template, not rec.Current: after
		// in-place resize rec.Current can lag while the template is
		// already at the rec (or still needs a usage-floor write).
		cachedSpec := workloadPodSpec(w)
		desired := make(map[string]corev1.ResourceRequirements)
		for _, c := range rec.Containers {
			if excludeSet[c.Name] {
				continue
			}
			want := materializeContainerResources(policy, c)
			if recLim, ok := want.Limits[corev1.ResourceMemory]; ok &&
				!c.Recommended.MemoryLimit.IsZero() && recLim.Cmp(c.Recommended.MemoryLimit) > 0 {
				logger.V(1).Info("Template persist memory limit floored above usage",
					"workload", rec.Workload, "container", c.Name,
					"recommendedLimit", c.Recommended.MemoryLimit.String(),
					"flooredLimit", recLim.String())
			}
			if cachedSpec != nil {
				if cur, ok := templateContainerResources(cachedSpec, c.Name); ok {
					next, destClamped := mergeTemplateResources(cur, want)
					if resourcesEqual(cur, next) {
						continue
					}
					if len(destClamped) > 0 {
						logger.V(1).Info("Requests clamped to leftover template limits",
							"workload", rec.Workload, "container", c.Name,
							"clampedResources", destClamped)
						for _, res := range destClamped {
							operatormetrics.RequestClampedTotal.WithLabelValues(
								policy.Namespace, policy.Name, c.Name, res).Inc()
						}
					}
				}
			}
			desired[c.Name] = want
		}
		if len(desired) == 0 {
			logger.V(1).Info("Template persistence no-op: no container changes",
				"workload", rec.Workload)
			continue
		}

		changed, err := r.patchWorkloadTemplateResources(ctx, w, desired, false)
		if err != nil {
			logger.Error(err, "Failed to patch workload template",
				"workload", rec.Workload, "kind", kind)
			operatormetrics.TemplatePatchTotal.WithLabelValues(policy.Namespace, rec.Workload, "failed").Inc()
			r.emitEventOnce(policy, corev1.EventTypeWarning, "TemplatePatchFailed", "template",
				"Failed to patch template for %s/%s: %v", kind, rec.Workload, err)
			history = append(history, attunev1alpha1.ResizeHistoryEntry{
				Timestamp: now,
				Workload:  rec.Workload,
				Container: "*",
				Resource:  "template",
				From:      "workload-template",
				To:        "recommended",
				Method:    "TemplatePersistence",
				Result:    attunev1alpha1.ResizeResultFailed,
				Reason:    err.Error(),
			})
			continue
		}
		if !changed {
			logger.V(1).Info("Template persistence no-op: template already matches",
				"workload", rec.Workload)
			continue
		}
		operatormetrics.TemplatePatchTotal.WithLabelValues(policy.Namespace, rec.Workload, "success").Inc()
		r.emitEventOnce(policy, corev1.EventTypeNormal, "TemplatePatched", "template",
			"Patched pod template resources for %s/%s (%s)", kind, rec.Workload, mode)
		history = append(history, attunev1alpha1.ResizeHistoryEntry{
			Timestamp: now,
			Workload:  rec.Workload,
			Container: "*",
			Resource:  "template",
			From:      "workload-template",
			To:        "recommended",
			Method:    "TemplatePersistence",
			Result:    attunev1alpha1.ResizeResultTemplatePatched,
			Reason:    string(mode),
		})
	}
	return history
}

// patchWorkloadTemplateResources updates container resources on the pod template.
// When replace is true (safety restore), CPU/memory come from the snapshot and
// omitted CPU/memory limit keys are deleted; other resource keys stay.
// Persist keeps merge. Returns (changed, error).
func (r *AttunePolicyReconciler) patchWorkloadTemplateResources(
	ctx context.Context,
	workload client.Object,
	desired map[string]corev1.ResourceRequirements,
	replace bool,
) (bool, error) {
	var changed bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		key := client.ObjectKeyFromObject(workload)
		switch workload.(type) {
		case *appsv1.Deployment:
			var deploy appsv1.Deployment
			// Live API read: cache may hold strip-transformed objects; MergeFrom
			// on a stripped template would wipe container image/command.
			if err := r.liveReader().Get(ctx, key, &deploy); err != nil {
				return err
			}
			original := deploy.DeepCopy()
			if !applyResourcesToPodSpec(&deploy.Spec.Template.Spec, desired, replace) {
				return nil
			}
			changed = true
			return r.Patch(ctx, &deploy, client.MergeFrom(original))
		case *appsv1.StatefulSet:
			var sts appsv1.StatefulSet
			if err := r.liveReader().Get(ctx, key, &sts); err != nil {
				return err
			}
			original := sts.DeepCopy()
			if !applyResourcesToPodSpec(&sts.Spec.Template.Spec, desired, replace) {
				return nil
			}
			changed = true
			return r.Patch(ctx, &sts, client.MergeFrom(original))
		default:
			return fmt.Errorf("unsupported workload type %T", workload)
		}
	})
	return changed, err
}

// applyResourcesToPodSpec sets resources on matching containers and native sidecars.
// replace=true restores CPU/memory from want (extended keys stay);
// replace=false merges (persist). Returns true if any container was modified.
func applyResourcesToPodSpec(spec *corev1.PodSpec, desired map[string]corev1.ResourceRequirements, replace bool) bool {
	modified := false
	for i := range spec.Containers {
		c := &spec.Containers[i]
		want, ok := desired[c.Name]
		if !ok {
			continue
		}
		if applyContainerResources(c, want, replace) {
			modified = true
		}
	}
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		want, ok := desired[c.Name]
		if !ok {
			continue
		}
		if applyContainerResources(c, want, replace) {
			modified = true
		}
	}
	return modified
}

// applyContainerResources writes want onto a container. Persist merges so
// RequestsOnly (Limits=nil) keeps leftover template limits. Restore overwrites
// CPU/memory from want and deletes only those CPU/memory limit keys that want
// omits, so persist-added limits clear while extended resources stay.
func applyContainerResources(c *corev1.Container, want corev1.ResourceRequirements, replace bool) bool {
	var next corev1.ResourceRequirements
	if !replace {
		// Merge first so RequestsOnly (Limits=nil) does not treat leftover
		// template limits as a change when requests already match.
		next, _ = mergeTemplateResources(c.Resources, want)
	} else {
		next = replaceCPUMemoryResources(c.Resources, want)
	}
	if resourcesEqual(c.Resources, next) {
		return false
	}
	c.Resources = next
	return true
}

// replaceCPUMemoryResources copies current, then applies want's CPU and
// memory requests/limits. Keys in want overwrite. CPU/memory keys absent
// from want are deleted (clears persist-added limits). Every other resource
// key (GPU, hugepages, ephemeral-storage) is left untouched.
func replaceCPUMemoryResources(current, want corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := current.DeepCopy()
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if v, ok := want.Requests[res]; ok {
			out.Requests[res] = v.DeepCopy()
		} else {
			delete(out.Requests, res)
		}
	}
	if out.Limits == nil && len(want.Limits) > 0 {
		out.Limits = corev1.ResourceList{}
	}
	if out.Limits != nil {
		for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if v, ok := want.Limits[res]; ok {
				out.Limits[res] = v.DeepCopy()
			} else {
				delete(out.Limits, res)
			}
		}
		if len(out.Limits) == 0 {
			out.Limits = nil
		}
	}
	return *out
}

// mergeTemplateResources applies want requests/limits onto current, keeping
// existing limit entries when want does not set limits for that resource.
// Requests are then clamped so they do not exceed leftover destination limits.
// The returned names are resources whose requests were dest-clamped.
func mergeTemplateResources(current, want corev1.ResourceRequirements) (corev1.ResourceRequirements, []string) {
	out := current.DeepCopy()
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	for k, v := range want.Requests {
		out.Requests[k] = v.DeepCopy()
	}
	if len(want.Limits) > 0 {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		for k, v := range want.Limits {
			out.Limits[k] = v.DeepCopy()
		}
	}
	clamped := resize.ClampRequestsToLimits(out)
	return *out, clamped
}

func workloadPodSpec(w client.Object) *corev1.PodSpec {
	switch o := w.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template.Spec
	case *appsv1.StatefulSet:
		return &o.Spec.Template.Spec
	default:
		return nil
	}
}

func templateContainerResources(spec *corev1.PodSpec, name string) (corev1.ResourceRequirements, bool) {
	if spec == nil {
		return corev1.ResourceRequirements{}, false
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return spec.Containers[i].Resources, true
		}
	}
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		if c.Name != name {
			continue
		}
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			return c.Resources, true
		}
	}
	return corev1.ResourceRequirements{}, false
}

func workloadKindName(w client.Object) string {
	switch w.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *batchv1.Job:
		return "Job"
	case *batchv1.CronJob:
		return "CronJob"
	default:
		return w.GetObjectKind().GroupVersionKind().Kind
	}
}

// isSuccessfulResizeForPersist reports whether history should trigger
// AfterSuccessfulResize template persistence. Includes in-place Success
// and Eviction+Evicted (InPlaceOrRecreate) so replacement pods pick up
// the updated template.
func isSuccessfulResizeForPersist(h attunev1alpha1.ResizeHistoryEntry) bool {
	if isSuccessfulInPlaceHistory(h) {
		return true
	}
	return resizeHistoryMethod(h) == "Eviction" && h.Result == attunev1alpha1.ResizeResultEvicted
}

// omitRevertedOrFailedContainers drops containers whose latest
// persist-relevant history row is Reverted or Failed. AfterSuccessfulResize
// persist is keyed by workload; without this filter a Success on container A
// still writes container B's rec onto the template. History is oldest-first
// (appendHistory); walk newest last so a later Success can persist again.
// Skip Resource=="template" and Method==TemplatePersistence so a
// TemplatePatched row cannot hide a Reverted or Failed resize outcome.
func omitRevertedOrFailedContainers(
	recs []attunev1alpha1.WorkloadRecommendation,
	history []attunev1alpha1.ResizeHistoryEntry,
) []attunev1alpha1.WorkloadRecommendation {
	skip := make(map[string]bool)
	seen := make(map[string]bool)
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Resource == "template" || h.Method == "TemplatePersistence" {
			continue
		}
		if h.Workload == "" || h.Container == "" || h.Container == "*" {
			continue
		}
		key := h.Workload + "/" + h.Container
		if seen[key] {
			continue
		}
		seen[key] = true
		if h.Result == attunev1alpha1.ResizeResultReverted || h.Result == attunev1alpha1.ResizeResultFailed {
			skip[key] = true
		}
	}
	if len(skip) == 0 {
		return recs
	}
	out := make([]attunev1alpha1.WorkloadRecommendation, 0, len(recs))
	for _, rec := range recs {
		filtered := make([]attunev1alpha1.ContainerRecommendation, 0, len(rec.Containers))
		for _, c := range rec.Containers {
			if skip[rec.Workload+"/"+c.Name] {
				continue
			}
			filtered = append(filtered, c)
		}
		if len(filtered) == 0 {
			continue
		}
		rec.Containers = filtered
		out = append(out, rec)
	}
	return out
}

// omittedPersistContainerNames lists workload/container keys present in recs
// but dropped by omitRevertedOrFailedContainers.
func omittedPersistContainerNames(
	recs, filtered []attunev1alpha1.WorkloadRecommendation,
) []string {
	keep := make(map[string]bool)
	for _, rec := range filtered {
		for _, c := range rec.Containers {
			keep[rec.Workload+"/"+c.Name] = true
		}
	}
	var omitted []string
	for _, rec := range recs {
		for _, c := range rec.Containers {
			key := rec.Workload + "/" + c.Name
			if !keep[key] {
				omitted = append(omitted, key)
			}
		}
	}
	return omitted
}

// successfulResizeWorkloads returns workload names that had a successful
// in-place resize or eviction in the given history batch.
func successfulResizeWorkloads(history []attunev1alpha1.ResizeHistoryEntry) map[string]bool {
	out := make(map[string]bool)
	for _, h := range history {
		if isSuccessfulResizeForPersist(h) {
			out[h.Workload] = true
		}
	}
	return out
}

// laggingAfterResizeWorkloads returns workloads that should (re)try template
// persistence after a successful in-place resize or eviction. Includes
// this-cycle successes always. From status history, only includes a workload
// when its latest persist-trigger success is not followed by a
// TemplatePatched entry (failed patch, mid-rollout skip, or never
// attempted). This avoids turning AfterSuccessfulResize into permanent
// OnRecommendation after one success.
func laggingAfterResizeWorkloads(
	cycleHistory []attunev1alpha1.ResizeHistoryEntry,
	statusHistory []attunev1alpha1.ResizeHistoryEntry,
) map[string]bool {
	out := successfulResizeWorkloads(cycleHistory)

	type wlState struct {
		lastSuccess metav1.Time
		hasSuccess  bool
		lastPatched metav1.Time
		hasPatched  bool
	}
	byWL := map[string]*wlState{}
	for _, h := range statusHistory {
		st := byWL[h.Workload]
		if st == nil {
			st = &wlState{}
			byWL[h.Workload] = st
		}
		if isSuccessfulResizeForPersist(h) {
			if !st.hasSuccess || h.Timestamp.After(st.lastSuccess.Time) {
				st.hasSuccess = true
				st.lastSuccess = h.Timestamp
			}
		}
		if h.Method == "TemplatePersistence" && h.Result == attunev1alpha1.ResizeResultTemplatePatched {
			if !st.hasPatched || h.Timestamp.After(st.lastPatched.Time) {
				st.hasPatched = true
				st.lastPatched = h.Timestamp
			}
		}
	}
	for wl, st := range byWL {
		if !st.hasSuccess {
			continue
		}
		// Template already patched at or after the last successful resize.
		if st.hasPatched && !st.lastSuccess.After(st.lastPatched.Time) {
			continue
		}
		out[wl] = true
	}
	return out
}
