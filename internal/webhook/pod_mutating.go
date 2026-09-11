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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/conflict"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

const (
	// AnnotationSkipKey opts a pod out of initial sizing.
	AnnotationSkipKey = "attune.io/skip"
	// AnnotationInitialSizing marks that initial sizing was applied.
	AnnotationInitialSizing = "attune.io/initial-sizing"
	// AnnotationInitialSizingPolicy records which policy was used.
	AnnotationInitialSizingPolicy = "attune.io/initial-sizing-policy"
	// AnnotationStartupBoostAt records when CREATE applied a startup CPU boost.
	AnnotationStartupBoostAt = "attune.io/startup-boost-at"
	// minConfidenceForInitialSizing is the minimum confidence to apply initial sizing.
	minConfidenceForInitialSizing = 0.5
)

// PodMutatingHandler handles pod admission requests for initial sizing.
// It reads pre-computed recommendations from AttunePolicy status (via the
// informer cache, not the API server) and mutates pod resources at creation time.
type PodMutatingHandler struct {
	Client client.Client
	Logger logr.Logger
}

// Handle processes a pod admission request.
func (h *PodMutatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	timer := operatormetrics.NewWebhookTimer("pod-initial-sizing")
	defer timer.Observe()

	// Only handle CREATE operations.
	if req.Operation != "CREATE" {
		return admission.Allowed("not a CREATE operation")
	}

	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		timer.RecordResult(err)
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding pod: %w", err))
	}

	// Skip pods with opt-out annotation.
	if pod.Annotations != nil && pod.Annotations[AnnotationSkipKey] == "true" {
		return admission.Allowed("pod has skip annotation")
	}

	// Skip pods in kube-system.
	if req.Namespace == "kube-system" {
		return admission.Allowed("kube-system namespace excluded")
	}

	// Find the owning workload.
	ownerKind, ownerName := resolveOwner(pod.OwnerReferences)
	if ownerKind == "" || ownerName == "" {
		return admission.Allowed("no recognized owner")
	}
	if ownerKind == "Job" {
		cronKind, cronName, err := resolveCronJobOwner(ctx, h.Client, req.Namespace, ownerName)
		if err != nil {
			h.Logger.Error(err, "getting Job for CronJob initial sizing; skipping",
				"namespace", req.Namespace, "job", ownerName)
			return admission.Allowed("cannot read Job for CronJob initial sizing")
		}
		if cronName != "" {
			ownerKind, ownerName = cronKind, cronName
		}
	}

	// List all AttunePolicies in the namespace (from informer cache).
	var policies attunev1alpha1.AttunePolicyList
	if err := h.Client.List(ctx, &policies, client.InNamespace(req.Namespace)); err != nil {
		h.Logger.Error(err, "listing policies for initial sizing", "namespace", req.Namespace)
		return admission.Allowed("error listing policies, skipping initial sizing")
	}

	// Match reconcile: merge cluster + namespace AttuneDefaults before
	// matching on initialSizing / type and before reading ControlledValues.
	defaults, err := h.listAdmissionDefaults(ctx, req.Namespace)
	if err != nil {
		h.Logger.Error(err, "listing AttuneDefaults for initial sizing; skipping",
			"namespace", req.Namespace)
		return admission.Allowed("error listing AttuneDefaults, skipping initial sizing")
	}

	// Find a matching policy with initial sizing enabled.
	policy, rec := h.findMatchingPolicy(ctx, req.Namespace, policies.Items, ownerKind, ownerName, pod.Name, defaults)
	if policy == nil || rec == nil {
		return admission.Allowed("no matching policy with initial sizing")
	}

	if policy.Spec.Paused != nil && *policy.Spec.Paused {
		h.Logger.Info("skipping initial sizing: policy is paused",
			"namespace", req.Namespace, "policy", policy.Name)
		return admission.Allowed("policy is paused")
	}

	// Namespace freeze is an incident kill-switch: do not CREATE-size.
	// Get errors fail closed (do not mutate). Exact "true" only.
	frozen, err := conflict.NamespaceApplyFrozen(ctx, h.Client, req.Namespace)
	if err != nil {
		h.Logger.Error(err, "getting namespace for attune.io/freeze; skipping initial sizing",
			"namespace", req.Namespace)
		return admission.Allowed("cannot read namespace for attune.io/freeze; skipping initial sizing (check namespaces get/list/watch RBAC)")
	}
	if frozen {
		h.Logger.Info("skipping initial sizing: namespace has attune.io/freeze=true",
			"namespace", req.Namespace, "policy", policy.Name)
		return admission.Allowed("namespace has attune.io/freeze=true")
	}

	// Mutate the pod's containers and native sidecars (init restartPolicy Always).
	mutated := false
	boosted := false
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		ok, didBoost := h.mutateContainer(container, rec, policy)
		if ok {
			mutated = true
		}
		if didBoost {
			boosted = true
		}
	}
	for i := range pod.Spec.InitContainers {
		container := &pod.Spec.InitContainers[i]
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			ok, didBoost := h.mutateContainer(container, rec, policy)
			if ok {
				mutated = true
			}
			if didBoost {
				boosted = true
			}
		}
	}

	if !mutated {
		return admission.Allowed("no containers matched recommendations")
	}

	// Add audit annotations.
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationInitialSizing] = "applied"
	pod.Annotations[AnnotationInitialSizingPolicy] = fmt.Sprintf("%s/%s", req.Namespace, policy.Name)
	if boosted {
		pod.Annotations[AnnotationStartupBoostAt] = time.Now().UTC().Format(time.RFC3339)
	}

	h.Logger.Info("initial sizing applied",
		"pod", podAdmissionName(pod, req.Name), "namespace", req.Namespace,
		"policy", policy.Name, "owner", ownerKind+"/"+ownerName)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		timer.RecordResult(err)
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling mutated pod: %w", err))
	}

	timer.RecordResult(nil)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// listAdmissionDefaults returns combined cluster + namespace defaults
