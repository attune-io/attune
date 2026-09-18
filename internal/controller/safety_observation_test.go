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
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

// ---------- checkPendingSafetyObservations ----------

func TestCheckPendingSafetyObservations_ObservationElapsed(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify tracking annotations were removed.
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations[annotationResizedAt]
	assert.False(t, hasResizedAt, "resized-at annotation should be removed")
	_, hasContainers := updated.Annotations[annotationResizedContainers]
	assert.False(t, hasContainers, "resized-containers annotation should be removed")
	_, hasTracked := updated.Labels[labelTracked]
	assert.False(t, hasTracked, "tracked label should be removed")

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSafetyObservation)
	require.NotNil(t, cond, "listed snapshot still had tracking when classified")
	assert.Equal(t, attunev1alpha1.ReasonSafetyEvaluating, cond.Reason)
}

func TestCheckPendingSafetyObservations_CleanupPatchFailureKeepsPending(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-fail-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(safetyTestDeploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if p, ok := obj.(*corev1.Pod); ok && p.Name == "cleanup-fail-pod" {
					return fmt.Errorf("simulated tracking cleanup patch failure")
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))

	assert.True(t, pending, "cleanup patch failure must keep observations pending")
	assert.Equal(t, before+1, after, "safety_observation should increment on cleanup patch error")

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "cleanup-fail-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.True(t, hasResizedAt, "tracking annotations must remain after cleanup patch failure")
}

func TestCheckPendingSafetyObservations_MalformedAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "not-a-quantity", // malformed
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	// Should not panic when the annotation value is unparseable.
	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	})

	// Annotations should still be present since the pod was skipped due to parse error.
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "bad-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.True(t, hasResizedAt, "annotations should remain after parse error")
}

func TestCheckPendingSafetyObservations_MissingPolicyAnnotationIgnored(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-policy-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", RestartCount: 0}},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "missing-policy-pod", Namespace: "default"}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations[annotationResizedAt]
	assert.True(t, has, "pod without policy annotation should be ignored")

	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		assert.False(t, a.GetVerb() == "update" && a.GetSubresource() == "resize", "ignored pod must not be reverted")
	}
}

func TestCheckPendingSafetyObservations_MismatchedPolicyAnnotationIgnored(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-policy-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "other-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", RestartCount: 0}},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "other-policy-pod", Namespace: "default"}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations[annotationResizedAt]
	assert.True(t, has, "pod owned by another policy should be ignored")

	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		assert.False(t, a.GetVerb() == "update" && a.GetSubresource() == "resize", "pod owned by another policy must not be reverted")
	}
}

func TestCheckPendingSafetyObservations_NotElapsed(t *testing.T) {
	// Just resized -- observation period has NOT elapsed yet.
	resizedAt := time.Now().UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recent-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Must report pending so the reconciler requeues at the observation
	// interval instead of the (much longer) cooldown.
	assert.True(t, pending, "should report observations pending when observation period not elapsed")

	// Verify annotations are still present (observation period not elapsed).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "recent-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.True(t, hasResizedAt, "annotations should remain when observation period not elapsed")
}

func TestCheckPendingSafetyObservations_EarlyCriticalOOMKill(t *testing.T) {
	// Pod was resized very recently (observation period NOT elapsed) but has
	// already been OOMKilled. The early critical detection should catch this
	// and trigger a revert immediately.
	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "main",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(time.Now()),
						},
					},
				},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	reconciler, _ := newSafetyTestReconciler(pod)

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.True(t, pending, "should still report pending (for annotation cleanup)")

	// Verify UpdateResize was called to revert the pod.
	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.True(t, foundResize, "OOMKill during observation period should trigger early revert")
}

