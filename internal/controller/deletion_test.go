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
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestHandleDeletion_CleansAnnotationsAndGauges(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: stringPtr("app")},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
			},
		},
	}

	// Pod managed by this policy.
	managedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-abc",
			Namespace: "default",
			Labels:    map[string]string{labelTracked: "true"},
			Annotations: map[string]string{
				annotationPolicy:                        "my-policy",
				annotationResizedAt:                     "2025-01-01T00:00:00Z",
				annotationResizedWorkload:               "app",
				annotationResizedContainers:             "main",
				annotationOriginalCPUPrefix + "main":    "100m",
				annotationOriginalMemoryPrefix + "main": "128Mi",
				annotationStartupBoostAt:                "2025-01-01T00:00:00Z",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, managedPod).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	// Seed gauge keys.
	operatormetrics.RecommendationCPU.WithLabelValues("default", "app", "main").Set(0.5)
	reconciler.gaugeKeys.Store("default/my-policy", []gaugeKey{
		{Namespace: "default", Workload: "app", Container: "main"},
	})
	reconciler.increaseRates.Store("default/my-policy", newIncreaseRateBucket(100, 100, time.Now()))
	reconciler.lastBlockerRefresh.Store(types.NamespacedName{Namespace: "default", Name: "my-policy"}, time.Now())

	result, err := reconciler.handleDeletion(context.Background(), policy)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Pod should be cleaned.
	var pod corev1.Pod
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "app-abc", Namespace: "default"}, &pod)
	require.NoError(t, err)
	assert.Empty(t, pod.Annotations[annotationPolicy], "annotationPolicy should be removed")
	assert.Empty(t, pod.Annotations[annotationResizedAt], "annotationResizedAt should be removed")
	assert.Empty(t, pod.Annotations[annotationResizedWorkload], "annotationResizedWorkload should be removed")
	assert.Empty(t, pod.Annotations[annotationResizedContainers], "annotationResizedContainers should be removed")
	assert.Empty(t, pod.Annotations[annotationStartupBoostAt], "annotationStartupBoostAt should be removed")
	assert.Empty(t, pod.Annotations[annotationOriginalCPUPrefix+"main"], "original CPU annotation should be removed")
	assert.Empty(t, pod.Annotations[annotationOriginalMemoryPrefix+"main"], "original memory annotation should be removed")
	assert.Empty(t, pod.Labels[labelTracked], "labelTracked should be removed")

	// Gauges should be cleaned.
	_, loaded := reconciler.gaugeKeys.Load("default/my-policy")
	assert.False(t, loaded, "gauge keys should be deleted")
	_, loaded = reconciler.increaseRates.Load("default/my-policy")
	assert.False(t, loaded, "increaseRates should be deleted")
	_, loaded = reconciler.lastBlockerRefresh.Load(types.NamespacedName{Namespace: "default", Name: "my-policy"})
	assert.False(t, loaded, "lastBlockerRefresh should be deleted")

	// Finalizer should be removed.
	assert.NotContains(t, policy.Finalizers, finalizerName,
		"finalizer should be removed after cleanup")
}

func TestHandleDeletion_CleansNamespaceSavingsGauges(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "only-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: stringPtr("app")},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
			},
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	// Seed per-workload gauge keys so handleDeletion can clean them.
	operatormetrics.RecommendationCPU.WithLabelValues("default", "app", "main").Set(0.5)
	reconciler.gaugeKeys.Store("default/only-policy", []gaugeKey{
		{Namespace: "default", Workload: "app", Container: "main"},
	})

	// Seed namespace-level savings gauges.
	operatormetrics.SavingsCPU.WithLabelValues("default").Set(1.0)
	operatormetrics.SavingsMemory.WithLabelValues("default").Set(1024)
	operatormetrics.SavingsEstimatedMonthly.WithLabelValues("default").Set(42.5)

	// Verify gauges are populated before deletion.
	require.Equal(t, 1, promtestutil.CollectAndCount(operatormetrics.SavingsCPU),
		"savings CPU gauge should exist before deletion")
	require.Equal(t, 1, promtestutil.CollectAndCount(operatormetrics.SavingsMemory),
		"savings memory gauge should exist before deletion")
	require.Equal(t, 1, promtestutil.CollectAndCount(operatormetrics.SavingsEstimatedMonthly),
		"savings estimated monthly gauge should exist before deletion")

	result, err := reconciler.handleDeletion(context.Background(), policy)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Namespace-level savings gauges should be deleted (not just zeroed)
	// because this was the last policy in the namespace.
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.SavingsCPU),
		"savings CPU gauge should be deleted after last policy removal")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.SavingsMemory),
		"savings memory gauge should be deleted after last policy removal")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.SavingsEstimatedMonthly),
		"savings estimated monthly gauge should be deleted after last policy removal")

	// Finalizer should be removed.
	assert.NotContains(t, policy.Finalizers, finalizerName,
		"finalizer should be removed after cleanup")
}

