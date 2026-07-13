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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
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

	if cpuCV == attunev1alpha1.ControlledRequestsAndLimits && !c.Recommended.CPULimit.IsZero() {
		limits[corev1.ResourceCPU] = c.Recommended.CPULimit.DeepCopy()
	}
	if memCV == attunev1alpha1.ControlledRequestsAndLimits && !c.Recommended.MemoryLimit.IsZero() {
		limits[corev1.ResourceMemory] = c.Recommended.MemoryLimit.DeepCopy()
	}

	out := corev1.ResourceRequirements{Requests: reqs}
	if len(limits) > 0 {
		out.Limits = limits
	}
	// Match resize path: requests must not exceed limits when both are set.
	_ = clampRequestsToLimits(&out)
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
		desired := make(map[string]corev1.ResourceRequirements)
		for _, c := range rec.Containers {
			if excludeSet[c.Name] {
				continue
			}
			// Skip if recommendation equals current (no real change).
			if c.Recommended.CPURequest.Equal(c.Current.CPURequest) &&
				c.Recommended.MemoryRequest.Equal(c.Current.MemoryRequest) &&
				c.Recommended.CPULimit.Equal(c.Current.CPULimit) &&
				c.Recommended.MemoryLimit.Equal(c.Current.MemoryLimit) {
				continue
			}
			desired[c.Name] = materializeContainerResources(policy, c)
		}
		if len(desired) == 0 {
			logger.V(1).Info("Template persistence no-op: no container changes",
				"workload", rec.Workload)
			continue
		}

		changed, err := r.patchWorkloadTemplateResources(ctx, w, desired)
		if err != nil {
			logger.Error(err, "Failed to patch workload template",
				"workload", rec.Workload, "kind", kind)
			operatormetrics.TemplatePatchTotal.WithLabelValues(policy.Namespace, rec.Workload, "failed").Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "TemplatePatchFailed", "template",
					"Failed to patch template for %s/%s: %v", kind, rec.Workload, err)
			}
			history = append(history, attunev1alpha1.ResizeHistoryEntry{
				Timestamp: now,
				Workload:  rec.Workload,
				Container: "*",
				Resource:  "template",
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
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "TemplatePatched", "template",
				"Patched pod template resources for %s/%s (%s)", kind, rec.Workload, mode)
		}
		history = append(history, attunev1alpha1.ResizeHistoryEntry{
			Timestamp: now,
			Workload:  rec.Workload,
			Container: "*",
			Resource:  "template",
			Method:    "TemplatePersistence",
			Result:    attunev1alpha1.ResizeResultTemplatePatched,
			Reason:    string(mode),
		})
	}
	return history
}

// patchWorkloadTemplateResources updates container resources on the pod template.
// Returns (changed, error).
func (r *AttunePolicyReconciler) patchWorkloadTemplateResources(
	ctx context.Context,
	workload client.Object,
	desired map[string]corev1.ResourceRequirements,
) (bool, error) {
	var changed bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		key := client.ObjectKeyFromObject(workload)
		switch workload.(type) {
		case *appsv1.Deployment:
			var deploy appsv1.Deployment
			if err := r.Get(ctx, key, &deploy); err != nil {
				return err
			}
			original := deploy.DeepCopy()
			if !applyResourcesToPodSpec(&deploy.Spec.Template.Spec, desired) {
				return nil
			}
			changed = true
			return r.Patch(ctx, &deploy, client.MergeFrom(original))
		case *appsv1.StatefulSet:
			var sts appsv1.StatefulSet
			if err := r.Get(ctx, key, &sts); err != nil {
				return err
			}
			original := sts.DeepCopy()
			if !applyResourcesToPodSpec(&sts.Spec.Template.Spec, desired) {
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
// Returns true if any container was modified.
func applyResourcesToPodSpec(spec *corev1.PodSpec, desired map[string]corev1.ResourceRequirements) bool {
	modified := false
	for i := range spec.Containers {
		c := &spec.Containers[i]
		want, ok := desired[c.Name]
		if !ok {
			continue
		}
		if resourcesEqual(c.Resources, want) {
			continue
		}
		// Preserve uncontrolled limit fields when only requests are set.
		merged := mergeTemplateResources(c.Resources, want)
		c.Resources = merged
		modified = true
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
		if resourcesEqual(c.Resources, want) {
			continue
		}
		c.Resources = mergeTemplateResources(c.Resources, want)
		modified = true
	}
	return modified
}

// mergeTemplateResources applies want requests/limits onto current, keeping
// existing limit entries when want does not set limits for that resource.
func mergeTemplateResources(current, want corev1.ResourceRequirements) corev1.ResourceRequirements {
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
	return *out
}

func workloadKindName(w client.Object) string {
	switch w.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	default:
		return w.GetObjectKind().GroupVersionKind().Kind
	}
}

// successfulResizeWorkloads returns workload names that had a successful
// in-place resize in the given history batch.
func successfulResizeWorkloads(history []attunev1alpha1.ResizeHistoryEntry) map[string]bool {
	out := make(map[string]bool)
	for _, h := range history {
		if isSuccessfulInPlaceHistory(h) {
			out[h.Workload] = true
		}
	}
	return out
}

// laggingAfterResizeWorkloads returns workloads that should retry template
// persistence after a prior successful in-place resize (e.g. patch failed or
// was skipped mid-rollout). Includes this-cycle successes and any prior
// InPlace Success still present in status history. applyTemplatePersistence
// no-ops when the template already matches, so re-attempts are safe.
func laggingAfterResizeWorkloads(
	cycleHistory []attunev1alpha1.ResizeHistoryEntry,
	statusHistory []attunev1alpha1.ResizeHistoryEntry,
) map[string]bool {
	out := successfulResizeWorkloads(cycleHistory)
	for _, h := range statusHistory {
		if isSuccessfulInPlaceHistory(h) {
			out[h.Workload] = true
		}
	}
	return out
}