func TestCheckPendingSafetyObservations_EarlyCriticalOOMKill_RestoresAfterSuccessfulResizeTemplate(t *testing.T) {
	t.Parallel()

	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-restore-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "1Gi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "main",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(time.Now()),
						},
					},
				},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	reconciler, fakeClient := newResizeReconciler(pod, deploy)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.True(t, foundResize, "OOMKill during observation period should trigger early revert")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Cpu().Equal(resource.MustParse("500m")),
		"template CPU request must restore to original 500m")
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"template memory request must restore to original 512Mi")
	assert.True(t, gotRes.Limits.Cpu().Equal(resource.MustParse("1000m")),
		"template CPU limit must restore to original 1000m")
	assert.True(t, gotRes.Limits.Memory().Equal(resource.MustParse("1Gi")),
		"template memory limit must restore to original 1Gi")
}

func TestCheckPendingSafetyObservations_EarlyCriticalOOMKillNotConfirmed(t *testing.T) {
	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	listed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-stale",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "main",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(time.Now()),
						},
					},
				},
			},
		},
	}
	live := listed.DeepCopy()
	live.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "main", RestartCount: 0},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(safetyTestDeploy, listed).Build()
	clientset := kubefake.NewSimpleClientset(live)
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	for _, a := range clientset.Actions() {
		assert.False(t, a.GetVerb() == "update" && a.GetSubresource() == "resize",
			"stale listed OOM must not revert after live confirm is clean")
	}
}

func TestCheckPendingSafetyObservations_EarlyCriticalHealthySkipped(t *testing.T) {
	// Pod was resized recently and is healthy. Early critical check should
	// NOT trigger a revert even though the observation period hasn't elapsed.
	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "healthy-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	reconciler, _ := newSafetyTestReconciler(pod)

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.True(t, pending, "should report pending (observation period not elapsed)")

	// Verify NO revert was triggered.
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("healthy pod during observation period should NOT trigger a revert")
		}
	}
}

func TestCheckPendingSafetyObservations_EarlyNotReadyDoesNotRevert(t *testing.T) {
	// Ready=False during the observation window is not a critical status.
	// Early path uses CheckCriticalStatuses only; full CheckPodObject would revert.
	resizedAt := time.Now().UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notready-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: false, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	reconciler, _ := newSafetyTestReconciler(pod)

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.True(t, pending, "should report pending (observation period not elapsed)")

	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("Ready=False during observation period should NOT trigger a revert")
		}
	}
}

func TestCheckPendingSafetyObservations_EarlyCriticalConfirmGet500DoesNotRevert(t *testing.T) {
	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-confirm-500",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "main",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(time.Now()),
						},
					},
				},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	reconciler, _ := newSafetyTestReconciler(pod)
	cs := reconciler.Clientset.(*kubefake.Clientset)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("injected confirm Get 500"))
	})

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("early OOM confirm Get 500 must not revert")
		}
	}
}

func TestCheckPendingSafetyObservations_EarlyCriticalConfirmGet404DoesNotRevert(t *testing.T) {
	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-confirm-404",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "1Gi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "main",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(time.Now()),
						},
					},
				},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	reconciler, fakeClient := newResizeReconciler(pod, deploy)
	cs := reconciler.Clientset.(*kubefake.Clientset)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), "oom-confirm-404")
	})

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("early OOM confirm Get 404 must not revert")
		}
	}

	var gotPod corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "oom-confirm-404", Namespace: "default",
	}, &gotPod))
	_, hasTracking := gotPod.Annotations[annotationResizedAt]
	assert.True(t, hasTracking, "tracking annotations must remain after confirm Get 404")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Cpu().Equal(resource.MustParse("250m")),
		"template CPU request must stay at rec 250m, not original 500m")
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"template memory request must stay at rec 256Mi, not original 512Mi")
}

// ---------- checkPendingSafetyObservations additional paths ----------

func TestCheckPendingSafetyObservations_MalformedTimestamp(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-ts-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   "not-a-timestamp",
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	})

	// Annotations should remain since the pod was skipped due to timestamp parse error.
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "bad-ts-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations["attune.io/resized-at"]
	assert.True(t, has, "annotations should remain after timestamp parse error")
}