// (same selection as controller.fetchDefaults). List errors fail closed.
func (h *PodMutatingHandler) listAdmissionDefaults(
	ctx context.Context,
	namespace string,
) (*attunev1alpha1.AttuneDefaults, error) {
	var nsList attunev1alpha1.AttuneNamespaceDefaultsList
	if err := h.Client.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing AttuneNamespaceDefaults in %s: %w", namespace, err)
	}
	var nsDefaults *attunev1alpha1.AttuneDefaults
	if len(nsList.Items) > 0 {
		picked := nsList.Items[0]
		for i := 1; i < len(nsList.Items); i++ {
			if nsList.Items[i].Name < picked.Name {
				picked = nsList.Items[i]
			}
		}
		nsDefaults = &attunev1alpha1.AttuneDefaults{
			ObjectMeta: picked.ObjectMeta,
			Spec:       picked.Spec,
		}
	}

	var clusterList attunev1alpha1.AttuneDefaultsList
	if err := h.Client.List(ctx, &clusterList); err != nil {
		return nil, fmt.Errorf("listing AttuneDefaults: %w", err)
	}
	var clusterDefaults *attunev1alpha1.AttuneDefaults
	if len(clusterList.Items) > 0 {
		clusterDefaults = &clusterList.Items[0]
		for i := 1; i < len(clusterList.Items); i++ {
			if clusterList.Items[i].Name < clusterDefaults.Name {
				clusterDefaults = &clusterList.Items[i]
			}
		}
	}

	return pkgdefaults.CombineDefaultsLayers(clusterDefaults, nsDefaults), nil
}

