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
	"net/http"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// patchedPod applies the admission response patches to the original pod bytes.
// PatchResponseFromRaw sets resp.Patches (parsed slice) but may not serialize
// resp.Patch (raw bytes) until the webhook server writes the response.
// We marshal the parsed patches ourselves for test application.
func patchedPod(t *testing.T, original []byte, resp admission.Response) *corev1.Pod {
	t.Helper()
	require.NotEmpty(t, resp.Patches, "expected non-empty patches")

	patchBytes, err := json.Marshal(resp.Patches)
	require.NoError(t, err, "marshaling patches")

	patch, err := jsonpatch.DecodePatch(patchBytes)
	require.NoError(t, err, "decoding JSON patch")

	mutated, err := patch.Apply(original)
	require.NoError(t, err, "applying JSON patch")

	pod := &corev1.Pod{}
	require.NoError(t, json.Unmarshal(mutated, pod))
	return pod
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = attunev1alpha1.AddToScheme(s)
	return s
}

func testDeployment(name, ns string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}
}

func makePodRaw(t *testing.T, pod *corev1.Pod) []byte {
	t.Helper()
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	return raw
}

func makeAdmissionRequest(t *testing.T, pod *corev1.Pod, ns string) admission.Request {
	t.Helper()
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: "CREATE",
			Namespace: ns,
			Object:    runtime.RawExtension{Raw: makePodRaw(t, pod)},
		},
	}
}

func testPolicy(name, ns, targetKind, targetName string, initialSizing bool, updateType attunev1alpha1.UpdateType) *attunev1alpha1.AttunePolicy {
	return &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: targetKind,
				Name: strPtr(targetName),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:          updateType,
				InitialSizing: boolPtr(initialSizing),
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Recommendations: []attunev1alpha1.WorkloadRecommendation{
				{
					Workload: targetName,
					Kind:     targetKind,
					Containers: []attunev1alpha1.ContainerRecommendation{
						{
							Name:       "app",
							Confidence: 0.8,
							Recommended: attunev1alpha1.ResourceValues{
								CPURequest:    resource.MustParse("500m"),
								MemoryRequest: resource.MustParse("256Mi"),
							},
						},
					},
				},
			},
		},
	}
}

func testPod(name string, ownerKind, ownerName string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: ownerKind, Name: ownerName},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
				},
			},
		},
	}
	return pod
}

func TestPodMutatingHandler_HappyPath(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)

	require.True(t, resp.Allowed, "expected pod to be allowed")
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("500m"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("256Mi"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, "applied", mutatedPod.Annotations[AnnotationInitialSizing])
	assert.Equal(t, "default/my-policy", mutatedPod.Annotations[AnnotationInitialSizingPolicy])
}

func TestPodMutatingHandler_SkipAnnotation(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	pod.Annotations = map[string]string{AnnotationSkipKey: "true"}

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "expected no patches for skipped pod")
}

func TestPodMutatingHandler_KubeSystem(t *testing.T) {
	handler := &PodMutatingHandler{
		Client: fake.NewClientBuilder().WithScheme(testScheme()).Build(),
		Logger: logr.Discard(),
	}
	pod := testPod("coredns-abc", "ReplicaSet", "coredns-abc")
	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "kube-system"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_NotCreate(t *testing.T) {
	handler := &PodMutatingHandler{
		Client: fake.NewClientBuilder().WithScheme(testScheme()).Build(),
		Logger: logr.Discard(),
	}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: "UPDATE",
			Namespace: "default",
		},
	}
	resp := handler.Handle(context.Background(), req)
	assert.True(t, resp.Allowed)
}

func TestPodMutatingHandler_NoOwner(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_InitialSizingDisabled(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", false, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_ObserveMode(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeObserve)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Observe mode should not mutate")
}

func TestPodMutatingHandler_RecommendMode(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeRecommend)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Recommend mode should not mutate")
}

func TestPodMutatingHandler_StaleRecommendation(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Stale = true
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_LowConfidence(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Confidence = 0.3
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "low confidence should skip initial sizing")
}

func TestPodMutatingHandler_LowConfidence_SuccessfulHistoryApplies(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Confidence = 0.01
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload:  "my-app",
			Container: "app",
			Resource:  "memory",
			From:      "64Mi",
			To:        "256Mi",
			Method:    "InPlace",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
	}
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "a rec already applied in-place may CREATE-size")
}

func TestPodMutatingHandler_LowConfidence_WrongWorkloadHistorySkips(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Confidence = 0.01
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload: "other-app",
			Method:   "InPlace",
			Result:   attunev1alpha1.ResizeResultSuccess,
		},
	}
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Success on a different workload must not unlock CREATE")
}

func TestPodMutatingHandler_LowConfidence_RevertedHistorySkips(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Confidence = 0.01
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload: "my-app",
			Method:   "InPlace",
			Result:   attunev1alpha1.ResizeResultReverted,
		},
	}
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Reverted history must not unlock CREATE")
}