func TestCheckPendingSafetyObservations_MalformedMemoryAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-mem-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "not-a-quantity",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	})

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "bad-mem-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations["attune.io/resized-at"]
	assert.True(t, has, "annotations should remain after memory parse error")
}

func TestCheckPendingSafetyObservations_CustomObservationPeriod(t *testing.T) {
	// Resized 2 minutes ago. With a custom observation period of 1 minute,
	// the period has elapsed and the pod should be checked.
	resizedAt := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-period-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        33,
		ObservationPeriod: metav1.Duration{Duration: 1 * time.Minute},
	}

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "custom-period-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations["attune.io/resized-at"]
	assert.False(t, has, "annotations should be removed after observation completes")
}

func TestCheckPendingSafetyObservations_ThrottleDeferredKeepsAnnotations(t *testing.T) {
	// When the observation period (1 min) is shorter than the throttle grace
	// window (5 min), the first deferred check should NOT remove tracking
	// annotations because the throttle check was skipped. This prevents the
	// bug where observationPeriod < throttleGrace permanently bypasses
	// throttle safety.
	resizedAt := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "throttle-deferred-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        33,
		ObservationPeriod: metav1.Duration{Duration: 1 * time.Minute},
	}

	reconciler, fakeClient := newSafetyTestReconciler(pod)
	// Pass a collector that implements ThrottleChecker so the safety monitor
	// has a throttle checker configured. The ratio value doesn't matter here
	// because the grace period will prevent the check from running.
	collector := &mockThrottleCollector{throttleRatio: 0.9}

	reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "throttle-deferred-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations["attune.io/resized-at"]
	assert.True(t, has, "annotations should be KEPT because throttle check was deferred")
}

func TestCheckPendingSafetyObservations_ThrottleDeferredLifecycle(t *testing.T) {
	// Full lifecycle: observation period (1 min) < throttle grace (5 min).
	// Pass 1 (T=2min): throttle deferred, annotations kept.
	// Pass 2 (T=6min): throttle grace elapsed, high throttle detected, pod reverted.
	resizeTime := time.Now().Add(-6 * time.Minute) // set to 6min ago for clock injection
	resizedAt := resizeTime.UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lifecycle-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        33,
		ObservationPeriod: metav1.Duration{Duration: 1 * time.Minute},
	}

	reconciler, fakeClient := newSafetyTestReconciler(pod)
	collector := &mockThrottleCollector{throttleRatio: 0.9} // very high throttle

	// Pass 1: Set clock to 2 minutes after resize (within throttle grace).
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(2 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.True(t, pending, "pass 1: should report observations pending (throttle deferred)")

	var pass1Pod corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "lifecycle-pod", Namespace: "default",
	}, &pass1Pod)
	require.NoError(t, err)
	_, has := pass1Pod.Annotations["attune.io/resized-at"]
	assert.True(t, has, "pass 1: annotations should be kept")

	// Pass 2: Advance clock to 6 minutes after resize (past throttle grace).
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(6 * time.Minute) })

	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending, "pass 2: should not report pending (throttle check completed)")

	// Verify the pod was reverted (UpdateResize called with original resources).
	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.True(t, foundResize, "pass 2: pod should be reverted due to high throttle")
}

func TestCheckPendingSafetyObservations_NilClientset(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	policy := newTestPolicy("test-policy", "default")
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:   attunev1alpha1.ConditionSafetyObservation,
		Status: metav1.ConditionTrue,
		Reason: attunev1alpha1.ReasonSafetyEvaluating,
	})

	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	})
	assert.Nil(t, meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSafetyObservation),
		"nil Clientset must clear a stale SafetyObservation condition")
}