// findMatchingPolicy finds a policy that targets the given owner workload
// and has initial sizing enabled with valid recommendations.
func (h *PodMutatingHandler) findMatchingPolicy(
	ctx context.Context,
	namespace string,
	policies []attunev1alpha1.AttunePolicy,
	ownerKind, ownerName, podName string,
	defaults *attunev1alpha1.AttuneDefaults,
) (*attunev1alpha1.AttunePolicy, *attunev1alpha1.WorkloadRecommendation) {
	for i := range policies {
		policy := policies[i].DeepCopy()
		pkgdefaults.MergeDefaults(policy, defaults)

		// Check initial sizing is enabled (after AttuneDefaults merge).
		if policy.Spec.UpdateStrategy == nil || policy.Spec.UpdateStrategy.InitialSizing == nil || !*policy.Spec.UpdateStrategy.InitialSizing {
			continue
		}

		// Reconcile fail-closed keeps last recs when conflict listing
		// fails. Do not CREATE-size from those leftover recommendations.
		if cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady); cond != nil && cond.Reason == attunev1alpha1.ReasonConflictCheckFailed {
			h.Logger.V(1).Info("initial sizing skipped: policy conflict check failed",
				"policy", policy.Name, "owner", ownerName, "pod", podName)
			continue
		}

		// Skip Observe and Recommend modes (no active resize intent).
		if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeObserve ||
			policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeRecommend ||
			policy.Spec.UpdateStrategy.Type == "" {
			continue
		}

		// Canary: do not CREATE-size at the full recommendation unless
		// this app has been promoted or the pod is already in the slice.
		if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeCanary &&
			!policy.Status.Canary.AllowsCreateSizing(ownerName, podName) {
			h.Logger.V(1).Info("initial sizing skipped: canary has not promoted this app",
				"policy", policy.Name, "owner", ownerName, "pod", podName)
			continue
		}

		if !h.targetRefMatches(ctx, namespace, policy, ownerKind, ownerName) {
			continue
		}

		// Find matching recommendation in status.
		for j := range policy.Status.Recommendations {
			rec := &policy.Status.Recommendations[j]
			if rec.Stale {
				if rec.Workload == ownerName && rec.Kind == ownerKind {
					h.Logger.V(1).Info("initial sizing skipped: recommendation is stale",
						"policy", policy.Name, "owner", ownerName, "pod", podName)
				}
				continue
			}
			if rec.Workload == ownerName && rec.Kind == ownerKind {
				if recEligibleForCreateSizing(rec, policy.Status.ResizeHistory, ownerName) {
					return policy, rec
				}
			}
		}
	}
	return nil, nil
}

// targetRefMatches reports whether this policy's targetRef covers the
// owning workload. Name is exact. A selector fetches the workload and
// matches its labels (fail closed on Get or parse errors).
func (h *PodMutatingHandler) targetRefMatches(
	ctx context.Context,
	namespace string,
	policy *attunev1alpha1.AttunePolicy,
	ownerKind, ownerName string,
) bool {
	if policy.Spec.TargetRef.Kind != ownerKind {
		return false
	}
	if policy.Spec.TargetRef.Name != nil {
		return *policy.Spec.TargetRef.Name == ownerName
	}
	if policy.Spec.TargetRef.Selector == nil {
		return true
	}
	sel, err := metav1.LabelSelectorAsSelector(policy.Spec.TargetRef.Selector)
	if err != nil || sel.Empty() {
		return false
	}
	obj, err := getWorkloadObject(ctx, h.Client, namespace, ownerKind, ownerName)
	if err != nil {
		h.Logger.Error(err, "fetching workload for initial-sizing selector",
			"kind", ownerKind, "name", ownerName, "namespace", namespace)
		return false
	}
	return sel.Matches(labels.Set(obj.GetLabels()))
}

func getWorkloadObject(ctx context.Context, c client.Client, namespace, kind, name string) (client.Object, error) {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	var obj client.Object
	switch kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "StatefulSet":
		obj = &appsv1.StatefulSet{}
	case "DaemonSet":
		obj = &appsv1.DaemonSet{}
	case "CronJob":
		obj = &batchv1.CronJob{}
	case "Job":
		obj = &batchv1.Job{}
	default:
		return nil, fmt.Errorf("unsupported workload kind %q", kind)
	}
	if err := c.Get(ctx, key, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// mutateContainer applies the recommendation to a single container.
func (h *PodMutatingHandler) mutateContainer(
	container *corev1.Container,
	rec *attunev1alpha1.WorkloadRecommendation,
	policy *attunev1alpha1.AttunePolicy,
) (mutated bool, boosted bool) {
	for _, cr := range rec.Containers {
		if cr.Name != container.Name {
			continue
		}

		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}

		mutated := false
		boosted := false

		// Apply CPU request (startup boost may raise it before dest clamp).
		if !cr.Recommended.CPURequest.IsZero() {
			container.Resources.Requests[corev1.ResourceCPU] = cr.Recommended.CPURequest
			boosted = applyCreateStartupBoost(container, policy)
			mutated = true
		}

		// Apply memory request.
		if !cr.Recommended.MemoryRequest.IsZero() {
			container.Resources.Requests[corev1.ResourceMemory] = cr.Recommended.MemoryRequest
			mutated = true
		}

		// Apply limits if controlledValues is RequestsAndLimits.
		cpuCV := policy.Spec.CPU.ControlledValues
		if cpuCV != nil && *cpuCV == attunev1alpha1.ControlledRequestsAndLimits {
			if !cr.Recommended.CPULimit.IsZero() {
				if container.Resources.Limits == nil {
					container.Resources.Limits = corev1.ResourceList{}
				}
				container.Resources.Limits[corev1.ResourceCPU] = cr.Recommended.CPULimit
				mutated = true
			}
		}

		memCV := policy.Spec.Memory.ControlledValues
		if memCV != nil && *memCV == attunev1alpha1.ControlledRequestsAndLimits {
			if !cr.Recommended.MemoryLimit.IsZero() {
				if container.Resources.Limits == nil {
					container.Resources.Limits = corev1.ResourceList{}
				}
				memTarget := applyCreateMemoryUsageFloor(container.Resources, cr, policy)
				container.Resources.Limits[corev1.ResourceMemory] = memTarget.Limits[corev1.ResourceMemory]
				mutated = true
				if req, ok := memTarget.Requests[corev1.ResourceMemory]; ok {
					container.Resources.Requests[corev1.ResourceMemory] = req
				}
			}
		}

		for _, res := range resize.ClampRequestsToLimits(&container.Resources) {
			operatormetrics.RequestClampedTotal.WithLabelValues(
				policy.Namespace, policy.Name, container.Name, res).Inc()
		}
		return mutated, boosted
	}
	return false, false
}

