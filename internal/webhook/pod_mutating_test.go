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
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/conflict"
	"github.com/attune-io/attune/internal/operatormetrics"
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
	_ = batchv1.AddToScheme(s)
	_ = attunev1alpha1.AddToScheme(s)
	return s
}

func testDeployment(name, ns string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}
}

func testNamespace(name string, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
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

func TestPodAdmissionName(t *testing.T) {
	t.Parallel()
	named := testPod("app-abc-xyz", "ReplicaSet", "app-abc")
	generated := testPod("", "ReplicaSet", "app-abc")
	generated.GenerateName = "app-abc-"
	assert.Equal(t, "app-abc-xyz", podAdmissionName(named, "ignored"))
	assert.Equal(t, "from-req", podAdmissionName(generated, "from-req"))
	assert.Equal(t, "app-abc-", podAdmissionName(generated, ""))
	assert.Equal(t, "", podAdmissionName(nil, ""))
}

func TestGetWorkloadObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("DaemonSet", func(t *testing.T) {
		t.Parallel()
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-ds", Namespace: "default",
				Labels: map[string]string{"app": "agent"},
			},
		}
		cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(ds).Build()
		obj, err := getWorkloadObject(ctx, cl, "default", "DaemonSet", "my-ds")
		require.NoError(t, err)
		assert.Equal(t, "my-ds", obj.GetName())
		assert.Equal(t, "agent", obj.GetLabels()["app"])
	})

	t.Run("unsupported kind", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		_, err := getWorkloadObject(ctx, cl, "default", "ReplicaSet", "my-rs")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unsupported workload kind "ReplicaSet"`)
	})

	t.Run("missing object", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		_, err := getWorkloadObject(ctx, cl, "default", "Deployment", "missing")
		require.Error(t, err)
	})
}

func TestPodMutatingHandler_HappyPath(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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

func TestPodMutatingHandler_NamespaceFreeze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ns          *corev1.Namespace
		getErr      bool
		wantPatches bool
		wantMsg     string
	}{
		{
			name:        "freeze=true skips CREATE sizing",
			ns:          testNamespace("default", map[string]string{conflict.AnnotationFreeze: "true"}),
			wantPatches: false,
			wantMsg:     "attune.io/freeze=true",
		},
		{
			name:        "freeze absent mutates",
			ns:          testNamespace("default", nil),
			wantPatches: true,
		},
		{
			name:        "True is not freeze",
			ns:          testNamespace("default", map[string]string{conflict.AnnotationFreeze: "True"}),
			wantPatches: true,
		},
		{
			name:        "Get error fail-closed",
			getErr:      true,
			wantPatches: false,
			wantMsg:     "cannot read namespace for attune.io/freeze; skipping initial sizing (check namespaces get/list/watch RBAC)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
			pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

			builder := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy)
			if tt.ns != nil {
				builder = builder.WithObjects(tt.ns)
			}
			if tt.getErr {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Namespace); ok {
							return fmt.Errorf("simulated namespace get failure")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			handler := &PodMutatingHandler{Client: builder.Build(), Logger: logr.Discard()}

			resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
			require.True(t, resp.Allowed)
			if tt.wantPatches {
				require.NotEmpty(t, resp.Patches, "expected CREATE initial sizing")
			} else {
				assert.Nil(t, resp.Patches, "expected no CREATE mutation")
				if tt.wantMsg != "" {
					require.NotNil(t, resp.Result)
					assert.Contains(t, resp.Result.Message, tt.wantMsg)
				}
			}
		})
	}
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

func TestPodMutatingHandler_ListErrorFailOpen(t *testing.T) {
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("simulated list error")
		},
	}).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "error listing policies")
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

func TestPodMutatingHandler_ConflictCheckFailedDoesNotOverrideHealthyPolicy(t *testing.T) {
	// B is fail-closed (leftover recs kept) and listed first. A is healthy
	// with higher weight. CREATE must skip B and apply A's recs.
	policyB := testPolicy("policy-b", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policyB.Spec.Weight = 1
	policyB.Status.Conditions = []metav1.Condition{{
		Type:   attunev1alpha1.ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: attunev1alpha1.ReasonConflictCheckFailed,
	}}
	policyB.Status.Recommendations[0].Containers[0].Recommended.CPURequest = resource.MustParse("999m")
	policyB.Status.Recommendations[0].Containers[0].Recommended.MemoryRequest = resource.MustParse("1Gi")

	policyA := testPolicy("policy-a", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policyA.Spec.Weight = 1000

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policyA, policyB, testNamespace("default", nil)).Build()
	var logged string
	handler := &PodMutatingHandler{
		Client: cl,
		Logger: funcr.NewJSON(func(obj string) { logged += obj }, funcr.Options{Verbosity: 1}),
	}

	// Slice order puts B first so leftover recs would win without the skip.
	picked, rec := handler.findMatchingPolicy(context.Background(), "default",
		[]attunev1alpha1.AttunePolicy{*policyB, *policyA},
		"Deployment", "my-app", "my-app-abc-xyz", nil)
	require.NotNil(t, picked, "healthy higher-weight policy must still match")
	require.NotNil(t, rec)
	assert.Equal(t, "policy-a", picked.Name, "ConflictCheckFailed policy must not be selected")
	assert.Equal(t, resource.MustParse("500m"), rec.Containers[0].Recommended.CPURequest)

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "healthy policy A must still CREATE-size")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("500m"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("256Mi"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, "default/policy-a", mutatedPod.Annotations[AnnotationInitialSizingPolicy])
	assert.Contains(t, logged, "policy conflict check failed")
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

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.Patches, "legacy Success with empty Method is InPlace")
}

func TestPodMutatingHandler_StatefulSet(t *testing.T) {
	policy := testPolicy("sts-policy", "default", "StatefulSet", "my-sts", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-sts-0", "StatefulSet", "my-sts")

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("500m"), mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU])
}

func TestPodMutatingHandler_DefaultsInitialSizingEnablesCreate(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Spec.UpdateStrategy.InitialSizing = nil

	enabled := true
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{InitialSizing: &enabled},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, defaults, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "AttuneDefaults initialSizing must enable CREATE")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.True(t, mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
}

func TestPodMutatingHandler_DefaultsTypeAutoEnablesCreate(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, "")
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, defaults, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "AttuneDefaults type Auto must enable CREATE")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.True(t, mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
}

func TestPodMutatingHandler_PausedSkipsCreate(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	paused := true
	policy.Spec.Paused = &paused
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "paused")
}

func TestPodMutatingHandler_CronJobRecommendCreates(t *testing.T) {
	policy := testPolicy("etl-policy", "default", "CronJob", "nightly-etl", true, attunev1alpha1.UpdateTypeRecommend)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-etl-29184000",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "nightly-etl"},
			},
		},
	}
	pod := testPod("nightly-etl-29184000-abc", "Job", "nightly-etl-29184000")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, job, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "Recommend CronJob must CREATE-size; it is the only apply path")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.True(t, mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
}

func TestPodMutatingHandler_CronJobOwnerMatchesPolicy(t *testing.T) {
	policy := testPolicy("etl-policy", "default", "CronJob", "nightly-etl", true, attunev1alpha1.UpdateTypeAuto)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-etl-29184000",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "nightly-etl"},
			},
		},
	}
	pod := testPod("nightly-etl-29184000-abc", "Job", "nightly-etl-29184000")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, job, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "CronJob policy must CREATE-size pods owned by its Job")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.True(t, mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
	assert.Equal(t, "default/etl-policy", mutatedPod.Annotations[AnnotationInitialSizingPolicy])
}

func TestPodMutatingHandler_StandaloneJobOwnerMatchesPolicy(t *testing.T) {
	policy := testPolicy("job-policy", "default", "Job", "standalone-job", true, attunev1alpha1.UpdateTypeAuto)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone-job",
			Namespace: "default",
		},
	}
	pod := testPod("standalone-job-abc", "Job", "standalone-job")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, job, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "standalone Job policy must CREATE-size pods it owns")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	wantCPU, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	got := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	assert.Equal(t, wantCPU.MilliValue(), got.MilliValue(), "standalone Job CPU request patched")
}

func TestPodMutatingHandler_JobGetErrorSkipsCreate(t *testing.T) {
	policy := testPolicy("job-policy", "default", "Job", "standalone-job", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("standalone-job-abc", "Job", "standalone-job")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, testNamespace("default", nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return fmt.Errorf("simulated job get failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "cannot read Job")
}

func TestPodMutatingHandler_JobKindPolicyDoesNotMatchGeneratedJobName(t *testing.T) {
	policy := testPolicy("job-policy", "default", "Job", "nightly-etl-29184000", true, attunev1alpha1.UpdateTypeAuto)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-etl-29184000",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "nightly-etl"},
			},
		},
	}
	pod := testPod("nightly-etl-29184000-abc", "Job", "nightly-etl-29184000")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, job, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "Job-kind policy must not match the generated Job name")
}

func TestPodMutatingHandler_StartupBoostRaisesCREATECPU(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 2 * time.Minute},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches)

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	got := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	assert.True(t, got.Equal(resource.MustParse("1")),
		"CREATE CPU %s want 1 (2x 500m boost)", got.String())
	assert.NotEmpty(t, mutatedPod.Annotations[AnnotationStartupBoostAt])
}

func TestPodMutatingHandler_StartupBoostDestClamp(t *testing.T) {
	t.Parallel()
	only := attunev1alpha1.ControlledRequestsOnly
	both := attunev1alpha1.ControlledRequestsAndLimits

	tests := []struct {
		name        string
		cv          *string
		leftover    string
		recDest     string
		maxAllowed  string
		wantCPU     string
		wantDest    string
		wantBoostAt bool
	}{
		{
			name:        "RequestsOnly leftover dest 200m dest-caps and skips boost",
			cv:          &only,
			leftover:    "200m",
			wantCPU:     "200m",
			wantDest:    "200m",
			wantBoostAt: false,
		},
		{
			name:        "RequestsOnly leftover dest 800m boosts to dest",
			cv:          &only,
			leftover:    "800m",
			wantCPU:     "800m",
			wantDest:    "800m",
			wantBoostAt: true,
		},
		{
			name:        "RequestsAndLimits leftover dest 200m boosts to rec dest",
			cv:          &both,
			leftover:    "200m",
			recDest:     "1",
			wantCPU:     "1",
			wantDest:    "1",
			wantBoostAt: true,
		},
		{
			name:        "RequestsAndLimits Guaranteed rec dest equals rec request raises dest with boost",
			cv:          &both,
			leftover:    "500m",
			recDest:     "500m",
			wantCPU:     "1",
			wantDest:    "1",
			wantBoostAt: true,
		},
		{
			name:        "boost capped at maxAllowed",
			cv:          &only,
			leftover:    "800m",
			maxAllowed:  "600m",
			wantCPU:     "600m",
			wantDest:    "800m",
			wantBoostAt: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
			policy.Spec.CPU.ControlledValues = tt.cv
			policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
				Multiplier: "2.0",
				Duration:   metav1.Duration{Duration: 2 * time.Minute},
			}
			if tt.recDest != "" {
				cpuLim, err := resource.ParseQuantity(tt.recDest)
				require.NoError(t, err)
				policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = cpuLim
			}
			if tt.maxAllowed != "" {
				maxAllowed, err := resource.ParseQuantity(tt.maxAllowed)
				require.NoError(t, err)
				policy.Spec.CPU.MaxAllowed = &maxAllowed
			}

			pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
			leftover, err := resource.ParseQuantity(tt.leftover)
			require.NoError(t, err)
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
				corev1.ResourceCPU: leftover,
			}

			cl := fake.NewClientBuilder().WithScheme(testScheme()).
				WithObjects(policy, testNamespace("default", nil)).Build()
			handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

			req := makeAdmissionRequest(t, pod, "default")
			resp := handler.Handle(context.Background(), req)
			require.True(t, resp.Allowed)
			require.NotEmpty(t, resp.Patches)

			mutatedPod := patchedPod(t, req.Object.Raw, resp)
			got := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			wantCPU, err := resource.ParseQuantity(tt.wantCPU)
			require.NoError(t, err)
			assert.Equal(t, wantCPU.MilliValue(), got.MilliValue(),
				"CREATE CPU request dest-clamp")
			if tt.wantDest != "" {
				wantDest, destErr := resource.ParseQuantity(tt.wantDest)
				require.NoError(t, destErr)
				gotDest := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
				assert.Equal(t, wantDest.MilliValue(), gotDest.MilliValue(),
					"CREATE CPU dest")
			}
			assert.Equal(t, tt.wantBoostAt, mutatedPod.Annotations[AnnotationStartupBoostAt] != "")
		})
	}
}

func TestPodMutatingHandler_ClusterDefaultsRequestsAndLimitsWritesCPULimit(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")

	cv := attunev1alpha1.ControlledRequestsAndLimits
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &cv},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, defaults, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "AttuneDefaults RequestsAndLimits must write the rec CPU limit")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	got := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	assert.True(t, got.Equal(resource.MustParse("1")),
		"CREATE CPU limit %s want 1 (merged from AttuneDefaults)", got.String())
}

func TestPodMutatingHandler_NamespaceDefaultsRequestsAndLimitsWritesCPULimit(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")

	cv := attunev1alpha1.ControlledRequestsAndLimits
	nsDefaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-defaults", Namespace: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &cv},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, nsDefaults, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "AttuneNamespaceDefaults RequestsAndLimits must write the rec CPU limit")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	got := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	assert.True(t, got.Equal(resource.MustParse("1")),
		"CREATE CPU limit %s want 1 (merged from AttuneNamespaceDefaults)", got.String())
}

func TestPodMutatingHandler_PolicyControlledValuesWinsOverDefaults(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	only := attunev1alpha1.ControlledRequestsOnly
	policy.Spec.CPU.ControlledValues = &only
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")

	cv := attunev1alpha1.ControlledRequestsAndLimits
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &cv},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, defaults, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches)

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	_, hasLimit := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	assert.False(t, hasLimit, "policy RequestsOnly must not inherit AttuneDefaults RequestsAndLimits")
}

func TestPodMutatingHandler_DefaultsListErrorSkipsCreate(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")

	base := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, testNamespace("default", nil)).Build()
	cl := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(policy, testNamespace("default", nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				switch list.(type) {
				case *attunev1alpha1.AttuneDefaultsList, *attunev1alpha1.AttuneNamespaceDefaultsList:
					return fmt.Errorf("simulated defaults list error")
				default:
					return base.List(ctx, list, opts...)
				}
			},
		}).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "AttuneDefaults")
}

func TestPodMutatingHandler_RequestsAndLimits(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	policy.Status.Recommendations[0].Containers[0].Recommended.CPULimit = resource.MustParse("1")
	policy.Status.Recommendations[0].Containers[0].Recommended.MemoryLimit = resource.MustParse("512Mi")

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.Equal(t, resource.MustParse("1"), mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory])
}

func TestPodMutatingHandler_LimitsOnlyCPUProducesPatch(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	cr := &policy.Status.Recommendations[0].Containers[0]
	cr.Recommended.CPURequest = resource.Quantity{}
	cr.Recommended.MemoryRequest = resource.Quantity{}
	cr.Recommended.CPULimit = resource.MustParse("1")
	cr.Recommended.MemoryLimit = resource.Quantity{}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "limits-only CPU rec must produce a patch")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	assert.True(t, mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU].Equal(resource.MustParse("1")))
}

func TestPodMutatingHandler_LimitsOnlyMemoryFloorProducesPatch(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.Memory.ControlledValues = &cv
	cr := &policy.Status.Recommendations[0].Containers[0]
	cr.Recommended.CPURequest = resource.Quantity{}
	cr.Recommended.MemoryRequest = resource.Quantity{}
	cr.Recommended.CPULimit = resource.Quantity{}
	cr.Recommended.MemoryLimit = resource.MustParse("200Mi")
	cr.Current = attunev1alpha1.ResourceValues{
		MemoryRequest: resource.MustParse("1Gi"),
		MemoryLimit:   resource.MustParse("1Gi"),
	}
	cr.Explanation = &attunev1alpha1.ContainerRecommendationExplanation{
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{
			RawPercentile: resource.MustParse("500Mi"),
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "limits-only memory rec must produce a patch")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	want := resource.MustParse("550Mi")
	gotLim := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	gotReq := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	assert.True(t, gotLim.Equal(want), "CREATE floor limit %s want %s", gotLim.String(), want.String())
	assert.True(t, gotReq.Equal(want), "CREATE floor request %s want %s", gotReq.String(), want.String())
}

func TestPodMutatingHandler_RequestsAndLimits_UsageFloorGuaranteed(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	cr := &policy.Status.Recommendations[0].Containers[0]
	cr.Current = attunev1alpha1.ResourceValues{
		CPURequest:    resource.MustParse("200m"),
		CPULimit:      resource.MustParse("200m"),
		MemoryRequest: resource.MustParse("1Gi"),
		MemoryLimit:   resource.MustParse("1Gi"),
	}
	cr.Recommended.CPURequest = resource.MustParse("200m")
	cr.Recommended.CPULimit = resource.MustParse("200m")
	cr.Recommended.MemoryRequest = resource.MustParse("200Mi")
	cr.Recommended.MemoryLimit = resource.MustParse("200Mi")
	cr.Explanation = &attunev1alpha1.ContainerRecommendationExplanation{
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{
			RawPercentile: resource.MustParse("500Mi"),
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	want := resource.MustParse("550Mi")
	gotLim := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	gotReq := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	assert.True(t, gotLim.Equal(want),
		"CREATE must not write a limit below the usage floor, got %s want %s",
		gotLim.String(), want.String())
	assert.True(t, gotReq.Equal(want),
		"Guaranteed CREATE request %s want %s", gotReq.String(), want.String())
}

func TestPodMutatingHandler_ClampsRequestToLeftoverLimit(t *testing.T) {
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("200m"),
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	beforeCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues(
		"default", "my-policy", "app", "cpu"))

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "expected patches")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	gotReq := mutatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	gotLim := mutatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	assert.True(t, gotReq.Equal(resource.MustParse("200m")),
		"CREATE request %s must clamp to leftover limit 200m", gotReq.String())
	assert.True(t, gotLim.Equal(resource.MustParse("200m")),
		"leftover CPU limit must stay 200m, got %s", gotLim.String())

	afterCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues(
		"default", "my-policy", "app", "cpu"))
	assert.Equal(t, beforeCPU+1, afterCPU,
		"RequestClampedTotal should increment for CREATE leftover CPU clamp")
}

func TestPodMutatingHandler_NativeSidecarInitialSizing(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeAuto)
	policy.Status.Recommendations[0].Containers = []attunev1alpha1.ContainerRecommendation{
		{
			Name:       "sidecar",
			Confidence: 0.8,
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("400m"),
				MemoryRequest: resource.MustParse("128Mi"),
			},
		},
		{
			Name:       "bootstrap",
			Confidence: 0.8,
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("300m"),
				MemoryRequest: resource.MustParse("64Mi"),
			},
		},
	}

	pod := testPod("my-app-abc-xyz", "ReplicaSet", "my-app-abc")
	pod.Spec.InitContainers = []corev1.Container{
		{
			Name:          "sidecar",
			RestartPolicy: &always,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
		},
		{
			Name: "bootstrap",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
	handler := &PodMutatingHandler{Client: cl, Logger: logr.Discard()}

	req := makeAdmissionRequest(t, pod, "default")
	resp := handler.Handle(context.Background(), req)
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "native sidecar must produce a CREATE patch")

	mutatedPod := patchedPod(t, req.Object.Raw, resp)
	require.Len(t, mutatedPod.Spec.InitContainers, 2)
	sidecar := mutatedPod.Spec.InitContainers[0]
	gotSidecarCPU := sidecar.Resources.Requests[corev1.ResourceCPU]
	gotSidecarMem := sidecar.Resources.Requests[corev1.ResourceMemory]
	assert.True(t, gotSidecarCPU.Equal(resource.MustParse("400m")),
		"native sidecar CPU request %s want 400m", gotSidecarCPU.String())
	assert.True(t, gotSidecarMem.Equal(resource.MustParse("128Mi")),
		"native sidecar memory request %s want 128Mi", gotSidecarMem.String())

	bootstrap := mutatedPod.Spec.InitContainers[1]
	gotInitCPU := bootstrap.Resources.Requests[corev1.ResourceCPU]
	gotInitMem := bootstrap.Resources.Requests[corev1.ResourceMemory]
	assert.True(t, gotInitCPU.Equal(resource.MustParse("50m")),
		"regular init must not be CREATE-sized, got %s", gotInitCPU.String())
	assert.True(t, gotInitMem.Equal(resource.MustParse("32Mi")),
		"regular init memory must stay 32Mi, got %s", gotInitMem.String())
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

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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

func TestPodMutatingHandler_CanaryMode_EmptyNameSkips(t *testing.T) {
	// ReplicaSet CREATE often has Name="" and only GenerateName. Canary
	// matching uses the real name. An empty name must not match the
	// slice and must not be treated as generateName.
	policy := testPolicy("my-policy", "default", "Deployment", "my-app", true, attunev1alpha1.UpdateTypeCanary)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Pods:  []string{"my-app-abc-xyz"},
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "my-app", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"my-app-abc-xyz"}},
		},
	}
	pod := testPod("", "ReplicaSet", "my-app-abc")
	pod.GenerateName = "my-app-abc-"

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy).Build()
	var logged string
	handler := &PodMutatingHandler{
		Client: cl,
		Logger: funcr.NewJSON(func(obj string) { logged += obj }, funcr.Options{Verbosity: 1}),
	}

	resp := handler.Handle(context.Background(), makeAdmissionRequest(t, pod, "default"))
	require.True(t, resp.Allowed)
	assert.Nil(t, resp.Patches, "empty CREATE Name is not in the canary slice")
	assert.Contains(t, logged, "canary has not promoted this app")
	assert.Contains(t, logged, `"pod":""`)
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

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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
		WithObjects(policy, deployA, deployB, deployOther, testNamespace("default", nil)).Build()
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

	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, testNamespace("default", nil)).Build()
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