func TestCheckPendingSafetyObservations_UnsafeVerdictReverts(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsafe-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")

	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify annotations were removed (observation complete).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "unsafe-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations["attune.io/resized-at"]
	assert.False(t, has, "tracking annotations should be removed after observation completes")

	// Verify the actual revert UpdateResize was issued (not just annotation cleanup).
	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			reverted := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpu := reverted.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, cpu.Equal(resource.MustParse("500m")),
				"CPU should be reverted to original 500m, got %s", cpu.String())
		}
	}
	assert.True(t, foundResize, "should have called UpdateResize to revert the pod")
}

func TestCheckPendingSafetyObservations_UnsafeVerdictReverts_RestoresAfterSuccessfulResizeTemplate(t *testing.T) {
	t.Parallel()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsafe-restore-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "1Gi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	reconciler, fakeClient := newResizeReconciler(pod, deploy)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			reverted := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpu := reverted.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, cpu.Equal(resource.MustParse("500m")),
				"CPU should be reverted to original 500m, got %s", cpu.String())
		}
	}
	assert.True(t, foundResize, "should have called UpdateResize to revert the pod")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Cpu().Equal(resource.MustParse("500m")),
		"template CPU request must restore to original 500m")
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"template memory request must restore to original 512Mi")
	assert.True(t, gotRes.Limits.Cpu().Equal(resource.MustParse("1000m")),
		"template CPU limit must restore to original 1000m")
	assert.True(t, gotRes.Limits.Memory().Equal(resource.MustParse("1Gi")),
		"template memory limit must restore to original 1Gi")
}

func TestCheckPendingSafetyObservations_RestoreTemplateFailsKeepsTrackingThenRetries(t *testing.T) {
	t.Parallel()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-fail-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "1Gi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	var failDeployPatch atomic.Bool
	failDeployPatch.Store(true)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && failDeployPatch.Load() {
					return fmt.Errorf("simulated template patch failure")
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})
	assert.True(t, pending, "failed template restore must keep observations pending")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"template must stay at the recommended 256Mi after a failed restore")

	var gotPod corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "restore-fail-pod", Namespace: "default",
	}, &gotPod))
	_, hasTracking := gotPod.Annotations[annotationResizedAt]
	assert.True(t, hasTracking, "tracking annotations must remain so the next reconcile retries restore")

	failDeployPatch.Store(false)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes = gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"second restore must write the original 512Mi snapshot")
}

func TestCheckPendingSafetyObservations_RestoreRetryAfterRevertWhenPodNowSafe(t *testing.T) {
	t.Parallel()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-retry-safe-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "1Gi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	var failDeployPatch atomic.Bool
	failDeployPatch.Store(true)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && failDeployPatch.Load() {
					return fmt.Errorf("simulated template patch failure")
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})
	assert.True(t, pending, "failed template restore must keep observations pending")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"template must stay at the recommended 256Mi after a failed restore")

	var gotPod corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "restore-retry-safe-pod", Namespace: "default",
	}, &gotPod))
	_, hasTracking := gotPod.Annotations[annotationResizedAt]
	assert.True(t, hasTracking, "tracking annotations must remain so the next reconcile retries restore")

	// Revert already landed; kubelet recovered. Informer and live Get now
	// show Ready at the original snapshot, so CheckPodObject is Safe.
	originalResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
	gotPod.Spec.Containers[0].Resources = originalResources
	gotPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	require.NoError(t, fakeClient.Update(context.Background(), &gotPod))

	live, err := clientset.CoreV1().Pods("default").Get(context.Background(), "restore-retry-safe-pod", metav1.GetOptions{})
	require.NoError(t, err)
	live.Spec.Containers[0].Resources = originalResources
	live.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	_, err = clientset.CoreV1().Pods("default").Update(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = clientset.CoreV1().Pods("default").UpdateStatus(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	failDeployPatch.Store(false)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes = gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"safe-path restore retry must write the original 512Mi snapshot")
}