func TestPodMutatingHandler_LowConfidence_EmptyMethodHistoryApplies(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Confidence = 0.01
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload: "my-app",
			Result:   attunev1alpha1.ResizeResultSuccess,
		},
	}
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "legacy Success with empty Method is InPlace")
}

func TestPodMutatingHandler_StatefulSet(t *testing.T) {
	policy := testPolicy("sts-policy", "default", "StatefulSet", "my-sts", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-sts-0", "StatefulSet", "my-sts")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("500m"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU])
}

func TestPodMutatingHandler_RequestsAndLimits(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")
	policy.Status.Recommendations[0].Containers[0].Recommended.MemoryLimit = resource.MustParse("512Mi")

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("1"), mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory])
}

func TestPodMutatingHandler_WrongNamespace(t *testing.T) {
	policy := testPolicy("my-policy", "production", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	// Pod is in "default" but policy is in "production".
	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_WrongTargetName(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "other-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
}

func TestPodMutatingHandler_InvalidPodJSON(t *testing.T) {
	handler := &PodMutatingHandler{
		Client: fake.NewClientBuilder().WithScheme(testScheme()).Build(),
		Logger: logr.Discard(),
	}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: "CREATE",
			Namespace: "default",
			Object:    runtime.RawExtension{Raw: []byte("{invalid")},
		},
	}
	resp := handler.Handle(context.Background(), req)
	assert.False(t, resp.Allowed)
	assert.Equal(t, int32(http.StatusBadRequest), resp.Result.Code)
}

func TestPodMutatingHandler_OneShotMode(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeOneShot)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "OneShot mode should mutate")
}

func TestPodMutatingHandler_CanaryMode_SkipsUntilPromoted(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeCanary)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "canary CREATE must not apply the full recommendation until that app is promoted")
}

func TestPodMutatingHandler_CanaryMode_MutatesCanarySlice(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeCanary)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Pods:  []string{"my-app-abc-xyz"},
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "my-app", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"my-app-abc-xyz"}},
		},
	}
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "pod already in the canary slice may be sized")
}

func TestPodMutatingHandler_CanaryMode_PromotedAppOnly(t *testing.T) {
	// One policy, two apps. CREATE must size only the promoted app (or a
	// pod already in the unpromoted app's canary slice).
	policy := testPolicy("my-policy", "default", "Deployment", "app-a", true, attunev1alpha1.UpdateTypeCanary)
	policy.Spec.TargetRef.Name = nil
	policy.Status.Recommendations = append(policy.Status.Recommendations, attunev1alpha1.WorkloadRecommendation{
		Workload: "app-b",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name:       "app",
				Confidence: 0.8,
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("500m"),
					MemoryRequest: resource.MustParse("256Mi"),
				},
			},
		},
	})
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseFullRollout},
			{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"app-b-canary"}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	aNew := testPod("app-a-new", "ReplicaSet", "app-a-abc")
	respA := handler.Handle(context.Background(), makeAdmissionRequest(t, aNew, "default"))
	require.True(t, respA.Allowed)
	require.NotNil(t, respA.Patches, "promoted app-a must CREATE-size a new pod")

	bNew := testPod("app-b-new", "ReplicaSet", "app-b-abc")
	respB := handler.Handle(context.Background(), makeAdmissionRequest(t, bNew, "default"))
	require.True(t, respB.Allowed)
	assert.Nil(t, respB.Patches, "unpromoted app-b must not CREATE-size a new pod")

	bCanary := testPod("app-b-canary", "ReplicaSet", "app-b-abc")
	respSlice := handler.Handle(context.Background(), makeAdmissionRequest(t, bCanary, "default"))
	require.True(t, respSlice.Allowed)
	require.NotNil(t, respSlice.Patches, "unpromoted app-b canary-slice pod may be sized")
}

func TestPodMutatingHandler_SelectorPolicy_CanaryIsolation(t *testing.T) {
	// Multi-app canary policies use a selector. CREATE must still size only
	// the promoted app (or the unpromoted canary slice).
	policy := testPolicy("fleet", "default", "Deployment", "app-a", true, attunev1alpha1.UpdateTypeCanary)
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "api"},
	}
	policy.Status.Recommendations = append(policy.Status.Recommendations, attunev1alpha1.WorkloadRecommendation{
		Workload: "app-b",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name:       "app",
				Confidence: 0.8,
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("500m"),
					MemoryRequest: resource.MustParse("256Mi"),
				},
			},
		},
	})
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseFullRollout},
			{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"app-b-canary"}},
		},
	}
	deployA := testDeployment("app-a", "default", map[string]string{"tier": "api"})
	deployB := testDeployment("app-b", "default", map[string]string{"tier": "api"})
	deployOther := testDeployment("other", "default", map[string]string{"tier": "batch"})

	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, deployA, deployB, deployOther).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	respA := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("app-a-new", "ReplicaSet", "app-a-abc"), "default"))
	require.True(t, respA.Allowed)
	require.NotNil(t, respA.Patches, "selector-matched promoted app must CREATE-size")

	respB := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("app-b-new", "ReplicaSet", "app-b-abc"), "default"))
	require.True(t, respB.Allowed)
	assert.Nil(t, respB.Patches, "selector-matched unpromoted app must not CREATE-size")

	respOther := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("other-new", "ReplicaSet", "other-abc"), "default"))
	require.True(t, respOther.Allowed)
	assert.Nil(t, respOther.Patches, "workload outside the selector must not CREATE-size")

	respSlice := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("app-b-canary", "ReplicaSet", "app-b-abc"), "default"))
	require.True(t, respSlice.Allowed)
	require.NotNil(t, respSlice.Patches, "selector-matched unpromoted canary-slice pod may be sized")
}

