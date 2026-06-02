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
	"net/http"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

const (
	// AnnotationSkipKey opts a pod out of initial sizing.
	AnnotationSkipKey = "attune.io/skip"
	// AnnotationInitialSizing marks that initial sizing was applied.
	AnnotationInitialSizing = "attune.io/initial-sizing"
	// AnnotationInitialSizingPolicy records which policy was used.
	AnnotationInitialSizingPolicy = "attune.io/initial-sizing-policy"
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

	// List all AttunePolicies in the namespace (from informer cache).
	var policies attunev1alpha1.AttunePolicyList
	if err := h.Client.List(ctx, &policies, client.InNamespace(req.Namespace)); err != nil {
		h.Logger.Error(err, "listing policies for initial sizing", "namespace", req.Namespace)
		return admission.Allowed("error listing policies, skipping initial sizing")
	}

	// Find a matching policy with initial sizing enabled.
	policy, rec := h.findMatchingPolicy(policies.Items, ownerKind, ownerName)
	if policy == nil || rec == nil {
		return admission.Allowed("no matching policy with initial sizing")
	}

	// Mutate the pod's containers.
	mutated := false
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if h.mutateContainer(container, rec, policy) {
			mutated = true
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

	h.Logger.Info("initial sizing applied",
		"pod", pod.Name, "namespace", req.Namespace,
		"policy", policy.Name, "owner", ownerKind+"/"+ownerName)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		timer.RecordResult(err)
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling mutated pod: %w", err))
	}

	timer.RecordResult(nil)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// findMatchingPolicy finds a policy that targets the given owner workload
// and has initial sizing enabled with valid recommendations.
func (h *PodMutatingHandler) findMatchingPolicy(
	policies []attunev1alpha1.AttunePolicy,
	ownerKind, ownerName string,
) (*attunev1alpha1.AttunePolicy, *attunev1alpha1.WorkloadRecommendation) {
	for i := range policies {
		policy := &policies[i]

		// Check initial sizing is enabled.
		if policy.Spec.UpdateStrategy == nil || policy.Spec.UpdateStrategy.InitialSizing == nil || !*policy.Spec.UpdateStrategy.InitialSizing {
			continue
		}

		// Skip Observe and Recommend modes (no active resize intent).
		if policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeObserve ||
			policy.Spec.UpdateStrategy.Type == attunev1alpha1.UpdateTypeRecommend ||
			policy.Spec.UpdateStrategy.Type == "" {
			continue
		}

		// Check targetRef matches.
		if policy.Spec.TargetRef.Kind != ownerKind {
			continue
		}
		if policy.Spec.TargetRef.Name != nil && *policy.Spec.TargetRef.Name != ownerName {
			continue
		}
		// Selector-based policies would need to fetch the workload object
		// to check labels. Skip them for now; name-based matching covers
		// the primary use case.
		if policy.Spec.TargetRef.Name == nil && policy.Spec.TargetRef.Selector != nil {
			continue
		}

		// Find matching recommendation in status.
		for j := range policy.Status.Recommendations {
			rec := &policy.Status.Recommendations[j]
			if rec.Stale {
				continue
			}
			if rec.Workload == ownerName && rec.Kind == ownerKind {
				if hasMinConfidence(rec.Containers, minConfidenceForInitialSizing) {
					return policy, rec
				}
			}
		}
	}
	return nil, nil
}

// mutateContainer applies the recommendation to a single container.
func (h *PodMutatingHandler) mutateContainer(
	container *corev1.Container,
	rec *attunev1alpha1.WorkloadRecommendation,
	policy *attunev1alpha1.AttunePolicy,
) bool {
	for _, cr := range rec.Containers {
		if cr.Name != container.Name {
			continue
		}

		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}

		mutated := false

		// Apply CPU request.
		if !cr.Recommended.CPURequest.IsZero() {
			container.Resources.Requests[corev1.ResourceCPU] = cr.Recommended.CPURequest
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
			}
		}

		memCV := policy.Spec.Memory.ControlledValues
		if memCV != nil && *memCV == attunev1alpha1.ControlledRequestsAndLimits {
			if !cr.Recommended.MemoryLimit.IsZero() {
				if container.Resources.Limits == nil {
					container.Resources.Limits = corev1.ResourceList{}
				}
				container.Resources.Limits[corev1.ResourceMemory] = cr.Recommended.MemoryLimit
			}
		}

		return mutated
	}
	return false
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