// applyCreateStartupBoost raises the CREATE CPU request by the policy
// multiplier, capped at maxAllowed and leftover dest limit. Returns true
// when the request was raised so Handle can stamp startup-boost-at.
func applyCreateStartupBoost(container *corev1.Container, policy *attunev1alpha1.AttunePolicy) bool {
	if policy == nil || policy.Spec.CPU.StartupBoost == nil {
		return false
	}
	cfg := policy.Spec.CPU.StartupBoost
	mult, err := strconv.ParseFloat(cfg.Multiplier, 64)
	if err != nil || math.IsNaN(mult) || math.IsInf(mult, 0) || mult <= 1 {
		return false
	}
	cpu, ok := container.Resources.Requests[corev1.ResourceCPU]
	if !ok || cpu.IsZero() {
		return false
	}
	boosted := *resource.NewMilliQuantity(int64(float64(cpu.MilliValue())*mult), resource.DecimalSI)
	if policy.Spec.CPU.MaxAllowed != nil && boosted.Cmp(*policy.Spec.CPU.MaxAllowed) > 0 {
		boosted = policy.Spec.CPU.MaxAllowed.DeepCopy()
	}
	if lim, hasLim := container.Resources.Limits[corev1.ResourceCPU]; hasLim && boosted.Cmp(lim) > 0 {
		boosted = lim.DeepCopy()
	}
	if boosted.Cmp(cpu) <= 0 {
		return false
	}
	container.Resources.Requests[corev1.ResourceCPU] = boosted
	return true
}

// applyCreateMemoryUsageFloor floors a CREATE memory limit the same way
// persist does (max currentLimit with usageFloor) and raises a Guaranteed
// request to the floored limit. CREATE is not /resize, so no 1.33 clamp.
func applyCreateMemoryUsageFloor(
	live corev1.ResourceRequirements,
	cr attunev1alpha1.ContainerRecommendation,
	policy *attunev1alpha1.AttunePolicy,
) corev1.ResourceRequirements {
	target := corev1.ResourceRequirements{
		Requests: live.Requests.DeepCopy(),
		Limits:   corev1.ResourceList{corev1.ResourceMemory: cr.Recommended.MemoryLimit.DeepCopy()},
	}
	if req, ok := live.Requests[corev1.ResourceMemory]; !ok || req.IsZero() {
		if !cr.Recommended.MemoryRequest.IsZero() {
			if target.Requests == nil {
				target.Requests = corev1.ResourceList{}
			}
			target.Requests[corev1.ResourceMemory] = cr.Recommended.MemoryRequest.DeepCopy()
		}
	}

	if cr.Explanation != nil && cr.Explanation.Memory != nil {
		usage := cr.Explanation.Memory.RawPercentile
		if !usage.IsZero() && usage.Sign() > 0 {
			margin := float64(attunev1alpha1.DefaultDecreaseUsageMarginPercent)
			if policy != nil && policy.Spec.Memory.DecreaseUsageMarginPercent != nil {
				margin = float64(*policy.Spec.Memory.DecreaseUsageMarginPercent)
			}
			if floored, applied := resize.FloorMemoryLimitAgainstStaleCurrent(
				target, cr.Current.MemoryLimit, usage, margin); applied {
				target = floored
			}
		}
	}

	guaranteed := resize.CurrentResourcesAreGuaranteed(
		cr.Current.CPURequest, cr.Current.CPULimit,
		cr.Current.MemoryRequest, cr.Current.MemoryLimit)
	return resize.RaiseMemoryRequestToLimitIfGuaranteed(target, guaranteed)
}