func TestPodMutatingHandler_SelectorPolicy_EmptySelectorFailsClosed(t *testing.T) {
	policy := testPolicy("fleet", "default", "Deployment", "app-a", true, attunev1alpha1.UpdateTypeAuto)
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}
	deployA := testDeployment("app-a", "default", map[string]string{"tier": "api"})
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, deployA).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("app-a-new", "ReplicaSet", "app-a-abc"), "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "empty targetRef.selector must not match every Deployment")
}

func TestPodMutatingHandler_SelectorPolicy_MissingWorkloadFailsClosed(t *testing.T) {
	policy := testPolicy("fleet", "default", "Deployment", "app-a", true, attunev1alpha1.UpdateTypeAuto)
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "api"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t,
		testPod("app-a-new", "ReplicaSet", "app-a-abc"), "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Get miss on the owning Deployment must not CREATE-size")
}

func TestPodMutatingHandler_CanaryMode_MutatesAfterPromote(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeCanary)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseFullRollout,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "my-app", Phase: attunev1alpha1.CanaryPhaseFullRollout},
		},
	}
	pod := testPod("my-app-new-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "promoted app may CREATE-size new pods")
}

func TestResolveOwner(t *testing.T) {
	tests := []struct {
		name         string
		refs         []metav1.OwnerReference
		expectedKind string
		expectedName string
	}{
		{
			name:         "ReplicaSet resolves to Deployment",
			refs:         []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "my-app-6f8d4c5b7d"}},
			expectedKind: "Deployment",
			expectedName: "my-app",
		},
		{
			name:         "ReplicaSet with multi-dash name",
			refs:         []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "my-cool-app-6f8d4c5b7d"}},
			expectedKind: "Deployment",
			expectedName: "my-cool-app",
		},
		{
			name:         "StatefulSet",
			refs:         []metav1.OwnerReference{{Kind: "StatefulSet", Name: "my-sts"}},
			expectedKind: "StatefulSet",
			expectedName: "my-sts",
		},
		{
			name:         "DaemonSet",
			refs:         []metav1.OwnerReference{{Kind: "DaemonSet", Name: "my-ds"}},
			expectedKind: "DaemonSet",
			expectedName: "my-ds",
		},
		{
			name:         "Job",
			refs:         []metav1.OwnerReference{{Kind: "Job", Name: "my-job"}},
			expectedKind: "Job",
			expectedName: "my-job",
		},
		{
			name:         "no recognized owner",
			refs:         []metav1.OwnerReference{{Kind: "Node", Name: "node-1"}},
			expectedKind: "",
			expectedName: "",
		},
		{
			name:         "empty refs",
			refs:         nil,
			expectedKind: "",
			expectedName: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, name := resolveOwner(tt.refs)
			assert.Equal(t, tt.expectedKind, kind)
			assert.Equal(t, tt.expectedName, name)
		})
	}
}

func TestExtractDeploymentName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-app-6f8d4c5b7d", "my-app"},
		{"simple-abc123", "simple"},
		{"no-dash-at-end", "no-dash-at"},
		{"nodash", ""},
		{"-leadingdash", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractDeploymentName(tt.input))
		})
	}
}

func TestHasMinConfidence(t *testing.T) {
	tests := []struct {
		name       string
		containers []attunev1alpha1.ContainerRecommendation
		minConf    float64
		expected   bool
	}{
		{"empty", nil, 0.5, false},
		{"above threshold", []attunev1alpha1.ContainerRecommendation{{Confidence: 0.8}}, 0.5, true},
		{"at threshold", []attunev1alpha1.ContainerRecommendation{{Confidence: 0.5}}, 0.5, true},
		{"below threshold", []attunev1alpha1.ContainerRecommendation{{Confidence: 0.3}}, 0.5, false},
		{"mixed", []attunev1alpha1.ContainerRecommendation{{Confidence: 0.8}, {Confidence: 0.3}}, 0.5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, hasMinConfidence(tt.containers, tt.minConf))
		})
	}
}