func TestCheckPendingSafetyObservations_RestoreRetryWhenLiveMatchesClampedRevert(t *testing.T) {
	t.Parallel()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	notRequiredMem := []corev1.ContainerResizePolicy{
		{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
		{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
	}
	// Rec / live after resize: higher memory limit than the original snapshot.
	// RevertPod(allowInPlace=false) keeps this limit when policy is NotRequired.
	postResize := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-retry-clamped-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "128Mi",
				"attune.io/original-cpu-limit.main":      "1000m",
				"attune.io/original-memory-limit.main":   "256Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:         "main",
					Image:        "nginx",
					Resources:    postResize,
					ResizePolicy: notRequiredMem,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	deploy := recSizedAPIServerDeploy()
	var failDeployPatch atomic.Bool
	failDeployPatch.Store(true)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && failDeployPatch.Load() {
					return fmt.Errorf("simulated template patch failure")
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})
	assert.True(t, pending, "failed template restore must keep observations pending")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"template must stay at the recommended 256Mi after a failed restore")

	var gotPod corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "restore-retry-clamped-pod", Namespace: "default",
	}, &gotPod))
	_, hasTracking := gotPod.Annotations[annotationResizedAt]
	assert.True(t, hasTracking, "tracking annotations must remain so the next reconcile retries restore")

	// Revert already landed. Live memory limit stays at the post-resize
	// value (ClampMemoryLimitForPolicy, allowInPlace=false). Guaranteed
	// QoS raises the memory request to that clamped limit. CPU is original.
	clampedRevert := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	gotPod.Spec.Containers[0].Resources = clampedRevert
	gotPod.Spec.Containers[0].ResizePolicy = notRequiredMem
	gotPod.Status.QOSClass = corev1.PodQOSGuaranteed
	gotPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	require.NoError(t, fakeClient.Update(context.Background(), &gotPod))

	live, err := clientset.CoreV1().Pods("default").Get(context.Background(), "restore-retry-clamped-pod", metav1.GetOptions{})
	require.NoError(t, err)
	live.Spec.Containers[0].Resources = clampedRevert
	live.Spec.Containers[0].ResizePolicy = notRequiredMem
	live.Status.QOSClass = corev1.PodQOSGuaranteed
	live.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	_, err = clientset.CoreV1().Pods("default").Update(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = clientset.CoreV1().Pods("default").UpdateStatus(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})
	assert.True(t, pending, "restore still failing; keep pending")
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSafetyObservation)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonSafetyRestorePending, cond.Reason)
	assert.Contains(t, cond.Message, "restorePending=1")

	failDeployPatch.Store(false)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes = gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("128Mi")),
		"restore must write the original 128Mi request, not the raised revert request")
	assert.True(t, gotRes.Limits.Memory().Equal(resource.MustParse("256Mi")),
		"restore must write the original 256Mi limit, not the clamped live 512Mi")
}

func TestCheckPendingSafetyObservations_RestoreRetryLiveGetErrorKeepsTracking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		getErr  error
		podName string
	}{
		{
			name:    "get500",
			getErr:  apierrors.NewInternalError(fmt.Errorf("injected restore Get 500")),
			podName: "restore-retry-get-500",
		},
		{
			name:    "get404",
			getErr:  apierrors.NewNotFound(corev1.Resource("pods"), "restore-retry-get-404"),
			podName: "restore-retry-get-404",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			// Listed already matches the original snapshot so ignoring a
			// live Get error would restore the AfterSuccessfulResize template.
			original := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1000m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.podName,
					Namespace: "default",
					Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
					Annotations: map[string]string{
						"attune.io/resized-at":                   resizedAt,
						"attune.io/resized-workload":             "api-server",
						"attune.io/resized-containers":           "main",
						"attune.io/original-cpu-request.main":    "500m",
						"attune.io/original-memory-request.main": "512Mi",
						"attune.io/original-cpu-limit.main":      "1000m",
						"attune.io/original-memory-limit.main":   "1Gi",
						"attune.io/policy":                       "test-policy",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      "main",
							Image:     "nginx",
							Resources: original,
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "main", RestartCount: 0},
					},
				},
			}

			deploy := recSizedAPIServerDeploy()
			reconciler, fakeClient := newResizeReconciler(pod, deploy)
			cs := reconciler.Clientset.(*kubefake.Clientset)
			cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.getErr
			})

			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
			policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
			policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
				Enabled: boolPtr(true),
				When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
			}

			pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, []client.Object{deploy})
			assert.True(t, pending, "live Get error must keep observations pending")

			var gotPod corev1.Pod
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
				Name: tt.podName, Namespace: "default",
			}, &gotPod))
			_, hasTracking := gotPod.Annotations[annotationResizedAt]
			assert.True(t, hasTracking, "tracking must remain when live Get fails")

			var gotDeploy appsv1.Deployment
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
				Name: "api-server", Namespace: "default",
			}, &gotDeploy))
			gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
			assert.True(t, gotRes.Requests.Cpu().Equal(resource.MustParse("250m")),
				"template CPU must stay at rec 250m, not listed original 500m")
			assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("256Mi")),
				"template memory must stay at rec 256Mi, not listed original 512Mi")
		})
	}
}