// resolveOwner walks the ownerReferences to find the top-level workload kind.
// For pods created by a ReplicaSet (owned by a Deployment), the owner chain is:
// Pod -> ReplicaSet -> Deployment. We resolve ReplicaSet to Deployment by
// stripping the pod-template-hash suffix from the ReplicaSet name.
func resolveOwner(refs []metav1.OwnerReference) (kind, name string) {
	for _, ref := range refs {
		switch ref.Kind {
		case "ReplicaSet":
			// ReplicaSet names follow <deployment-name>-<pod-template-hash>.
			deployName := extractDeploymentName(ref.Name)
			if deployName != "" {
				return "Deployment", deployName
			}
			return "ReplicaSet", ref.Name
		case "StatefulSet", "DaemonSet", "Job":
			return ref.Kind, ref.Name
		}
	}
	return "", ""
}

// resolveCronJobOwner returns the CronJob that owns jobName. Empty name
// means a standalone Job. Get errors fail closed so CREATE does not
// size from the generated Job name.
func resolveCronJobOwner(ctx context.Context, c client.Client, namespace, jobName string) (kind, name string, err error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobName}, job); err != nil {
		return "", "", err
	}
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "CronJob" && ref.Name != "" {
			return "CronJob", ref.Name, nil
		}
	}
	return "", "", nil
}

// extractDeploymentName extracts the Deployment name from a ReplicaSet name
// by stripping the last -<hash> suffix.
func extractDeploymentName(rsName string) string {
	for i := len(rsName) - 1; i >= 0; i-- {
		if rsName[i] == '-' {
			if i > 0 {
				return rsName[:i]
			}
			return ""
		}
	}
	return ""
}

// recEligibleForCreateSizing is true when every container meets the
// confidence floor, or this workload already has a successful in-place
// resize. One Success means we trust CREATE for that name even when
// the current rec is still low-confidence (short history windows).
func recEligibleForCreateSizing(
	rec *attunev1alpha1.WorkloadRecommendation,
	history []attunev1alpha1.ResizeHistoryEntry,
	workload string,
) bool {
	if rec == nil {
		return false
	}
	if hasMinConfidence(rec.Containers, minConfidenceForInitialSizing) {
		return true
	}
	return hasSuccessfulInPlaceHistory(history, workload)
}

func hasSuccessfulInPlaceHistory(history []attunev1alpha1.ResizeHistoryEntry, workload string) bool {
	if workload == "" {
		return false
	}
	for i := range history {
		h := history[i]
		// Empty Method is InPlace, same as resizeHistoryMethod in the
		// controller (legacy rows before the field was required).
		method := h.Method
		if method == "" {
			method = "InPlace"
		}
		if h.Workload == workload && method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			return true
		}
	}
	return false
}

// podAdmissionName is the best identifier at CREATE. ReplicaSet pods
// often have an empty Name and only GenerateName until the API assigns
// one. Do not use this for canary slice matching.
func podAdmissionName(pod *corev1.Pod, reqName string) string {
	if pod != nil && pod.Name != "" {
		return pod.Name
	}
	if reqName != "" {
		return reqName
	}
	if pod != nil && pod.GenerateName != "" {
		return pod.GenerateName
	}
	return ""
}

// hasMinConfidence returns true if all containers meet the minimum confidence.
func hasMinConfidence(containers []attunev1alpha1.ContainerRecommendation, minConf float64) bool {
	if len(containers) == 0 {
		return false
	}
	for _, c := range containers {
		if c.Confidence < minConf {
			return false
		}
	}
	return true
}