func TestHandleDeletion_SkipsPodsFromOtherPolicy(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: stringPtr("app")},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
			},
		},
	}

	// Pod managed by a DIFFERENT policy.
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-app-xyz",
			Namespace: "default",
			Labels:    map[string]string{labelTracked: "true"},
			Annotations: map[string]string{
				annotationPolicy:          "other-policy",
				annotationResizedAt:       "2025-01-01T00:00:00Z",
				annotationResizedWorkload: "other-app",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, otherPod).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	result, err := reconciler.handleDeletion(context.Background(), policy)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Other policy's pod should NOT be touched.
	var pod corev1.Pod
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "other-app-xyz", Namespace: "default"}, &pod)
	require.NoError(t, err)
	assert.Equal(t, "other-policy", pod.Annotations[annotationPolicy],
		"other policy's annotation should be untouched")
	assert.Equal(t, "true", pod.Labels[labelTracked],
		"other policy's tracked label should be untouched")
	assert.Equal(t, "2025-01-01T00:00:00Z", pod.Annotations[annotationResizedAt],
		"other policy's resize timestamp should be untouched")
}

func TestHandleDeletion_SkipsWithoutFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			DeletionTimestamp: &now,
			// No finalizer -- handleDeletion should be a no-op.
		},
	}

	// Build reconciler without seeding the deleted policy into the client
	// (fake client rejects deletionTimestamp without finalizers).
	reconciler := newReconcilerWithClient()
	result, err := reconciler.handleDeletion(context.Background(), policy)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "should return immediately without finalizer")
}

func TestHandleDeletion_ListErrorRetainsFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
	}

	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = failingClient
	reconciler.Scheme = scheme
	_, err := reconciler.handleDeletion(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing tracked pods")
	assert.Contains(t, policy.Finalizers, finalizerName,
		"finalizer must remain so controller retries cleanup")
}

func TestHandleDeletion_ContinuesOnPodUpdateError(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: stringPtr("app")},
		},
	}

	// Two managed pods: first will fail on Patch, second should still be cleaned.
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-fail", Namespace: "default",
			Labels:      map[string]string{labelTracked: "true"},
			Annotations: map[string]string{annotationPolicy: "my-policy", annotationResizedAt: "2025-01-01T00:00:00Z"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-ok", Namespace: "default",
			Labels:      map[string]string{labelTracked: "true"},
			Annotations: map[string]string{annotationPolicy: "my-policy", annotationResizedAt: "2025-01-01T00:00:00Z"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, pod1, pod2).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "pod-fail" {
					return fmt.Errorf("simulated conflict")
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	_, err := reconciler.handleDeletion(context.Background(), policy)
	require.Error(t, err, "should return error for failed pod cleanup")
	assert.Contains(t, err.Error(), "pod-fail")

	// pod2 should still be cleaned despite pod1's failure.
	var cleaned corev1.Pod
	getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: "pod-ok", Namespace: "default"}, &cleaned)
	require.NoError(t, getErr)
	assert.Empty(t, cleaned.Annotations[annotationPolicy], "pod-ok should be cleaned despite pod-fail error")
	assert.Empty(t, cleaned.Labels[labelTracked], "pod-ok tracked label should be removed")

	// Finalizer should still be present (error returned, retry needed).
	assert.Contains(t, policy.Finalizers, finalizerName,
		"finalizer must remain so controller retries for pod-fail")
}

func TestHandleDeletion_PodDeletedBetweenListAndPatch(t *testing.T) {
	now := metav1.Now()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-policy",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: stringPtr("app")},
		},
	}

	vanishingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vanishing-pod", Namespace: "default",
			Labels:      map[string]string{labelTracked: "true"},
			Annotations: map[string]string{annotationPolicy: "my-policy", annotationResizedAt: "2025-01-01T00:00:00Z"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, vanishingPod).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return apierrors.NewNotFound(corev1.Resource("pods"), obj.GetName())
				}
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	result, err := reconciler.handleDeletion(context.Background(), policy)
	require.NoError(t, err, "IsNotFound on pod patch should not cause error")
	assert.Equal(t, ctrl.Result{}, result)
	assert.NotContains(t, policy.Finalizers, finalizerName,
		"finalizer should be removed even if pod vanished")
}