func TestCheckPendingSafetyObservations_NotReadyFlapConfirmedBeforeRevert(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	listed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flap-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}
	live := listed.DeepCopy()
	live.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}

	policy := newTestPolicy("test-policy", "default")
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(safetyTestDeploy, listed).Build()
	clientset := kubefake.NewSimpleClientset(live)
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	for _, a := range clientset.Actions() {
		assert.False(t, a.GetVerb() == "update" && a.GetSubresource() == "resize",
			"flapping notready must not revert after live confirm")
	}
}

func TestCheckPendingSafetyObservations_ConfirmGet500DoesNotRevert(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "confirm-500-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)
	cs := reconciler.Clientset.(*kubefake.Clientset)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("injected confirm Get 500"))
	})

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))

	assert.True(t, pending, "confirm Get 500 should keep observation pending")
	assert.Equal(t, before+1, after, "safety_observation should increment on confirm Get error")

	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("confirm Get 500 must not revert")
		}
	}
}

func TestCheckPendingSafetyObservations_ConfirmGetNotFoundDoesNotRevert(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "confirm-404-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)
	cs := reconciler.Clientset.(*kubefake.Clientset)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), ga.GetName())
	})

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("confirm Get 404 must not revert")
		}
	}
}

func TestCheckPendingSafetyObservations_MultiContainerNoPerContainerGet(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                      resizedAt,
				"attune.io/resized-workload":                "api-server",
				"attune.io/resized-containers":              "app,sidecar",
				"attune.io/original-cpu-request.app":        "500m",
				"attune.io/original-memory-request.app":     "512Mi",
				"attune.io/original-cpu-request.sidecar":    "100m",
				"attune.io/original-memory-request.sidecar": "128Mi",
				"attune.io/policy":                          "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
				{Name: "sidecar", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	gets := 0
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "pods" {
			gets++
		}
	}
	assert.Equal(t, 0, gets, "safe multi-container observation must not Get per container")
}

func TestCheckPendingSafetyObservations_RevertUsesWithoutCancel(t *testing.T) {
	// PromQL can spend the whole PrometheusTimeout. RevertPod must not
	// inherit that dead deadline (same class as persist confirm).
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cancelled-ctx-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)
	reconciler.Clientset = &cancelAwareResizeClientset{Interface: reconciler.Clientset}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconciler.checkPendingSafetyObservations(ctx, policy, nil, safetyWorkloads())

	var foundResize bool
	for _, a := range reconciler.Clientset.(*cancelAwareResizeClientset).Interface.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.True(t, foundResize, "revert must still UpdateResize after a spent Prometheus deadline")
}

type cancelAwareResizeClientset struct {
	kubernetes.Interface
}

