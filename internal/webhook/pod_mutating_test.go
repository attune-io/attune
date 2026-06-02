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
	_ = attunev1alpha1.AddToScheme(s)
	return s
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

func TestPodMutatingHandler_CanaryMode(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeCanary)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "Canary mode should mutate")
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