func TestCheckPendingSafetyObservations_UnsafeVerdictMarksHistoryReverted(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "history-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse}, // triggers unsafe verdict
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	// Pre-populate resize history with a Success entry that should become Reverted.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "cpu",
			From:      "500m",
			To:        "250m",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "memory",
			From:      "512Mi",
			To:        "256Mi",
			Result:    attunev1alpha1.ResizeResultSuccess,
		},
	}

	reconciler, _ := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify that matching Success entries were marked as Reverted with reason.
	for _, h := range policy.Status.ResizeHistory {
		assert.Equal(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"history entry %s/%s should be Reverted, got %s", h.Workload, h.Container, h.Result)
		assert.NotEmpty(t, h.Reason,
			"history entry %s/%s should have a revert reason", h.Workload, h.Container)
	}
}

func TestCheckPendingSafetyObservations_UnsafeVerdictEmitsEvent(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsafe-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/policy":                       "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify a Reverted event was emitted containing the pod name.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Reverted")
		assert.Contains(t, event, "unsafe-pod")
	default:
		t.Fatal("expected a Reverted event but channel was empty")
	}
}

func TestCheckPendingSafetyObservations_RestartCountParsed(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restart-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                        resizedAt,
				"attune.io/resized-workload":                  "api-server",
				"attune.io/resized-containers":                "main",
				"attune.io/original-cpu-request.main":         "500m",
				"attune.io/original-memory-request.main":      "512Mi",
				annotationOriginalRestartCountPrefix + "main": "3",
				"attune.io/policy":                            "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				// RestartCount 4 is within threshold (baseline 3 + 2 = 5),
				// so the pod should be considered safe and annotations removed.
				{Name: "main", RestartCount: 4},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.False(t, foundResize, "safe restart count must not UpdateResize")

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "restart-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.False(t, hasResizedAt, "safe pod should have annotations removed")
}

func TestCheckPendingSafetyObservations_RestartCountExceeded(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crashing-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                        resizedAt,
				"attune.io/resized-workload":                  "api-server",
				"attune.io/resized-containers":                "main",
				"attune.io/original-cpu-request.main":         "500m",
				"attune.io/original-memory-request.main":      "512Mi",
				annotationOriginalRestartCountPrefix + "main": "3",
				"attune.io/policy":                            "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				// RestartCount 5 >= baseline 3 + 2: triggers revert.
				{Name: "main", RestartCount: 5},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, _ := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify UpdateResize (revert) was called.
	var found bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			found = true
		}
	}
	assert.True(t, found, "should have reverted pod with excessive restarts")
}

func TestCheckPendingSafetyObservations_InvalidRestartCount(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-annotation-pod",
			Namespace: "default",
			Labels:    map[string]string{"attune.io/tracked": "true"},
			Annotations: map[string]string{
				"attune.io/resized-at":                        resizedAt,
				"attune.io/resized-workload":                  "api-server",
				"attune.io/resized-containers":                "main",
				"attune.io/original-cpu-request.main":         "500m",
				"attune.io/original-memory-request.main":      "512Mi",
				annotationOriginalRestartCountPrefix + "main": "not-a-number",
				"attune.io/policy":                            "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				// Parse of original-restart-count.<container> fails; the
				// observation skips this pod and keeps tracking annotations.
				{Name: "main", RestartCount: 1},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))
	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))
	assert.Equal(t, before+1, after, "safety_observation should increment on restart-count parse error")

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "bad-annotation-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	// Invalid restart count is a parse error; skip the pod and keep annotations.
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.True(t, hasResizedAt, "parse error must keep tracking annotations")
}

func TestCheckPendingSafetyObservations_ListErrorIncrementsCounter(t *testing.T) {
	scheme := testScheme()
	// Use an interceptor to make List return an error for pods.
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()

	r := NewAttunePolicyReconciler()
	r.Client = failingClient
	r.Scheme = scheme
	r.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))

	r.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation"))
	assert.Equal(t, before+1, after, "safety_observation error counter should increment on List failure")
}

// --- Issue #658: annotation cleanup is a merge patch (no Get, no 409 loop) ---

func TestCheckPendingSafetyObservations_AnnotationCleanupPatch(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-abc-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "api-server", labelTracked: "true"},
			Annotations: map[string]string{
				annotationResizedAt:                     resizedAt.UTC().Format(time.RFC3339),
				annotationResizedContainers:             "main",
				annotationOriginalCPUPrefix + "main":    "500m",
				annotationOriginalMemoryPrefix + "main": "512Mi",
				annotationPolicy:                        "test-policy",
				annotationResizedWorkload:               "api-server",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("750m"),
							corev1.ResourceMemory: resource.MustParse("384Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 0},
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	var observing atomic.Bool
	observing.Store(true)
	var patchCalls atomic.Int32
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if p, ok := obj.(*corev1.Pod); ok && p.Name == pod.Name && p.Namespace == pod.Namespace {
					require.Equal(t, types.MergePatchType, patch.Type(), "cleanup must use a merge patch")
					patchCalls.Add(1)
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
			Get: func(ctx context.Context, cw client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && observing.Load() {
					t.Errorf("cleanup must not Get the pod via the controller-runtime client")
					return fmt.Errorf("unexpected Get on pod %s/%s", key.Namespace, key.Name)
				}
				return cw.Get(ctx, key, obj, opts...)
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				if p, ok := obj.(*corev1.Pod); ok && observing.Load() {
					t.Errorf("cleanup must not Update the pod via the controller-runtime client")
					return fmt.Errorf("unexpected Update on pod %s/%s", p.Namespace, p.Name)
				}
				return fmt.Errorf("unexpected Update on %T", obj)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 5 * time.Minute}

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, []client.Object{deploy})
	observing.Store(false)
	assert.False(t, pending, "should not be pending after successful cleanup")
	assert.Greater(t, patchCalls.Load(), int32(0), "cleanup must Patch the tracked pod")

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations[annotationResizedAt]
	assert.False(t, hasResizedAt, "tracking annotations should be removed after cleanup")
	_, hasContainers := updated.Annotations[annotationResizedContainers]
	assert.False(t, hasContainers)
	_, hasPolicy := updated.Annotations[annotationPolicy]
	assert.False(t, hasPolicy)
	_, hasOrigCPU := updated.Annotations[annotationOriginalCPUPrefix+"main"]
	assert.False(t, hasOrigCPU)
	_, hasOrigMem := updated.Annotations[annotationOriginalMemoryPrefix+"main"]
	assert.False(t, hasOrigMem)
	_, hasTracked := updated.Labels[labelTracked]
	assert.False(t, hasTracked, "tracked label should be removed after cleanup")
}

func TestCheckPendingSafetyObservations_ListErrorReturnsObservationsPending(t *testing.T) {
	// When the pod list fails, observationsPending must be true (fail-safe)
	// so the reconciler requeues at the short observation interval instead
	// of the full cooldown. This prevents a delayed safety detection window.
	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()

	r := NewAttunePolicyReconciler()
	r.Client = failingClient
	r.Scheme = scheme
	r.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")

	pending := r.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	assert.True(t, pending,
		"observationsPending should be true on List error (fail-safe requeue)")
}

func TestCheckPendingSafetyObservations_ListErrorClearsStaleCondition(t *testing.T) {
	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()

	r := NewAttunePolicyReconciler()
	r.Client = failingClient
	r.Scheme = scheme
	r.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    attunev1alpha1.ConditionSafetyObservation,
		Status:  metav1.ConditionTrue,
		Reason:  attunev1alpha1.ReasonSafetyEvaluating,
		Message: "2 pod(s) in safety lifecycle (observing=0 evaluating=2 restorePending=0 incomplete=0)",
	})

	pending := r.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.True(t, pending)
	assert.Nil(t, meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSafetyObservation),
		"List failure must not keep a stale SafetyObservation count")
}
