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
	"math"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/conflict"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/recommendation"
	"github.com/attune-io/attune/internal/resize"
)

// testScheme returns a runtime.Scheme with all needed types registered.
func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = attunev1alpha1.AddToScheme(scheme)
	return scheme
}

// int32Ptr returns a pointer to an int32.
func int32Ptr(i int32) *int32 {
	return &i
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// stringPtr returns a pointer to a string.
func stringPtr(s string) *string {
	return &s
}

// boolPtr returns a pointer to a bool.
func boolPtr(b bool) *bool {
	return &b
}

func ptrCompletionMode(mode batchv1.CompletionMode) *batchv1.CompletionMode {
	return &mode
}

// newTestDeployment creates a Deployment for testing.
func newTestDeployment(name, namespace string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:latest",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          2,
			UpdatedReplicas:   2,
			AvailableReplicas: 2,
		},
	}
}

// newTestPod creates a Pod for testing with the given labels.
func newTestPod(name, namespace string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx:latest",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// newTestPolicy creates an AttunePolicy for testing.
func newTestPolicy(name, namespace string) *attunev1alpha1.AttunePolicy {
	return &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: stringPtr("api-server"),
			},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
				},
				MinimumDataPoints: int32Ptr(48),
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
				MinAllowed: quantityPtr("50m"),
				MaxAllowed: quantityPtr("4000m"),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
				MinAllowed: quantityPtr("64Mi"),
				MaxAllowed: quantityPtr("8Gi"),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeRecommend,
				Cooldown: &metav1.Duration{
					Duration: 1 * time.Hour,
				},
			},
		},
	}
}

// mockMetricsFactory returns a MetricsCollectorFactory that creates a mock collector.
func mockMetricsFactory(collector rsmetrics.MetricsCollector) MetricsCollectorFactory {
	return func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return collector, nil
	}
}

// mockCollector implements MetricsCollector for testing.
type mockCollector struct {
	queryRangeFunc        func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]rsmetrics.Sample, error)
	queryRangeGroupedFunc func(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]rsmetrics.Sample, error)
	queryFunc             func(ctx context.Context, query string, ts time.Time) (float64, error)
}

func (m *mockCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]rsmetrics.Sample, error) {
	if m.queryRangeFunc != nil {
		return m.queryRangeFunc(ctx, query, start, end, step)
	}
	if m.queryRangeGroupedFunc != nil {
		grouped, err := m.queryRangeGroupedFunc(ctx, query, start, end, step)
		if err != nil {
			return nil, err
		}
		var samples []rsmetrics.Sample
		for _, groupedSamples := range grouped {
			samples = append(samples, groupedSamples...)
		}
		return samples, nil
	}
	return nil, nil
}

func (m *mockCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string][]rsmetrics.Sample, error) {
	if m.queryRangeGroupedFunc != nil {
		return m.queryRangeGroupedFunc(ctx, query, start, end, step)
	}
	if m.queryRangeFunc != nil {
		samples, err := m.queryRangeFunc(ctx, query, start, end, step)
		if err != nil {
			return nil, err
		}
		return map[string][]rsmetrics.Sample{"": samples}, nil
	}
	return map[string][]rsmetrics.Sample{}, nil
}

func (m *mockCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, ts)
	}
	return 0, nil
}

// newResizePod creates a running Pod with specified resources, matching
// a deployment named deployName. Reduces the 20+ line inline Pod construction
// that repeats across executeResizes tests.
func newResizePod(deployName string, cpuReq, memReq, cpuLim, memLim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName + "-abc-1",
			Namespace: "default",
			Labels:    map[string]string{"app": deployName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuReq),
							corev1.ResourceMemory: resource.MustParse(memReq),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuLim),
							corev1.ResourceMemory: resource.MustParse(memLim),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// newResizeRecommendation creates a WorkloadRecommendation for the given
// workload with a single container. Replaces the 15+ line struct construction
// that repeats across executeResizes tests.
func newResizeRecommendation(workload, curCPU, curMem, curCPULim, curMemLim, recCPU, recMem, recCPULim, recMemLim string) attunev1alpha1.WorkloadRecommendation {
	return attunev1alpha1.WorkloadRecommendation{
		Workload: workload,
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name: "main",
				Current: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse(curCPU),
					CPULimit:      resource.MustParse(curCPULim),
					MemoryRequest: resource.MustParse(curMem),
					MemoryLimit:   resource.MustParse(curMemLim),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse(recCPU),
					CPULimit:      resource.MustParse(recCPULim),
					MemoryRequest: resource.MustParse(recMem),
					MemoryLimit:   resource.MustParse(recMemLim),
				},
			},
		},
	}
}

// newReconcilerWithClient creates an AttunePolicyReconciler with the given
// objects pre-loaded. Reduces the 5-line scheme+client+reconciler setup
// that repeats in nearly every test.
func newReconcilerWithClient(objects ...client.Object) *AttunePolicyReconciler {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	return r
}

// newReconcilerForReconcile creates a reconciler with status subresource
// support and a mock metrics factory, ready for Reconcile tests.
func newReconcilerForReconcile(mc rsmetrics.MetricsCollector, objects ...client.Object) (*AttunePolicyReconciler, client.Client) {
	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(mc)
	return r, c
}

func newReconcilerForReconcileWithClient(mc rsmetrics.MetricsCollector, c client.Client, scheme *runtime.Scheme) *AttunePolicyReconciler {
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(mc)
	return r
}

// newResizeReconciler creates a reconciler with both a controller-runtime
// fake client and a typed clientset for resize tests.
func newResizeReconciler(pod *corev1.Pod, objects ...client.Object) (*AttunePolicyReconciler, client.Client) {
	scheme := testScheme()
	allObjects := append(objects, pod)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.Clientset = clientset
	return r, c
}

// podMap builds a podsByWorkload map for use in executeResizes tests.
func podMap(workloadName string, pods ...*corev1.Pod) map[string][]corev1.Pod {
	m := make(map[string][]corev1.Pod, 1)
	for _, p := range pods {
		m[workloadName] = append(m[workloadName], *p)
	}
	return m
}

func TestDiscoverWorkloads_FindsDeploymentByName(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})
	reconciler := newReconcilerWithClient(deploy)

	policy := newTestPolicy("test-policy", "default")

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "api-server", workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsDeploymentsByLabelSelector(t *testing.T) {
	deploy1 := newTestDeployment("api-server-1", "default", map[string]string{"tier": "api"})
	deploy2 := newTestDeployment("api-server-2", "default", map[string]string{"tier": "api"})
	deploy3 := newTestDeployment("worker", "default", map[string]string{"tier": "worker"})
	reconciler := newReconcilerWithClient(deploy1, deploy2, deploy3)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "api"},
				},
			},
		},
	}

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Len(t, workloads, 2)

	names := make(map[string]bool)
	for _, w := range workloads {
		names[w.GetName()] = true
	}
	assert.True(t, names["api-server-1"])
	assert.True(t, names["api-server-2"])
	assert.False(t, names["worker"])
}

func TestDiscoverWorkloads_ReturnsEmptyForNonMatchingSelector(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})
	reconciler := newReconcilerWithClient(deploy)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "nonexistent"},
				},
			},
		},
	}

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Empty(t, workloads)
}

func TestGetPodsForWorkload_ReturnsMatchingPods(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", nil)
	pod1 := newTestPod("api-server-abc-123", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-456", "default", map[string]string{"app": "api-server"})
	pod3 := newTestPod("worker-def-789", "default", map[string]string{"app": "worker"})
	reconciler := newReconcilerWithClient(deploy, pod1, pod2, pod3)

	pods, err := reconciler.getPodsForWorkload(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	podNames := make(map[string]bool)
	for _, p := range pods {
		podNames[p.Name] = true
	}
	assert.True(t, podNames["api-server-abc-123"])
	assert.True(t, podNames["api-server-abc-456"])
	assert.False(t, podNames["worker-def-789"])
}

func TestBuildPrometheusQuery_CPU(t *testing.T) {
	query := buildPrometheusQuery("production", "api-server-[a-z0-9]+-[a-z0-9]{5}", "main", "cpu", 5*time.Minute)
	expected := `rate(container_cpu_usage_seconds_total{namespace="production",pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}",container="main"}[5m])`
	assert.Equal(t, expected, query)
}

func TestBuildPrometheusQuery_Memory(t *testing.T) {
	query := buildPrometheusQuery("production", "api-server-[a-z0-9]+-[a-z0-9]{5}", "main", "memory", 5*time.Minute)
	expected := `container_memory_working_set_bytes{namespace="production",pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}",container="main"}`
	assert.Equal(t, expected, query)
}

func TestReconcile_MissingPolicyReturnsNoError(t *testing.T) {
	reconciler := newReconcilerWithClient()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent-policy",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_PausedPolicySkipsReconciliation(t *testing.T) {
	paused := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-policy",
			Namespace:  "default",
			Finalizers: []string{"attune.io/cleanup"},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			Paused: &paused,
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: func() *string { s := "my-app"; return &s }(),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = attunev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paused-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// Paused policies should not requeue (no work to do until unpaused).
	assert.Equal(t, ctrl.Result{}, result)

	// Verify the Ready condition is set to False with reason Paused.
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "paused-policy", Namespace: "default"}, &updated))
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, readyCond, "Ready condition should be set")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, attunev1alpha1.ReasonPaused, readyCond.Reason)
	assert.Contains(t, readyCond.Message, "paused")
}

func TestReconcile_MissingPolicyCleansGauges(t *testing.T) {
	reconciler := newReconcilerWithClient()

	// Seed gauges for namespace "default" as if a prior reconcile set them.
	operatormetrics.RecommendationCPU.WithLabelValues("default", "api-server", "main").Set(0.5)
	operatormetrics.RecommendationMemory.WithLabelValues("default", "api-server", "main").Set(512 * 1024 * 1024)
	operatormetrics.Confidence.WithLabelValues("default", "api-server", "main").Set(0.9)
	operatormetrics.BurstFactor.WithLabelValues("default", "api-server", "main", "cpu").Set(1.2)

	// Simulate a prior reconcile that tracked these gauge keys.
	reconciler.gaugeKeys.Store("default/deleted-policy", []gaugeKey{
		{Namespace: "default", Workload: "api-server", Container: "main"},
	})

	// Verify gauges are set.
	require.InDelta(t, 0.5, promtestutil.ToFloat64(
		operatormetrics.RecommendationCPU.WithLabelValues("default", "api-server", "main")), 1e-9)

	// Reconcile a missing policy in "default" namespace.
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "deleted-policy",
			Namespace: "default",
		},
	}
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Gauges for this policy should be cleaned up.
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.RecommendationCPU),
		"recommendation CPU gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.RecommendationMemory),
		"recommendation memory gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.Confidence),
		"confidence gauges should be cleaned after policy deletion")
	assert.Equal(t, 0, promtestutil.CollectAndCount(operatormetrics.BurstFactor),
		"burst factor gauges should be cleaned after policy deletion")
}

func TestReconcile_AddsFinalizer(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	assert.Contains(t, updated.Finalizers, finalizerName,
		"finalizer should be added on first reconcile")
}

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

func TestReconcile_NoMatchingWorkloadsSetsNoWorkloadsFound(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-policy",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Verify the status was updated with NoWorkloadsFound condition.
	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-policy",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, "NoWorkloadsFound", updated.Status.Conditions[0].Reason)
}

func TestParseFloat64_Valid(t *testing.T) {
	v := parseFloat64("1.5", 1.0)
	assert.InDelta(t, 1.5, v, 0.001)
}

func TestParseFloat64_Empty(t *testing.T) {
	v := parseFloat64("", 1.2)
	assert.InDelta(t, 1.2, v, 0.001)
}

func TestParseFloat64_Invalid(t *testing.T) {
	v := parseFloat64("abc", 1.3)
	assert.InDelta(t, 1.3, v, 0.001)
}

func TestIsRollingOut_DeploymentStable(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	deploy := newTestDeployment("test", "default", nil)
	assert.False(t, reconciler.isRollingOut(deploy))
}

func TestIsRollingOut_DeploymentMidRollout(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	deploy := newTestDeployment("test", "default", nil)
	deploy.Status.UpdatedReplicas = 1 // Only 1 of 2 updated.
	assert.True(t, reconciler.isRollingOut(deploy))
}

func TestBuildPrometheusQuery_FallbackNoContainer(t *testing.T) {
	query := buildPrometheusQuery("default", "api-server-[a-z0-9]+-[a-z0-9]{5}", "", "cpu", 5*time.Minute)
	assert.Contains(t, query, `namespace="default"`)
	assert.Contains(t, query, `pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}"`)
	assert.NotContains(t, query, `container=`)
}

func TestBuildPrometheusQuery_MemoryFallbackNoContainer(t *testing.T) {
	query := buildPrometheusQuery("default", "api-server-[a-z0-9]+-[a-z0-9]{5}", "", "memory", 5*time.Minute)
	assert.Contains(t, query, `namespace="default"`)
	assert.Contains(t, query, `pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}"`)
	assert.NotContains(t, query, `container=`)
}

func TestScaleLimits(t *testing.T) {
	tests := []struct {
		name       string
		currentReq string
		currentLim string
		newReq     string
		wantLim    string
	}{
		{
			name:       "2:1 ratio preserved",
			currentReq: "500m",
			currentLim: "1000m",
			newReq:     "250m",
			wantLim:    "500m",
		},
		{
			name:       "1:1 ratio preserved",
			currentReq: "500m",
			currentLim: "500m",
			newReq:     "300m",
			wantLim:    "300m",
		},
		{
			name:       "zero current req returns zero limit",
			currentReq: "0",
			currentLim: "1000m",
			newReq:     "250m",
			wantLim:    "0",
		},
		{
			name:       "zero current lim returns zero limit",
			currentReq: "500m",
			currentLim: "0",
			newReq:     "250m",
			wantLim:    "0",
		},
		{
			name:       "negative ratio falls back to newReq",
			currentReq: "-500m",
			currentLim: "1000m",
			newReq:     "250m",
			wantLim:    "250m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaleLimits(
				resource.MustParse(tt.currentReq),
				resource.MustParse(tt.currentLim),
				resource.MustParse(tt.newReq),
			)
			want := resource.MustParse(tt.wantLim)
			assert.Equal(t, want.MilliValue(), got.MilliValue())
		})
	}
}

func TestScaleLimits_OverflowClamped(t *testing.T) {
	// 1Ki request with 100Gi limit: ratio = 104857600.
	// New 1Gi request * ratio overflows int64; must preserve existing limit.
	got := scaleLimits(
		resource.MustParse("1Ki"),
		resource.MustParse("100Gi"),
		resource.MustParse("1Gi"),
	)
	assert.True(t, got.Value() > 0, "overflow must not produce negative limit: %v", got)
	want := resource.MustParse("100Gi")
	assert.Equal(t, want.Value(), got.Value(),
		"overflow should preserve existing limit")
}

func TestParseFloat64_NaNFallback(t *testing.T) {
	assert.InDelta(t, 1.2, parseFloat64("NaN", 1.2), 0.001)
}

func TestParseFloat64_InfFallback(t *testing.T) {
	assert.InDelta(t, 1.2, parseFloat64("Inf", 1.2), 0.001)
}

func TestParseFloat64_NegativeFallback(t *testing.T) {
	assert.InDelta(t, 1.2, parseFloat64("-0.5", 1.2), 0.001)
}

func TestParseFloat64_ZeroFallback(t *testing.T) {
	assert.InDelta(t, 1.2, parseFloat64("0", 1.2), 0.001)
}

func TestParseFloat64Ratio_AcceptsHighValues(t *testing.T) {
	// memoryFromCpuRatio values above 10.0 are valid (e.g. 16 GiB per core).
	assert.InDelta(t, 16.0, parseFloat64Ratio("16.0"), 0.001)
	assert.InDelta(t, 32.0, parseFloat64Ratio("32.0"), 0.001)
	assert.InDelta(t, 0.5, parseFloat64Ratio("0.5"), 0.001)
}

func TestParseFloat64Ratio_RejectsBadValues(t *testing.T) {
	assert.InDelta(t, 0, parseFloat64Ratio(""), 0.001)
	assert.InDelta(t, 0, parseFloat64Ratio("-1"), 0.001)
	assert.InDelta(t, 0, parseFloat64Ratio("0"), 0.001)
	assert.InDelta(t, 0, parseFloat64Ratio("1001"), 0.001)
	assert.InDelta(t, 0, parseFloat64Ratio("abc"), 0.001)
}

func TestParseOverheadPercent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		fallback float64
		expected float64
	}{
		{"valid 20", "20", 15.0, 20.0},
		{"valid 0", "0", 15.0, 0.0},
		{"valid 900 boundary", "900", 15.0, 900.0},
		{"valid decimal", "20.5", 15.0, 20.5},
		{"valid scientific", "1e2", 15.0, 100.0},
		{"valid signed positive", "+20", 15.0, 20.0},
		{"empty returns fallback", "", 15.0, 15.0},
		{"non-numeric returns fallback", "abc", 15.0, 15.0},
		{"NaN returns fallback", "NaN", 15.0, 15.0},
		{"Inf returns fallback", "Inf", 15.0, 15.0},
		{"-Inf returns fallback", "-Inf", 15.0, 15.0},
		{"negative returns fallback", "-5", 15.0, 15.0},
		{"over 900 returns fallback", "900.01", 15.0, 15.0},
		{"over 900 large", "1000", 15.0, 15.0},
		{"negative zero", "-0", 15.0, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOverheadPercent(tt.input, tt.fallback)
			assert.InDelta(t, tt.expected, got, 0.001)
		})
	}
}

func TestComputeSavings_ReturnsCorrectStructure(t *testing.T) {
	scheme := testScheme()
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "api",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("1"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"),
					},
				},
			},
		},
	}
	savings, _ := r.computeSavings(recs, nil)
	assert.NotEmpty(t, savings.CPURequestReduction)
	assert.Equal(t, "500m", savings.CPURequestReduction)
}

func TestGetContainers_Deployment(t *testing.T) {
	r := NewAttunePolicyReconciler()
	dep := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "web", Image: "nginx"},
						{Name: "sidecar", Image: "envoy"},
					},
				},
			},
		},
	}
	containers := r.getContainers(dep)
	assert.Len(t, containers, 2)
	assert.Equal(t, "web", containers[0].Name)
	assert.Equal(t, "sidecar", containers[1].Name)
}

func TestGetContainers_StatefulSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	sts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "db", Image: "postgres"},
					},
				},
			},
		},
	}
	containers := r.getContainers(sts)
	assert.Len(t, containers, 1)
	assert.Equal(t, "db", containers[0].Name)
}

func TestGetPodRegex(t *testing.T) {
	r := NewAttunePolicyReconciler()

	tests := []struct {
		name     string
		workload client.Object
		want     string
	}{
		{
			name:     "Deployment uses RS hash + pod hash pattern",
			workload: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api-server"}},
			want:     "api-server-[a-z0-9]+-[a-z0-9]{5}",
		},
		{
			name:     "StatefulSet uses ordinal pattern",
			workload: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "redis"}},
			want:     "redis-[0-9]+",
		},
		{
			name:     "DaemonSet uses pod hash pattern",
			workload: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "node-agent"}},
			want:     "node-agent-[a-z0-9]{5}",
		},
		{
			name:     "Job uses hash suffix pattern",
			workload: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "data-migrate"}},
			want:     "data-migrate-[a-z0-9]{5}",
		},
		{
			name:     "Indexed Job uses index and hash suffix pattern",
			workload: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "data-migrate"}, Spec: batchv1.JobSpec{CompletionMode: ptrCompletionMode(batchv1.IndexedCompletion)}},
			want:     "data-migrate-[0-9]+-[a-z0-9]{5}",
		},
		{
			name:     "CronJob uses timestamp and hash suffix pattern",
			workload: &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly-report"}},
			want:     "nightly-report-[0-9]{10}-[a-z0-9]{5}",
		},
		{
			name:     "Indexed CronJob uses timestamp, index, and hash suffix pattern",
			workload: &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly-report"}, Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{CompletionMode: ptrCompletionMode(batchv1.IndexedCompletion)}}}},
			want:     "nightly-report-[0-9]{10}-[0-9]+-[a-z0-9]{5}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getPodRegex(tt.workload)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseHistoryWindow_Default(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	assert.Equal(t, 7*24*time.Hour, r.parseHistoryWindow(policy))
}

func TestParseHistoryWindow_Custom(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	d := metav1.Duration{Duration: 14 * 24 * time.Hour}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, 14*24*time.Hour, r.parseHistoryWindow(policy))
}

func TestParseHistoryWindow_ClampedTooSmall(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	d := metav1.Duration{Duration: 10 * time.Minute}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, time.Hour, r.parseHistoryWindow(policy), "should clamp to 1h minimum")
}

func TestParseHistoryWindow_ClampedTooLarge(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	d := metav1.Duration{Duration: 1000 * time.Hour}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, 720*time.Hour, r.parseHistoryWindow(policy), "should clamp to 720h maximum")
}

func TestGetMinimumDataPoints_Default(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	assert.Equal(t, int32(48), r.getMinimumDataPoints(policy))
}

func TestGetMinimumDataPoints_Custom(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(42)
	assert.Equal(t, int32(42), r.getMinimumDataPoints(policy))
}

func TestGetQueryStep_Default(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	assert.Equal(t, 5*time.Minute, r.getQueryStep(policy))
}

func TestGetQueryStep_Custom(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 30 * time.Second}
	assert.Equal(t, 30*time.Second, r.getQueryStep(policy))
}

func TestGetQueryStep_ClampedTooSmall(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 1 * time.Second}
	assert.Equal(t, 10*time.Second, r.getQueryStep(policy))
}

func TestGetQueryStep_Zero(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 0}
	assert.Equal(t, 10*time.Second, r.getQueryStep(policy))
}

func TestGetQueryStep_ClampedTooLarge(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 2 * time.Hour}
	assert.Equal(t, 1*time.Hour, r.getQueryStep(policy))
}

func TestIsRollingOut_StatefulSetStable(t *testing.T) {
	r := NewAttunePolicyReconciler()
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{UpdatedReplicas: 3},
	}
	assert.False(t, r.isRollingOut(sts))
}

func TestIsRollingOut_StatefulSetMidRollout(t *testing.T) {
	r := NewAttunePolicyReconciler()
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{UpdatedReplicas: 1},
	}
	assert.True(t, r.isRollingOut(sts))
}

func TestIsRollingOut_DaemonSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 5,
		},
	}
	assert.False(t, r.isRollingOut(ds))
}

func TestIsRollingOut_DaemonSetMidRollout(t *testing.T) {
	r := NewAttunePolicyReconciler()
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 2,
		},
	}
	assert.True(t, r.isRollingOut(ds))
}

func TestParseCooldown_Default(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	assert.Equal(t, 1*time.Hour, r.parseCooldown(policy))
}

func TestParseCooldown_Custom(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	d := metav1.Duration{Duration: 5 * time.Minute}
	policy.Spec.UpdateStrategy.Cooldown = &d
	assert.Equal(t, 5*time.Minute, r.parseCooldown(policy))
}

func TestParseCooldown_SubMinuteClampedTo1m(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	d := metav1.Duration{Duration: 30 * time.Second}
	policy.Spec.UpdateStrategy.Cooldown = &d
	assert.Equal(t, 1*time.Minute, r.parseCooldown(policy))
}

func TestDiscoverWorkloads_FindsStatefulSetByName(t *testing.T) {
	name := "my-sts"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sts", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-sts"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-sts"}},
			},
		},
	}
	r := newReconcilerWithClient(sts)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "StatefulSet",
				Name: &name,
			},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "my-sts", workloads[0].GetName())
}

// generateSamples creates metric samples spread over hourly intervals for testing.
func generateSamples(count int, baseValue float64) []rsmetrics.Sample {
	samples := make([]rsmetrics.Sample, count)
	now := time.Now()
	for i := 0; i < count; i++ {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(count-i) * time.Hour),
			Value:     baseValue + float64(i%10)*0.01,
		}
	}
	return samples
}

// ---------- getOrCreateCollector ----------

func TestGetOrCreateCollector_CacheHit(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	mc := &mockCollector{}
	staleTime := time.Now().Add(-5 * time.Minute)
	reconciler.collectors.Store("http://prom:9090", &collectorEntry{
		collector: mc,
		lastUsed:  staleTime,
	})

	before := time.Now()
	got, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)

	// Verify lastUsed was refreshed on cache hit.
	entry, ok := reconciler.collectors.Load("http://prom:9090")
	require.True(t, ok)
	ce, ok := entry.(*collectorEntry)
	require.True(t, ok, "cached value should be *collectorEntry")
	assert.True(t, ce.lastUsed.After(before) || ce.lastUsed.Equal(before),
		"lastUsed should be refreshed to ~now on cache hit, got %v", ce.lastUsed)
}

func TestGetOrCreateCollector_CacheMiss(t *testing.T) {
	mc := &mockCollector{}
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(address string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		assert.Equal(t, "http://new:9090", address)
		return mc, nil
	}

	got, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)
}

func TestGetOrCreateCollector_FactoryError(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("connection refused")
	}

	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://broken:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestGetOrCreateCollector_CacheFull(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, nil
	}
	// Fill the cache to maxCollectors.
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://prom-%d:9090", i)
		_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: addr}, nil)
		require.NoError(t, err)
	}

	// The next address should be rejected.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://one-too-many:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collector cache full")
}

func TestGetOrCreateCollector_CustomTTL(t *testing.T) {
	customTTL := 2 * time.Minute
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = customTTL
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}

	// Store an entry that is stale under custom TTL but fresh under default TTL.
	staleTime := time.Now().Add(-(customTTL + time.Minute))
	reconciler.collectors.Store("http://stale:9090", &collectorEntry{
		collector: &mockCollector{},
		lastUsed:  staleTime,
	})

	// Creating a new collector should trigger eviction of the stale entry.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)

	_, stillExists := reconciler.collectors.Load("http://stale:9090")
	assert.False(t, stillExists, "entry older than custom TTL should be evicted")
}

func TestGetOrCreateCollector_EvictsStaleEntries(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}
	// Fill the cache to maxCollectors with stale entries.
	staleTime := time.Now().Add(-(collectorTTL + time.Minute))
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://stale-%d:9090", i)
		reconciler.collectors.Store(addr, &collectorEntry{
			collector: &mockCollector{},
			lastUsed:  staleTime,
		})
	}

	// A new address should succeed because stale entries get evicted.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)
}

func TestGetOrCreateCollector_ConcurrentAccess(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = 50 * time.Millisecond
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}

	// Seed some entries that will become stale mid-test.
	staleTime := time.Now().Add(-(collectorTTL + time.Minute))
	for i := 0; i < 10; i++ {
		reconciler.collectors.Store(fmt.Sprintf("http://stale-%d:9090", i),
			&collectorEntry{collector: &mockCollector{}, lastUsed: staleTime})
	}

	const goroutines = 20
	addresses := make([]string, goroutines)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("http://concurrent-%d:9090", i%5) // 5 unique addresses
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: addresses[idx]}, nil)
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "goroutine %d", i)
	}
	// With 5 unique addresses, at most 5 entries should remain in the map
	// (LoadOrStore deduplicates). The factory may be called more often due
	// to races, but the cache itself must be bounded.
	var stored int
	reconciler.collectors.Range(func(_, _ any) bool { stored++; return true })
	assert.LessOrEqual(t, stored, 15, "cache should not grow unbounded")
}

// closableMockCollector wraps mockCollector and implements io.Closer so
// we can verify that evicted collectors have Close() called.
type closableMockCollector struct {
	mockCollector
	closed bool
}

func (c *closableMockCollector) Close() error {
	c.closed = true
	return nil
}

func TestGetOrCreateCollector_EvictionClosesCollector(t *testing.T) {
	closable := &closableMockCollector{}

	now := time.Now()
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = time.Millisecond
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &closableMockCollector{}, nil
	}
	reconciler.SetNowFunc(func() time.Time { return now })

	// Seed the cache with the closable collector at "now".
	reconciler.collectors.Store("http://old:9090", &collectorEntry{
		collector: closable,
		lastUsed:  now,
	})

	// Advance time past the TTL so the entry becomes stale.
	now = now.Add(2 * time.Millisecond)

	// Requesting a different address triggers eviction of stale entries.
	_, err := reconciler.getOrCreateCollector(
		&attunev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil,
	)
	require.NoError(t, err)

	assert.True(t, closable.closed,
		"Close() should be called on evicted collector that implements io.Closer")
}

func TestGetOrCreateCollector_ConcurrentRaceClosesUnused(t *testing.T) {
	var mu sync.Mutex
	var created []*closableMockCollector

	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = collectorTTL
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		c := &closableMockCollector{}
		mu.Lock()
		created = append(created, c)
		mu.Unlock()
		return c, nil
	}

	// All goroutines race to create the same address.
	const goroutines = 10
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.getOrCreateCollector(
				&attunev1alpha1.PrometheusConfig{Address: "http://race:9090"}, nil)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	// Exactly one collector should survive; all others should be closed.
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(created), 1, "at least one collector must be created")

	var openCount int
	for _, c := range created {
		if !c.closed {
			openCount++
		}
	}
	assert.Equal(t, 1, openCount, "exactly one collector should remain open; race losers must be closed")
}

// ---------- computeRecommendations ----------

func TestComputeRecommendations_HappyPath(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu_usage_seconds_total") {
				return generateSamples(200, 0.1), nil // ~100m CPU
			}
			return generateSamples(200, 128*1024*1024), nil // ~128Mi memory
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
	assert.Greater(t, rec.Containers[0].DataPoints, int32(0))
	assert.Greater(t, rec.Containers[0].Confidence, 0.0)
}

func TestComputeRecommendations_InsufficientDataPoints(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil // Only 20 samples, below 48 threshold
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec) // No recommendation because data points are insufficient
}

func TestComputeRecommendations_AllNaNInfSamplesLogsDataQuality(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return samples with all NaN values. BuildProfile filters them out,
	// producing DataPoints == 0 while len(samples) > 0. This exercises
	// the V(1) "All CPU/memory samples were NaN/Inf" log path added in #171.
	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nanSamples, nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "no recommendation when all samples are NaN")
}

func TestComputeRecommendations_CPUAllNaNMemoryValid(t *testing.T) {
	// CPU samples are all NaN, but memory samples are valid.
	// The recommendation should still be produced using memory data,
	// with CPU staying at the current value.
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	reconciler := newReconcilerWithClient()

	nanSamples := make([]rsmetrics.Sample, 50)
	now := time.Now()
	for i := range nanSamples {
		nanSamples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(50-i) * time.Hour),
			Value:     math.NaN(),
		}
	}

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				// Valid memory samples (~256Mi usage).
				return map[string][]rsmetrics.Sample{"main": generateSamples(200, 256*1024*1024)}, nil
			}
			// CPU: all NaN.
			return map[string][]rsmetrics.Sample{"main": nanSamples}, nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec, "recommendation should be produced from memory data even when CPU is all NaN")
	require.Len(t, rec.Containers, 1)
	// CPU should stay at current because CPU had no valid data points.
	assert.Equal(t, resource.MustParse("100m"), rec.Containers[0].Recommended.CPURequest,
		"CPU should stay at current when CPU samples are all NaN")
	// Memory should differ from current (recommendations engine processes valid memory data).
	assert.NotEqual(t, resource.MustParse("128Mi"), rec.Containers[0].Recommended.MemoryRequest,
		"Memory recommendation should change when memory samples are valid")
}

func TestComputeRecommendations_QueryError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	rec, qErrors, failedMetricTypes, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.Greater(t, qErrors, 0, "query failures should be counted")
	assert.ElementsMatch(t, []string{"CPU", "memory"}, failedMetricTypes)
}

func TestComputeRecommendations_PartialQueryErrorTracksFailedMetricType(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return nil, fmt.Errorf("memory query failed")
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}

	rec, qErrors, failedMetricTypes, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 1, qErrors)
	assert.Equal(t, []string{"memory"}, failedMetricTypes)
}

func TestComputeRecommendations_ContextCancelledDuringParallelQueries(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	ctx, cancel := context.WithCancel(context.Background())
	var queryCalls atomic.Int32
	mc := &mockCollector{
		queryRangeGroupedFunc: func(qctx context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			queryCalls.Add(1)
			// Simulate slow query: wait for context cancellation.
			cancel() // Cancel as soon as first query starts.
			<-qctx.Done()
			return nil, qctx.Err()
		},
	}

	rec, qErrors, _, _, err := reconciler.computeRecommendations(ctx, policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.Equal(t, 2, qErrors, "both queries should report failure when context is cancelled")
}

func TestComputeRecommendations_EmptyContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	emptyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{}},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, emptyDeploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec)
}

func TestComputeRecommendations_AllowDecreaseBlocked(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// AllowDecrease is nil (default) — memory decreases should be clamped.

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return very low memory usage (0.001 cores CPU, ~1MiB memory)
	// to produce recommendations lower than current (512Mi).
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.001), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// Memory should be clamped to current (512Mi) since AllowDecrease is nil.
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Cmp(resource.MustParse("512Mi")) >= 0,
		"memory should not decrease below current when AllowDecrease is nil, got %s", rec.Containers[0].Recommended.MemoryRequest.String())
}

func TestComputeRecommendations_CPUAllowDecreaseNilAllowsDecrease(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// CPU AllowDecrease is nil (default) — CPU decreases should be allowed.
	require.Nil(t, policy.Spec.CPU.AllowDecrease)
	// Use 4000m current request with 200m actual usage (0.2 cores). With 500
	// data points (good confidence), the recommendation should be well under 4000m.
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("4000m")
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(500, 0.2), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// CPU should decrease below current (4000m) when AllowDecrease is nil (defaults to true for CPU).
	cpuRec := rec.Containers[0].Recommended.CPURequest
	assert.True(t, cpuRec.Cmp(resource.MustParse("4000m")) < 0,
		"CPU should decrease below current when AllowDecrease is nil, got %s", cpuRec.String())
}

func TestEnforceAllowDecrease(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")

	tests := []struct {
		name           string
		allowDecrease  bool
		rec            string
		current        string
		wantClamped    bool
		wantAdjustment string
	}{
		{
			name:          "decrease allowed, rec < current",
			allowDecrease: true,
			rec:           "100m",
			current:       "500m",
			wantClamped:   false,
		},
		{
			name:           "decrease blocked, rec < current",
			allowDecrease:  false,
			rec:            "100m",
			current:        "500m",
			wantClamped:    true,
			wantAdjustment: "CPU decrease from 500m to 100m blocked by allowDecrease=false",
		},
		{
			name:          "decrease blocked, rec > current (increase always allowed)",
			allowDecrease: false,
			rec:           "1000m",
			current:       "500m",
			wantClamped:   false,
		},
		{
			name:          "decrease blocked, rec == current (no change)",
			allowDecrease: false,
			rec:           "500m",
			current:       "500m",
			wantClamped:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newReconcilerWithClient()
			rec := resource.MustParse(tt.rec)
			current := resource.MustParse(tt.current)
			explain := recommendation.RecommendationExplanation{}

			result := reconciler.enforceAllowDecrease(tt.allowDecrease, rec, current, &explain, policy, "test-container", "CPU")

			if tt.wantClamped {
				assert.Equal(t, current.String(), result.String(), "should be clamped to current")
				assert.Equal(t, tt.wantAdjustment, explain.FinalAdjustment)
				assert.Equal(t, current.String(), explain.Final.String())
			} else {
				assert.Equal(t, rec.String(), result.String(), "should not be clamped")
				assert.Empty(t, explain.FinalAdjustment)
			}
		})
	}
}

func TestScaleControlledLimits(t *testing.T) {
	tests := []struct {
		name          string
		cpuControlled *string
		memControlled *string
		wantCPULim    string
		wantMemLim    string
	}{
		{
			name:       "nil (default RequestsOnly) keeps original limits",
			wantCPULim: "1",
			wantMemLim: "1Gi",
		},
		{
			name:          "RequestsOnly keeps original limits",
			cpuControlled: stringPtr("RequestsOnly"),
			memControlled: stringPtr("RequestsOnly"),
			wantCPULim:    "1",
			wantMemLim:    "1Gi",
		},
		{
			name:          "RequestsAndLimits scales limits proportionally",
			cpuControlled: stringPtr("RequestsAndLimits"),
			memControlled: stringPtr("RequestsAndLimits"),
			wantCPULim:    "2",      // 500m->1000m req means 1000m->2000m lim (2:1 ratio)
			wantMemLim:    "1536Mi", // 512Mi->768Mi req means 1Gi->1536Mi lim (2:1 ratio)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			policy.Spec.CPU.ControlledValues = tt.cpuControlled
			policy.Spec.Memory.ControlledValues = tt.memControlled

			rec := attunev1alpha1.ContainerRecommendation{
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("1000m"),
					CPULimit:      resource.MustParse("1000m"),
					MemoryRequest: resource.MustParse("768Mi"),
					MemoryLimit:   resource.MustParse("1Gi"),
				},
			}

			scaleControlledLimits(policy, &rec,
				resource.MustParse("500m"),  // currentCPUReq
				resource.MustParse("1000m"), // currentCPULim
				resource.MustParse("512Mi"), // currentMemReq
				resource.MustParse("1Gi"),   // currentMemLim
			)

			assert.Equal(t, tt.wantCPULim, rec.Recommended.CPULimit.String())
			assert.Equal(t, tt.wantMemLim, rec.Recommended.MemoryLimit.String())
		})
	}
}

func TestSetRecommendationGauges(t *testing.T) {
	rec := &attunev1alpha1.ContainerRecommendation{
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Confidence: 0.85,
	}

	setRecommendationGauges("test-ns", "my-deploy", "main", rec)

	cpuGauge := promtestutil.ToFloat64(operatormetrics.RecommendationCPU.WithLabelValues("test-ns", "my-deploy", "main"))
	memGauge := promtestutil.ToFloat64(operatormetrics.RecommendationMemory.WithLabelValues("test-ns", "my-deploy", "main"))
	confGauge := promtestutil.ToFloat64(operatormetrics.Confidence.WithLabelValues("test-ns", "my-deploy", "main"))

	assert.InDelta(t, 0.5, cpuGauge, 1e-9)
	assert.Equal(t, float64(256*1024*1024), memGauge)
	assert.InDelta(t, 0.85, confGauge, 1e-9)
}

func TestComputeRecommendations_CPUAllowDecreaseBlocked(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// Explicitly disable CPU decreases. nil defaults to true for CPU.
	policy.Spec.CPU.AllowDecrease = boolPtr(false)

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return very low CPU usage to produce a recommendation lower than current (500m).
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.001), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// CPU should be clamped to current (500m) since AllowDecrease is explicitly false.
	assert.True(t, rec.Containers[0].Recommended.CPURequest.Cmp(resource.MustParse("500m")) >= 0,
		"CPU should not decrease below current when AllowDecrease is false, got %s", rec.Containers[0].Recommended.CPURequest.String())
}

func TestComputeRecommendations_RequestsOnly(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// ControlledValues defaults to RequestsOnly when nil.
	// Also verify the explicit "RequestsOnly" value behaves identically.
	for _, cv := range []struct {
		name string
		val  *string
	}{
		{"nil (default)", nil},
		{"explicit RequestsOnly", stringPtr("RequestsOnly")},
	} {
		t.Run(cv.name, func(t *testing.T) {
			policy.Spec.CPU.ControlledValues = cv.val
			policy.Spec.Memory.ControlledValues = cv.val

			deploy := newTestDeployment("api-server", "default", nil)
			reconciler := newReconcilerWithClient()

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}

			rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
			require.NoError(t, err)
			require.NotNil(t, rec)
			require.Len(t, rec.Containers, 1)

			c := rec.Containers[0]
			// Requests should be adjusted by the recommendation engine.
			assert.False(t, c.Recommended.CPURequest.IsZero(), "CPURequest should be set")
			assert.False(t, c.Recommended.MemoryRequest.IsZero(), "MemoryRequest should be set")

			// With RequestsOnly, limits should stay at the CURRENT values (not scaled).
			// The deployment has limits: CPU=1000m, Memory=1Gi.
			assert.True(t, c.Recommended.CPULimit.Equal(resource.MustParse("1000m")),
				"CPULimit should be unchanged at 1000m, got %s", c.Recommended.CPULimit.String())
			assert.True(t, c.Recommended.MemoryLimit.Equal(resource.MustParse("1Gi")),
				"MemoryLimit should be unchanged at 1Gi, got %s", c.Recommended.MemoryLimit.String())

			// Verify requests actually changed from the original 500m/512Mi.
			original := resource.MustParse("500m")
			assert.NotEqual(t, original.MilliValue(), c.Recommended.CPURequest.MilliValue(),
				"CPURequest should differ from the original 500m")
		})
	}
}

func TestComputeRecommendations_RequestsAndLimits(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ral := "RequestsAndLimits"
	policy.Spec.CPU.ControlledValues = &ral
	policy.Spec.Memory.ControlledValues = &ral

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	c := rec.Containers[0]
	// With RequestsAndLimits, limits must be scaled proportionally.
	assert.False(t, c.Recommended.CPULimit.IsZero(), "CPULimit should be set when ControlledValues=RequestsAndLimits")
	assert.False(t, c.Recommended.MemoryLimit.IsZero(), "MemoryLimit should be set when ControlledValues=RequestsAndLimits")

	// The deployment has 2:1 ratio (limits=1000m, requests=500m for CPU; limits=1Gi, requests=512Mi for memory).
	// Limits should be proportionally scaled from the new request.
	cpuRatio := float64(c.Recommended.CPULimit.MilliValue()) / float64(c.Recommended.CPURequest.MilliValue())
	assert.InDelta(t, 2.0, cpuRatio, 0.01, "CPU limit/request ratio should preserve the original 2:1 ratio")

	memRatio := float64(c.Recommended.MemoryLimit.Value()) / float64(c.Recommended.MemoryRequest.Value())
	assert.InDelta(t, 2.0, memRatio, 0.01, "Memory limit/request ratio should preserve the original ~2:1 ratio")
}

func TestComputeRecommendations_BatchesQueriesPerWorkload(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	deploy.Spec.Template.Spec.Containers = append(deploy.Spec.Template.Spec.Containers, corev1.Container{
		Name:  "sidecar",
		Image: "busybox",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	reconciler := newReconcilerWithClient()

	var mu sync.Mutex
	calls := make(map[string]int)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			mu.Lock()
			calls[query]++
			mu.Unlock()
			return map[string][]rsmetrics.Sample{
				"main":    generateSamples(200, 0.1),
				"sidecar": generateSamples(200, 0.05),
			}, nil
		},
	}

	rec, qErrors, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, qErrors)
	require.Len(t, rec.Containers, 2)
	assert.Len(t, calls, 2, "expected one CPU query and one memory query per workload")
	for _, count := range calls {
		assert.Equal(t, 1, count)
	}
}

func TestComputeRecommendations_UsesPodLevelSeriesWithoutExtraQuery(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu_usage_seconds_total") {
				return map[string][]rsmetrics.Sample{"": generateSamples(200, 0.1)}, nil
			}
			return map[string][]rsmetrics.Sample{"": generateSamples(200, 128*1024*1024)}, nil
		},
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("unexpected extra fallback query: %s", query)
		},
	}

	rec, qErrors, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, qErrors)
	require.Len(t, rec.Containers, 1)
}

func TestComputeRecommendations_PopulatesExplanation(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{
				"main": generateSamples(200, 0.1),
			}, nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	require.NotNil(t, rec.Containers[0].Explanation)
	require.NotNil(t, rec.Containers[0].Explanation.CPU)
	require.NotNil(t, rec.Containers[0].Explanation.Memory)
	assert.False(t, rec.Containers[0].Explanation.CPU.RawPercentile.IsZero())
	assert.False(t, rec.Containers[0].Explanation.CPU.Final.IsZero())
	assert.False(t, rec.Containers[0].Explanation.Memory.RawPercentile.IsZero())
	assert.False(t, rec.Containers[0].Explanation.Memory.Final.IsZero())
}

// ---------- resolvePrometheusConfig ----------

func TestResolvePrometheusConfig_PolicyHasAddress(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler := newReconcilerWithClient()

	config, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus:9090", config.Address)
}

func TestResolvePrometheusConfig_FallsBackToDefaults(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}

	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://defaults-prometheus:9090",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(defaults)

	config, err := reconciler.resolvePrometheusConfig(context.Background(), policy, defaults)
	assert.NoError(t, err)
	assert.Equal(t, "http://defaults-prometheus:9090", config.Address)
}

func TestResolvePrometheusConfig_NoAddressAnywhere(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	reconciler := newReconcilerWithClient()

	_, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no Prometheus address configured")
}

func TestResolvePrometheusConfig_RejectsBlockedPolicyAddress(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.Address = "http://127.0.0.1:9090"
	reconciler := newReconcilerWithClient()

	_, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
}

func TestResolvePrometheusConfig_RejectsBlockedDefaultsAddress(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://127.0.0.1:9090",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(defaults)

	_, err := reconciler.resolvePrometheusConfig(context.Background(), policy, defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
}

// ---------- resolveDatadogCollector ----------

func TestResolveDatadogCollector_HappyPath(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key-12345"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.eu",
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, qb, err := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be non-nil")
	assert.IsType(t, &rsmetrics.DatadogQueryBuilder{}, qb, "should return DatadogQueryBuilder")
}

func TestResolveDatadogCollector_DefaultSite(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "", // empty = default
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, _, err := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be created with default site")
}

func TestResolveDatadogCollector_WithAppKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
			"app-key": []byte("test-app-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "us5.datadoghq.com",
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, _, err := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should succeed when app-key is present")
}

func TestResolveDatadogCollector_MissingSecret(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "nonexistent-secret", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient() // no secret

	_, _, err := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Datadog API key")
}

func TestResolveDatadogCollector_MissingKeyInSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"wrong-key": []byte("value"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	_, _, err := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Datadog API key")
}

func TestResolveDatadogCollector_CachesCollector(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.com",
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	c1, _, err1 := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.NoError(t, err1)
	c2, _, err2 := reconciler.resolveDatadogCollector(context.Background(), policy)
	require.NoError(t, err2)
	assert.Same(t, c1, c2, "second call should return cached collector")
}

// ---------- resolveCloudWatchCollector ----------

func TestResolveCloudWatchCollector_CreatesCollector(t *testing.T) {
	// NewCloudWatchCollector succeeds at construction time (the AWS SDK
	// loads config from env/files without making API calls). Verify the
	// collector and query builder are created correctly.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "test-cluster",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	collector, qb, err := reconciler.resolveCloudWatchCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be created")
	cwQB, ok := qb.(*rsmetrics.CloudWatchQueryBuilder)
	require.True(t, ok, "should return CloudWatchQueryBuilder")
	assert.Equal(t, "test-cluster", cwQB.ClusterName)
}

func TestResolveCloudWatchCollector_QueryBuilder(t *testing.T) {
	// Even though the collector creation fails, we can verify the function's
	// structure by testing a cached path. Pre-seed the cache with a mock.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "eu-west-1",
					ClusterName: "prod-cluster",
					RoleARN:     "arn:aws:iam::123456789012:role/test",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	// Pre-seed the collector cache so the factory is not called (avoids AWS SDK).
	cacheKey := fmt.Sprintf("cloudwatch:%s|%s|%s", "eu-west-1", "prod-cluster", "arn:aws:iam::123456789012:role/test")
	mc := &mockCollector{}
	reconciler.collectors.Store(cacheKey, &collectorEntry{collector: mc, lastUsed: time.Now()})

	collector, qb, err := reconciler.resolveCloudWatchCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.Same(t, mc, collector, "should return pre-seeded cached collector")
	cwQB, ok := qb.(*rsmetrics.CloudWatchQueryBuilder)
	require.True(t, ok, "should return CloudWatchQueryBuilder")
	assert.Equal(t, "prod-cluster", cwQB.ClusterName, "ClusterName should match policy")
}

func TestResolveCloudWatchCollector_CacheKeyIncludesRoleARN(t *testing.T) {
	reconciler := newReconcilerWithClient()

	// Pre-seed two entries with different role ARNs.
	mc1 := &mockCollector{}
	mc2 := &mockCollector{}
	reconciler.collectors.Store("cloudwatch:us-east-1|cluster|", &collectorEntry{collector: mc1, lastUsed: time.Now()})
	reconciler.collectors.Store("cloudwatch:us-east-1|cluster|arn:aws:iam::111:role/x", &collectorEntry{collector: mc2, lastUsed: time.Now()})

	policy1 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "cluster",
				},
			},
		},
	}
	policy2 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "cluster",
					RoleARN:     "arn:aws:iam::111:role/x",
				},
			},
		},
	}

	c1, _, err1 := reconciler.resolveCloudWatchCollector(context.Background(), policy1)
	require.NoError(t, err1)
	c2, _, err2 := reconciler.resolveCloudWatchCollector(context.Background(), policy2)
	require.NoError(t, err2)
	assert.Same(t, mc1, c1, "no-role policy should return pre-seeded mc1")
	assert.Same(t, mc2, c2, "role-arn policy should return pre-seeded mc2")
	assert.NotSame(t, c1, c2, "different role ARNs should resolve to different collectors")
}

// ---------- collectorCacheKey ----------

func TestCollectorCacheKey_AddressOnly(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	assert.Equal(t, "http://prom:9090", collectorCacheKey(config, nil))
}

func TestCollectorCacheKey_WithOptions(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		BearerToken:        "tok",
		InsecureSkipVerify: true,
		Headers:            map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	key := collectorCacheKey(config, opts)
	assert.Contains(t, key, "http://prom:9090")
	assert.Contains(t, key, "|bearer:")
	assert.Contains(t, key, "|insecure")
	assert.Contains(t, key, "|h:X-Scope-OrgID=")
	assert.NotContains(t, key, "tenant-1") // header value should be hashed
	assert.NotContains(t, key, "tok")      // bearer token should be hashed
}

func TestCollectorCacheKey_DeterministicWithMultipleHeaders(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		Headers: map[string]string{"Z-Header": "z", "A-Header": "a", "M-Header": "m"},
	}
	// Call multiple times to verify map iteration order doesn't affect the key.
	key1 := collectorCacheKey(config, opts)
	for i := 0; i < 100; i++ {
		assert.Equal(t, key1, collectorCacheKey(config, opts), "cache key must be deterministic on iteration %d", i)
	}
	// Verify sorted order: A before M before Z (values are now SHA-256 hashed).
	assert.Contains(t, key1, "|h:A-Header=")
	assert.Contains(t, key1, "|h:M-Header=")
	assert.Contains(t, key1, "|h:Z-Header=")
	// Hashed values should NOT contain the raw header value.
	assert.NotContains(t, key1, "=a|")
	assert.NotContains(t, key1, "=z")
}

func TestCollectorCacheKey_DifferentConfigsDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, nil)
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok"})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_DifferentBearerTokensDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-a"})
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-b"})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_WithQueryParameters(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "30s", "timeout": "10s"},
	}
	key := collectorCacheKey(config, opts)
	assert.Contains(t, key, "|qp:step=30s")
	assert.Contains(t, key, "|qp:timeout=10s")
}

func TestCollectorCacheKey_DifferentQueryParametersDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "30s"},
	})
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "60s"},
	})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_QueryParametersDeterministic(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"z-param": "z", "a-param": "a", "m-param": "m"},
	}
	key1 := collectorCacheKey(config, opts)
	for i := 0; i < 100; i++ {
		assert.Equal(t, key1, collectorCacheKey(config, opts), "cache key must be deterministic on iteration %d", i)
	}
}

// ---------- buildCollectorOptions ----------

func TestBuildCollectorOptions_NilWhenNoAuthOrTLS(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	assert.Nil(t, opts)
}

func TestBuildCollectorOptions_WithHeaders(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		Headers: map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "tenant-1", opts.Headers["X-Scope-OrgID"])
}

func TestBuildCollectorOptions_WithQueryParameters(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address:         "http://prom:9090",
		QueryParameters: map[string]string{"dedup": "true"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "true", opts.QueryParameters["dedup"])
}

func TestBuildCollectorOptions_RejectsReservedQueryParameters(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address:         "http://prom:9090",
		QueryParameters: map[string]string{"query": "up"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "reserved")
}

func TestBuildCollectorOptions_WithTLS(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address: "https://prom:9090",
		TLS:     &attunev1alpha1.TLSConfig{InsecureSkipVerify: true},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.True(t, opts.InsecureSkipVerify)
}

func TestBuildCollectorOptions_WithBearerToken(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-bearer")},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "prom-token",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "test-bearer", opts.BearerToken)
}

func TestBuildCollectorOptions_SecretNotFound(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "missing-secret",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "missing-secret")
}

// ---------- readSecretKey ----------

func TestReadSecretKey_Success(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("my-bearer-token")},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	token, err := r.readSecretKey(context.Background(), "default", "prom-token", "token")
	assert.NoError(t, err)
	assert.Equal(t, "my-bearer-token", token)
}

func TestReadSecretKey_SecretNotFound(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	_, err := r.readSecretKey(context.Background(), "default", "missing-secret", "token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading secret default/missing-secret")
}

func TestReadSecretKey_KeyNotFound(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"wrong-key": []byte("value")},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	_, err := r.readSecretKey(context.Background(), "default", "prom-token", "token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "key \"token\" not found")
}

func TestReconcile_BearerTokenSecretRotationRecreatesCollector(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &attunev1alpha1.SecretKeyRef{
		Name: "prom-token",
		Key:  "token",
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("token-a")},
	}

	collector1 := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	collector2 := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	collectors := []rsmetrics.MetricsCollector{collector1, collector2}
	var optsSeen []*rsmetrics.CollectorOptions

	reconciler, fakeClient := newReconcilerForReconcile(collector1, policy, deploy, secret)
	reconciler.MetricsFactory = func(_ string, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		require.NotNil(t, opts)
		copyOpts := &rsmetrics.CollectorOptions{
			BearerToken:        opts.BearerToken,
			InsecureSkipVerify: opts.InsecureSkipVerify,
		}
		if opts.Headers != nil {
			copyOpts.Headers = make(map[string]string, len(opts.Headers))
			for k, v := range opts.Headers {
				copyOpts.Headers[k] = v
			}
		}
		optsSeen = append(optsSeen, copyOpts)
		idx := len(optsSeen) - 1
		return collectors[idx], nil
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, optsSeen, 1)
	assert.Equal(t, "token-a", optsSeen[0].BearerToken)

	var rotated corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "prom-token", Namespace: "default"}, &rotated))
	rotated.Data["token"] = []byte("token-b")
	require.NoError(t, fakeClient.Update(context.Background(), &rotated))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, optsSeen, 2)
	assert.Equal(t, "token-b", optsSeen[1].BearerToken)
	assert.NotSame(t, collector1, collector2)
}

// ---------- updateStatusWithRetry ----------

func TestUpdateStatusWithRetry_SuccessFirstAttempt(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	p.Status.Workloads = attunev1alpha1.WorkloadStatus{Discovered: 5}

	err := reconciler.updateStatusWithRetry(ctx, &p, key)
	assert.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &updated))
	assert.Equal(t, int32(5), updated.Status.Workloads.Discovered)
}

func TestUpdateStatusWithRetry_ConflictThenRetry(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	// Set status we want to persist.
	p.Status.Workloads = attunev1alpha1.WorkloadStatus{Discovered: 7}

	// Create a concurrent metadata update to bump the resource version.
	var concurrent attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &concurrent))
	if concurrent.Annotations == nil {
		concurrent.Annotations = make(map[string]string)
	}
	concurrent.Annotations["test-bump"] = "true"
	require.NoError(t, fakeClient.Update(ctx, &concurrent))

	// p now has a stale resource version. The function should handle the
	// conflict, re-fetch the object, and retry successfully.
	err := reconciler.updateStatusWithRetry(ctx, &p, key)
	assert.NoError(t, err)

	var final attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &final))
	assert.Equal(t, int32(7), final.Status.Workloads.Discovered)
	// The concurrent annotation should be present (proves re-fetch picked up latest).
	assert.Equal(t, "true", final.Annotations["test-bump"])
}

func TestUpdateStatusWithRetry_PreservesHigherResizedCount(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	// This reconcile has Resized=0 (stale snapshot).
	p.Status.Workloads = attunev1alpha1.WorkloadStatus{Discovered: 5, Resized: 0}

	// Simulate a concurrent reconcile that set Resized=2.
	var concurrent attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &concurrent))
	concurrent.Status.Workloads = attunev1alpha1.WorkloadStatus{Discovered: 5, Resized: 2}
	require.NoError(t, fakeClient.Status().Update(ctx, &concurrent))

	// p now has a stale resource version AND a lower Resized count.
	err := reconciler.updateStatusWithRetry(ctx, &p, key)
	assert.NoError(t, err)

	var final attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &final))
	assert.Equal(t, int32(2), final.Status.Workloads.Resized,
		"should preserve the higher Resized count from the concurrent reconcile")
}

// ---------- markResizeTime ----------

func TestMarkResizeTime_NoExistingAnnotations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler := newReconcilerWithClient(policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, reconciler.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, reconciler.Get(ctx, key, &updated))

	resizeTime, ok := updated.Annotations[lastResizeAnnotation]
	assert.True(t, ok, "last-resize-time annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, resizeTime)
	assert.NoError(t, parseErr, "annotation value should be valid RFC3339")
}

func TestMarkResizeTime_ExistingAnnotations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Annotations = map[string]string{"existing-key": "existing-value"}
	reconciler := newReconcilerWithClient(policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, reconciler.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, reconciler.Get(ctx, key, &updated))

	assert.Equal(t, "existing-value", updated.Annotations["existing-key"])
	resizeTime, ok := updated.Annotations[lastResizeAnnotation]
	assert.True(t, ok, "last-resize-time annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, resizeTime)
	assert.NoError(t, parseErr, "annotation value should be valid RFC3339")
}

// ---------- Reconcile happy path ----------

func TestReconcile_HappyPathWithRecommendations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod1, pod2)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	// Verify status was updated with recommendations and Ready=True.
	var updated attunev1alpha1.AttunePolicy
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	assert.Equal(t, int32(1), updated.Status.Workloads.Pending, "Recommend mode: all workloads with recs should be pending")
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)

	// Verify Ready condition.
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "Monitoring", updated.Status.Conditions[0].Reason)
}

func TestReconcile_ObserveModeOmitsRecommendations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

	// Observe mode: workloads are discovered and data points tracked,
	// but recommendations and savings are not surfaced.
	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Empty(t, updated.Status.Recommendations, "Observe mode should not populate recommendations")
	assert.Empty(t, updated.Status.Savings.EstimatedMonthlySavings, "Observe mode should not compute savings")
	assert.True(t, updated.Status.Workloads.DataPointsCollected > 0, "Observe mode should still track data points")
}

func TestReconcile_RecommendModeKeepsRecommendationsWithoutLivePods(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	assert.Equal(t, int32(1), updated.Status.Workloads.Pending)
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)
}

// ---------- observation-period requeue ----------

func TestRequeueShortenedByObservationPeriod(t *testing.T) {
	// Test that getObservationPeriod returns the canary config value.
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		ObservationPeriod: metav1.Duration{Duration: 2 * time.Minute},
	}
	assert.Equal(t, 2*time.Minute, getObservationPeriod(policy))

	// Test that default observation period is used when no canary config.
	policyNoCanary := newTestPolicy("test-policy2", "default")
	assert.Equal(t, defaultObservationPeriod, getObservationPeriod(policyNoCanary))

	// Test that safetyObservationPeriod takes precedence over canary.
	policySOP := newTestPolicy("test-policy3", "default")
	policySOP.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 3 * time.Minute}
	policySOP.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		ObservationPeriod: metav1.Duration{Duration: 2 * time.Minute},
	}
	assert.Equal(t, 3*time.Minute, getObservationPeriod(policySOP),
		"safetyObservationPeriod should take precedence over canary.observationPeriod")

	// Test that safetyObservationPeriod works without canary config.
	policySOP2 := newTestPolicy("test-policy4", "default")
	policySOP2.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 90 * time.Second}
	assert.Equal(t, 90*time.Second, getObservationPeriod(policySOP2))

	// Test the min(cooldown, observationPeriod) requeue logic directly.
	// When AutoRevert is true and resizes occurred, the reconciler
	// uses min(cooldown, observationPeriod) as requeue interval
	// (lines 417-424 of attunepolicy_controller.go).
	cooldown := 1 * time.Hour
	obs := getObservationPeriod(policy) // 2m
	requeueAfter := cooldown
	if obs < requeueAfter {
		requeueAfter = obs
	}
	assert.Equal(t, 2*time.Minute, requeueAfter,
		"requeue should be shortened to observation period when it is less than cooldown")

	// When observation period exceeds cooldown, cooldown wins.
	longObs := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 5 * time.Minute},
				Canary: &attunev1alpha1.CanaryConfig{
					ObservationPeriod: metav1.Duration{Duration: 10 * time.Minute},
				},
			},
		},
	}
	obsLong := getObservationPeriod(longObs)
	cooldownShort := longObs.Spec.UpdateStrategy.Cooldown.Duration
	requeueAfter2 := cooldownShort
	if obsLong < requeueAfter2 {
		requeueAfter2 = obsLong
	}
	assert.Equal(t, 5*time.Minute, requeueAfter2,
		"cooldown should win when observation period is longer")
}

// ---------- appendResizedContainer ----------

func TestAppendResizedContainer(t *testing.T) {
	tests := []struct {
		name      string
		existing  string
		container string
		want      string
	}{
		{"first container on empty", "", "main", "main"},
		{"append second container", "main", "sidecar", "main,sidecar"},
		{"dedup existing container", "main", "main", "main"},
		{"dedup in multi-container list", "main,sidecar", "main", "main,sidecar"},
		{"append third container", "main,sidecar", "worker", "main,sidecar,worker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			}}
			if tt.existing != "" {
				pod.Annotations[annotationResizedContainers] = tt.existing
			}
			appendResizedContainer(pod, tt.container)
			assert.Equal(t, tt.want, pod.Annotations[annotationResizedContainers])
		})
	}
}

// ---------- parseResizeRecords ----------

func TestParseResizeRecords_MultiContainer(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                              resizedAt,
				annotationResizedContainers:                      "app,sidecar",
				annotationOriginalCPUPrefix + "app":              "500m",
				annotationOriginalMemoryPrefix + "app":           "512Mi",
				annotationOriginalRestartCountPrefix + "app":     "0",
				annotationOriginalCPUPrefix + "sidecar":          "100m",
				annotationOriginalMemoryPrefix + "sidecar":       "128Mi",
				annotationOriginalRestartCountPrefix + "sidecar": "2",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
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
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "app", records[0].Container)
	assert.Equal(t, "sidecar", records[1].Container)
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("500m")))
	assert.True(t, records[1].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.Equal(t, int32(2), records[1].RestartCount)
}

func TestParseResizeRecords_RestoresLimits(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "limits-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "100m",
				annotationOriginalMemoryPrefix + "app":       "64Mi",
				annotationOriginalCPULimitPrefix + "app":     "200m",
				annotationOriginalMemoryLimitPrefix + "app":  "128Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("150m"),
					corev1.ResourceMemory: resource.MustParse("96Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("192Mi"),
				},
			}},
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 1)
	// Requests restored from annotations.
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.True(t, records[0].OriginalResources.Requests.Memory().Equal(resource.MustParse("64Mi")))
	// Limits restored from limit annotations.
	require.NotNil(t, records[0].OriginalResources.Limits, "Limits should be populated from limit annotations")
	assert.True(t, records[0].OriginalResources.Limits.Cpu().Equal(resource.MustParse("200m")))
	assert.True(t, records[0].OriginalResources.Limits.Memory().Equal(resource.MustParse("128Mi")))
}

func TestParseResizeRecords_NoLimitAnnotations(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "no-limits-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "100m",
				annotationOriginalMemoryPrefix + "app":       "64Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("150m"),
					corev1.ResourceMemory: resource.MustParse("96Mi"),
				},
			}},
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	// Limits should be nil when no limit annotations exist (backwards compat).
	assert.Nil(t, records[0].OriginalResources.Limits, "Limits should be nil when limit annotations are absent")
}

func TestParseResizeRecords_MissingCPUAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                    resizedAt,
				annotationResizedContainers:            "app",
				annotationOriginalMemoryPrefix + "app": "512Mi",
			},
		},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing original CPU for app")
}

func TestParseResizeRecords_InvalidTimestamp(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt: "not-a-timestamp",
			},
		},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing resized-at annotation")
}

func TestParseResizeRecords_MalformedRestartCount(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "500m",
				annotationOriginalMemoryPrefix + "app":       "512Mi",
				annotationOriginalRestartCountPrefix + "app": "not-a-number",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
		}},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing original restart count for app")
}

func TestParseResizeRecords_MalformedLimitAnnotations(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	testCases := []struct {
		name          string
		podName       string
		annotations   map[string]string
		errorContains string
	}{
		{
			name:    "cpu limit",
			podName: "bad-cpu-limit-pod",
			annotations: map[string]string{
				annotationOriginalCPULimitPrefix + "app": "not-a-quantity",
			},
			errorContains: "parsing original CPU limit for app",
		},
		{
			name:    "memory limit",
			podName: "bad-memory-limit-pod",
			annotations: map[string]string{
				annotationOriginalMemoryLimitPrefix + "app": "not-a-quantity",
			},
			errorContains: "parsing original memory limit for app",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "500m",
				annotationOriginalMemoryPrefix + "app":       "512Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			}
			for key, value := range tc.annotations {
				annotations[key] = value
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        tc.podName,
					Namespace:   "default",
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			}

			_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errorContains)
		})
	}
}

func TestRemoveTrackingAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":               "test",
				"attune.io/tracked": "true",
				"unrelated":         "keep",
			},
			Annotations: map[string]string{
				"attune.io/resized-at":                      "2026-01-01T00:00:00Z",
				"attune.io/resized-workload":                "api-server",
				"attune.io/resized-containers":              "main,sidecar",
				"attune.io/original-cpu-request.main":       "500m",
				"attune.io/original-memory-request.main":    "512Mi",
				"attune.io/original-cpu-limit.main":         "1000m",
				"attune.io/original-memory-limit.main":      "1Gi",
				"attune.io/original-restart-count.main":     "0",
				"attune.io/original-cpu-request.sidecar":    "100m",
				"attune.io/original-memory-request.sidecar": "64Mi",
				"attune.io/original-cpu-limit.sidecar":      "200m",
				"attune.io/original-memory-limit.sidecar":   "128Mi",
				"attune.io/original-restart-count.sidecar":  "2",
				"unrelated-annotation":                      "keep",
			},
		},
	}

	removeTrackingAnnotations(pod)

	// Tracking label should be removed.
	_, hasTracked := pod.Labels["attune.io/tracked"]
	assert.False(t, hasTracked, "tracked label should be removed")
	assert.Equal(t, "keep", pod.Labels["unrelated"], "unrelated labels should be preserved")

	// All tracking annotations should be removed.
	for key := range pod.Annotations {
		assert.False(t, strings.HasPrefix(key, "attune.io/"),
			"tracking annotation %q should be removed", key)
	}
	assert.Equal(t, "keep", pod.Annotations["unrelated-annotation"],
		"unrelated annotations should be preserved")
}

// safetyTestDeploy is the deployment used by safety observation tests.
// Declared at package level so tests can pass it as the workloads arg
// to checkPendingSafetyObservations.
var safetyTestDeploy = newTestDeployment("api-server", "default", nil)

// newSafetyTestReconciler creates a reconciler with a pod and a matching
// deployment for safety observation tests. The deploy satisfies the
// provenance check in checkPendingSafetyObservations.
func newSafetyTestReconciler(pod *corev1.Pod) (*AttunePolicyReconciler, client.Client) {
	return newResizeReconciler(pod, safetyTestDeploy)
}

// safetyWorkloads returns the workloads slice for safety observation tests.
func safetyWorkloads() []client.Object {
	return []client.Object{safetyTestDeploy}
}

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
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.False(t, hasResizedAt, "resized-at annotation should be removed")
	_, hasContainer := updated.Annotations["attune.io/resized-container"]
	assert.False(t, hasContainer, "resized-container annotation should be removed")
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

// ---------- isCooldownActive parse error ----------

func TestIsCooldownActive_MalformedDate(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	policy := newTestPolicy("test-policy", "default")
	policy.Annotations = map[string]string{
		lastResizeAnnotation: "not-a-valid-date",
	}
	assert.False(t, reconciler.isCooldownActive(policy))
}

// ---------- executeResizes ----------

func TestExecuteResizes_NoClientset(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	policy := newTestPolicy("test-policy", "default")

	count, history := reconciler.executeResizes(context.Background(), policy, nil, nil, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Nil(t, history)
}

func TestExecuteResizes_SuccessfulResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)
	require.Len(t, history, 2, "expect one cpu + one memory history entry")
	assert.Equal(t, "api-server", history[0].Workload)
	assert.Equal(t, "main", history[0].Container)
	assert.Equal(t, "InPlace", history[0].Method)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result, "cpu resize should succeed")
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[1].Result, "memory resize should succeed")
}

func TestExecuteResizes_ContextCancelledAbortsRemaining(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, history := reconciler.executeResizes(ctx, policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "no resizes should complete with cancelled context")
	assert.Empty(t, history, "no history entries with cancelled context")
}

func TestExecuteResizes_SkipsMatchingResources(t *testing.T) {
	// Pod already at the recommended values.
	pod := newResizePod("api-server", "750m", "384Mi", "1500m", "768Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "0", "0", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_NoMatchingWorkload(t *testing.T) {
	deploy := newTestDeployment("other-app", "default", nil)
	reconciler := newReconcilerWithClient(deploy)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "api-server", Kind: "Deployment"},
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_SkipsStaleRecommendation(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient(deploy)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "api-server", Kind: "Deployment", Stale: true},
	}

	before := promtestutil.ToFloat64(operatormetrics.StaleRecommendationsTotal.WithLabelValues("default", "test-policy"))
	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	after := promtestutil.ToFloat64(operatormetrics.StaleRecommendationsTotal.WithLabelValues("default", "test-policy"))
	assert.Equal(t, before+1, after, "StaleRecommendationsTotal should increment with policy labels")
}

// ---------- listWorkloadsBySelector (StatefulSet + DaemonSet paths) ----------

func TestListWorkloadsBySelector_StatefulSets(t *testing.T) {
	sts1 := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "default", Labels: map[string]string{"tier": "db"}},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db-1"}},
		},
	}
	sts2 := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db-2", Namespace: "default", Labels: map[string]string{"tier": "db"}},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db-2"}},
		},
	}
	r := newReconcilerWithClient(sts1, sts2)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "db"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "StatefulSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 2)
}

func TestListWorkloadsBySelector_DaemonSets(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "log-agent", Namespace: "default", Labels: map[string]string{"role": "logging"}},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "log-agent"}},
		},
	}
	r := newReconcilerWithClient(ds)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "logging"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "DaemonSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "log-agent", workloads[0].GetName())
}

func TestListWorkloadsBySelector_UnsupportedKind(t *testing.T) {
	r := newReconcilerWithClient()

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "ConfigMap", selector)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// ---------- getWorkloadByName (DaemonSet + unsupported kind) ----------

func TestDiscoverWorkloads_FindsDaemonSetByName(t *testing.T) {
	name := "node-agent"
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
	r := newReconcilerWithClient(ds)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "DaemonSet",
				Name: &name,
			},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsCronJobByName(t *testing.T) {
	name := "nightly-report"
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "report", Image: "report:latest"}},
						},
					},
				},
			},
		},
	}
	r := newReconcilerWithClient(cj)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "CronJob", Name: &name},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsJobByName(t *testing.T) {
	name := "data-migration"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "migrate", Image: "migrate:latest"}},
				},
			},
		},
	}
	r := newReconcilerWithClient(job)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Job", Name: &name},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestGetContainers_CronJob(t *testing.T) {
	r := NewAttunePolicyReconciler()
	cj := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "worker", Image: "worker:1"},
								{Name: "sidecar", Image: "sidecar:1"},
							},
						},
					},
				},
			},
		},
	}
	containers := r.getContainers(cj)
	require.Len(t, containers, 2)
	assert.Equal(t, "worker", containers[0].Name)
	assert.Equal(t, "sidecar", containers[1].Name)
}

func TestGetPodSelectorLabels_CronJob(t *testing.T) {
	r := NewAttunePolicyReconciler()
	cj := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "report"}},
					},
				},
			},
		},
	}
	labels := r.getPodSelectorLabels(cj)
	assert.Equal(t, map[string]string{"app": "report"}, labels)
}

func TestIsBatchWorkload(t *testing.T) {
	assert.True(t, isBatchWorkload(&batchv1.CronJob{}))
	assert.True(t, isBatchWorkload(&batchv1.Job{}))
	assert.False(t, isBatchWorkload(&appsv1.Deployment{}))
	assert.False(t, isBatchWorkload(&appsv1.StatefulSet{}))
	assert.False(t, isBatchWorkload(&appsv1.DaemonSet{}))
	assert.False(t, isBatchWorkload(&appsv1.ReplicaSet{}))
}

func TestListWorkloadsBySelector_CronJobs(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "report", Namespace: "default", Labels: map[string]string{"tier": "batch"}},
	}
	r := newReconcilerWithClient(cj)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "batch"}}
	result, err := r.listWorkloadsBySelector(context.Background(), "default", "CronJob", selector)
	assert.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "report", result[0].GetName())
}

func TestListWorkloadsBySelector_Jobs(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "default", Labels: map[string]string{"tier": "batch"}},
	}
	r := newReconcilerWithClient(job)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "batch"}}
	result, err := r.listWorkloadsBySelector(context.Background(), "default", "Job", selector)
	assert.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "migrate", result[0].GetName())
}

func TestDiscoverWorkloads_UnsupportedKind(t *testing.T) {
	name := "my-configmap"
	r := newReconcilerWithClient()

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "ConfigMap",
				Name: &name,
			},
		},
	}

	_, err := r.discoverWorkloads(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// ---------- getPodSelectorLabels (StatefulSet + DaemonSet) ----------

func TestGetPodSelectorLabels_StatefulSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	sts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
		},
	}
	labels := r.getPodSelectorLabels(sts)
	assert.Equal(t, map[string]string{"app": "db"}, labels)
}

func TestGetPodSelectorLabels_DaemonSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	ds := &appsv1.DaemonSet{
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
		},
	}
	labels := r.getPodSelectorLabels(ds)
	assert.Equal(t, map[string]string{"app": "agent"}, labels)
}

func TestGetPodSelectorLabels_NilSelector(t *testing.T) {
	r := NewAttunePolicyReconciler()
	dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{}}
	labels := r.getPodSelectorLabels(dep)
	assert.Nil(t, labels)
}

// ---------- getContainers (DaemonSet) ----------

func TestGetContainers_DaemonSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	ds := &appsv1.DaemonSet{
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "agent", Image: "fluentd"},
					},
				},
			},
		},
	}
	containers := r.getContainers(ds)
	assert.Len(t, containers, 1)
	assert.Equal(t, "agent", containers[0].Name)
}

func TestGetContainers_UnknownType(t *testing.T) {
	r := NewAttunePolicyReconciler()
	containers := r.getContainers(&corev1.Pod{})
	assert.Nil(t, containers)
}

func TestGetContainers_IncludesNativeSidecars(t *testing.T) {
	r := NewAttunePolicyReconciler()
	always := corev1.ContainerRestartPolicyAlways
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "istio-proxy", RestartPolicy: &always, Image: "istio"},
						{Name: "init-db", Image: "busybox"}, // regular init, NOT a native sidecar
					},
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx"},
					},
				},
			},
		},
	}
	containers := r.getContainers(deploy)
	require.Len(t, containers, 2) // istio-proxy + app, NOT init-db
	assert.Equal(t, "istio-proxy", containers[0].Name)
	assert.Equal(t, "app", containers[1].Name)
}

func TestGetContainers_NoNativeSidecars(t *testing.T) {
	r := NewAttunePolicyReconciler()
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-db", Image: "busybox"}, // regular init
					},
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx"},
					},
				},
			},
		},
	}
	containers := r.getContainers(deploy)
	require.Len(t, containers, 1)
	assert.Equal(t, "app", containers[0].Name)
}

// ---------- mergeDefaults (more paths) ----------

func TestMergeDefaults_MergesAllFields(t *testing.T) {
	queryStep := metav1.Duration{Duration: 30 * time.Second}
	historyWindow := metav1.Duration{Duration: 48 * time.Hour}
	cooldown := metav1.Duration{Duration: 30 * time.Minute}
	autoRevert := true
	controlledValues := "RequestsAndLimits"
	burstSensitivity := "0.2"
	allowDecrease := true
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile:       90,
				Overhead:         "50",
				ControlledValues: &controlledValues,
				BurstSensitivity: &burstSensitivity,
				MinAllowed:       quantityPtr("100m"),
				MaxAllowed:       quantityPtr("8"),
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
				MaxChangePercent: int32Ptr(80),
			},
			Memory: &attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "40",
				AllowDecrease:    &allowDecrease,
				MaxChangePercent: int32Ptr(60),
			},
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep:         &queryStep,
				HistoryWindow:     &historyWindow,
				MinimumDataPoints: int32Ptr(24),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                   attunev1alpha1.UpdateTypeAuto,
				Cooldown:               &cooldown,
				AutoRevert:             &autoRevert,
				ResizeMethod:           attunev1alpha1.ResizeMethodInPlaceOrRecreate,
				MaxConcurrentResizes:   5,
				MaxTotalCPUIncrease:    quantityPtr("2000m"),
				MaxTotalMemoryIncrease: quantityPtr("4Gi"),
				Schedule: &attunev1alpha1.ResizeSchedule{
					DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
					Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
				},
				Canary: &attunev1alpha1.CanaryConfig{
					Percentage:        10,
					AutoPromote:       true,
					ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
				},
				SafetyObservationPeriod: &metav1.Duration{Duration: 3 * time.Minute},
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with all zeros/empty (should inherit from defaults).
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	r.mergeDefaults(policy, defaults)

	// CPU
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "50", policy.Spec.CPU.Overhead)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, "RequestsAndLimits", *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.CPU.BurstSensitivity)
	assert.Equal(t, "0.2", *policy.Spec.CPU.BurstSensitivity)
	require.NotNil(t, policy.Spec.CPU.MinAllowed)
	assert.Equal(t, resource.MustParse("100m"), *policy.Spec.CPU.MinAllowed)
	require.NotNil(t, policy.Spec.CPU.StartupBoost)
	assert.Equal(t, "3.0", policy.Spec.CPU.StartupBoost.Multiplier)
	assert.Equal(t, 2*time.Minute, policy.Spec.CPU.StartupBoost.Duration.Duration)

	// Memory
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, "40", policy.Spec.Memory.Overhead)
	require.NotNil(t, policy.Spec.Memory.AllowDecrease)
	assert.True(t, *policy.Spec.Memory.AllowDecrease)

	// MetricsSource
	require.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, 30*time.Second, policy.Spec.MetricsSource.QueryStep.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Equal(t, 48*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, int32(24), *policy.Spec.MetricsSource.MinimumDataPoints)

	// UpdateStrategy
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, int32(5), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxTotalCPUIncrease)
	assert.Equal(t, resource.MustParse("2000m"), *policy.Spec.UpdateStrategy.MaxTotalCPUIncrease)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease)
	assert.Equal(t, resource.MustParse("4Gi"), *policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease)
	require.NotNil(t, policy.Spec.UpdateStrategy.Schedule)
	assert.Equal(t, []string{"Monday", "Wednesday", "Friday"}, policy.Spec.UpdateStrategy.Schedule.DaysOfWeek)
	require.NotNil(t, policy.Spec.UpdateStrategy.Canary)
	assert.Equal(t, int32(10), policy.Spec.UpdateStrategy.Canary.Percentage)
	assert.True(t, policy.Spec.UpdateStrategy.Canary.AutoPromote)
	assert.Equal(t, 5*time.Minute, policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.SafetyObservationPeriod)
	assert.Equal(t, 3*time.Minute, policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration)
}

func TestApplyBuiltInDefaults_FillsAllFields(t *testing.T) {
	r := newReconcilerWithClient()
	// Create a policy with ALL fields unset (no webhook defaults, no cluster defaults).
	policy := &attunev1alpha1.AttunePolicy{}

	r.applyBuiltInDefaults(policy)

	// Every field should now have a built-in default value.
	assert.Equal(t, attunev1alpha1.DefaultUpdateType, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultCPUMaxChangePercent, *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultMemoryMaxChangePercent, *policy.Spec.Memory.MaxChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, time.Hour, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, attunev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
	require.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Equal(t, 168*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, 5*time.Minute, policy.Spec.MetricsSource.QueryStep.Duration)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
}

func TestApplyBuiltInDefaults_PreservesUserValues(t *testing.T) {
	r := newReconcilerWithClient()
	// Create a policy with explicit user values.
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(80)
	policy.Spec.Memory.MaxChangePercent = int32Ptr(60)
	autoRevert := false
	policy.Spec.UpdateStrategy.AutoRevert = &autoRevert
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(24)
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 30 * time.Minute}
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 48 * time.Hour}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 30 * time.Second}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv

	r.applyBuiltInDefaults(policy)

	// User values should be preserved, not overwritten.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.Equal(t, int32(24), *policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	assert.Equal(t, 48*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	assert.Equal(t, 30*time.Second, policy.Spec.MetricsSource.QueryStep.Duration)
	assert.Equal(t, attunev1alpha1.ControlledRequestsAndLimits, *policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.ControlledRequestsAndLimits, *policy.Spec.Memory.ControlledValues)
}

func TestMergeDefaults_ClusterDefaultsTakeEffect(t *testing.T) {
	// This is the #267 regression test: verify that cluster defaults actually
	// override built-in defaults when the webhook does not pre-fill fields.
	cooldown := metav1.Duration{Duration: 30 * time.Minute}
	autoRevert := false
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(80),
			},
			Memory: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(60),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:         attunev1alpha1.UpdateTypeAuto,
				Cooldown:     &cooldown,
				AutoRevert:   &autoRevert,
				ResizeMethod: attunev1alpha1.ResizeMethodInPlaceOrRecreate,
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with ALL fields unset (as if no webhook defaulting occurred).
	policy := &attunev1alpha1.AttunePolicy{}

	r.mergeDefaults(policy, defaults)
	r.applyBuiltInDefaults(policy)

	// Cluster defaults should take effect.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
}

func TestMergeAndApplyDefaults_PartialClusterDefaults(t *testing.T) {
	// Admin sets only Mode and CPU MaxChangePercent; everything else nil.
	// After mergeDefaults + applyBuiltInDefaults, the inherited fields must
	// be preserved and the rest must get built-in defaults.
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(80),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}
	r := newReconcilerWithClient(defaults)
	policy := &attunev1alpha1.AttunePolicy{}

	r.mergeDefaults(policy, defaults)
	// Verify partial state before applyBuiltInDefaults.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Nil(t, policy.Spec.Memory.MaxChangePercent,
		"should still be nil before applyBuiltInDefaults")
	assert.Nil(t, policy.Spec.UpdateStrategy.AutoRevert)

	r.applyBuiltInDefaults(policy)
	// Inherited values preserved.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	// Built-in defaults fill the rest.
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultMemoryMaxChangePercent, *policy.Spec.Memory.MaxChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, attunev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
}

func TestMergeDefaults_QueryStepPolicyOverrides(t *testing.T) {
	defaultStep := metav1.Duration{Duration: 30 * time.Second}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep: &defaultStep,
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	policyStep := metav1.Duration{Duration: 1 * time.Minute}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	policy.Spec.MetricsSource.QueryStep = &policyStep

	r.mergeDefaults(policy, defaults)

	// Policy-level value should NOT be overwritten.
	assert.Equal(t, 1*time.Minute, policy.Spec.MetricsSource.QueryStep.Duration)
}

func TestMergeDefaults_PolicyOverridesDefaults(t *testing.T) {
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU:    &attunev1alpha1.ResourceConfig{Percentile: 90, Overhead: "50"},
			Memory: &attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "40"},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with explicit values (should NOT be overwritten).
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU:    attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "10"},
			Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "20"},
		},
	}

	r.mergeDefaults(policy, nil)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "10", policy.Spec.CPU.Overhead)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "20", policy.Spec.Memory.Overhead)
}

// ---------- appendHistory ----------

func TestAppendHistory_CapsAtMaxEntries(t *testing.T) {
	existing := make([]attunev1alpha1.ResizeHistoryEntry, maxHistoryEntries-2)
	for i := range existing {
		existing[i] = attunev1alpha1.ResizeHistoryEntry{Workload: fmt.Sprintf("w-%d", i)}
	}
	newEntries := []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "new-1"},
		{Workload: "new-2"},
		{Workload: "new-3"},
		{Workload: "new-4"},
	}

	result := appendHistory(existing, newEntries, maxHistoryEntries)
	assert.Len(t, result, maxHistoryEntries)
	assert.Equal(t, "w-2", result[0].Workload)
	assert.Equal(t, "new-4", result[maxHistoryEntries-1].Workload)
}

// ---------- Reconcile with OneShot mode (exercises resize path entry) ----------

func TestReconcile_OneShotMode_NoClientset_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
}

// ---------- Reconcile with Prometheus error ----------

func TestReconcile_PrometheusUnavailable(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("connection refused")
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "PrometheusUnavailable", updated.Status.Conditions[0].Reason)
}

func TestReconcile_PrometheusQueryErrorsMentionBlockedDataTypes(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory_working_set_bytes") {
				return nil, fmt.Errorf("memory query failed")
			}
			return map[string][]rsmetrics.Sample{"main": generateSamples(200, 0.1)}, nil
		},
	}, policy, deploy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, reconciler.parseCooldown(policy), result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonMonitoring, cond.Reason)
	assert.Contains(t, cond.Message, "Watching 1 workloads, 1 with recommendations")
	assert.Contains(t, cond.Message, "Prometheus query errors (1)")
	assert.Contains(t, cond.Message, "memory data collection")
	assert.NotContains(t, cond.Message, "CPU and/or memory")
}

func TestReconcile_PrometheusQueryErrorsMentionCPUAndMemoryWhenBothFail(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}, policy, deploy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, reconciler.parseCooldown(policy), result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonPrometheusUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "Prometheus query errors (2)")
	assert.Contains(t, cond.Message, "CPU and memory data collection")
}

// ---------- resolveCanaryPhase ----------

func TestResolveCanaryPhase_InitializesOnFirstCall(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "first call should stay in canary mode")
	require.NotNil(t, policy.Status.Canary)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.NotNil(t, policy.Status.Canary.StartTime)
}

func TestResolveCanaryPhase_PromotesAfterObservation(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	// No reverts in history.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(startTime.Add(1 * time.Minute))},
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode, "should promote to auto after observation passes")
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_LegacyHistoryWithoutMethodPromotesCanary(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Result: attunev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(startTime.Add(1 * time.Minute))},
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode, "legacy in-place history without method should still promote canary")
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_EvictionDoesNotPromoteCanary(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: metav1.NewTime(startTime.Add(1 * time.Minute))},
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "eviction-only history should not count as a successful canary resize")
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_WaitsDuringObservation(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "should stay in canary during observation")
}

func TestResolveCanaryPhase_BlocksOnRevert(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	// Revert happened during observation.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Result: attunev1alpha1.ResizeResultReverted, Timestamp: metav1.NewTime(startTime.Add(2 * time.Minute))},
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "should block promotion when revert happened")
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_FullRolloutStaysAuto(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseFullRollout,
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode, "FullRollout should map to Auto")
}

func TestResolveCanaryPhase_ResetsOnSpecChange(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Generation = 3
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	// Canary was started at generation 2 -- spec has since changed.
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:              attunev1alpha1.CanaryPhaseFullRollout,
		StartTime:          &startTime,
		ObservedGeneration: 2,
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	// Should reset and re-initialize, staying in canary mode.
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "spec change should reset canary, not stay in FullRollout")
	require.NotNil(t, policy.Status.Canary)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.Equal(t, int64(3), policy.Status.Canary.ObservedGeneration, "new cycle should track current generation")
}

func TestResolveCanaryPhase_NoResetWhenGenerationMatches(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Generation = 2
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:              attunev1alpha1.CanaryPhaseInProgress,
		StartTime:          &startTime,
		ObservedGeneration: 2,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(startTime.Add(1 * time.Minute))},
	}

	reconciler := NewAttunePolicyReconciler()
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	// Same generation: should promote normally after observation period.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode, "same generation should promote normally")
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

// ---------- Reconcile with cooldown active ----------

func TestReconcile_CooldownActive_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot
	policy.Annotations = map[string]string{
		lastResizeAnnotation: time.Now().UTC().Format(time.RFC3339),
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
}

// ---------- History-based Resized count derivation ----------

func TestReconcile_HistoryBasedResizedDerivation(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name        string
		mode        attunev1alpha1.UpdateType
		history     []attunev1alpha1.ResizeHistoryEntry
		wantResized int32
	}{
		{
			name: "derives Resized from distinct successful in-place workloads",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "worker", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 2,
		},
		{
			name: "evicted workloads do not count as resized",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: now},
				{Workload: "worker", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "legacy successful history without method still counts as resized",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "worker", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 2,
		},
		{
			name: "only failed and reverted entries leave Resized at 0",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed, Timestamp: now},
				{Workload: "worker", Method: "InPlace", Result: attunev1alpha1.ResizeResultReverted, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "duplicate workload entries counted as one",
			mode: attunev1alpha1.UpdateTypeOneShot,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed, Timestamp: now},
			},
			wantResized: 1,
		},
		{
			name:        "empty history leaves Resized at 0",
			mode:        attunev1alpha1.UpdateTypeOneShot,
			history:     nil,
			wantResized: 0,
		},
		{
			name: "Recommend mode skips derivation entirely",
			mode: attunev1alpha1.UpdateTypeRecommend,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 0,
		},
		{
			name: "Observe mode skips derivation entirely",
			mode: attunev1alpha1.UpdateTypeObserve,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: now},
			},
			wantResized: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Type = tt.mode
			// Set cooldown annotation so resize execution is skipped;
			// this isolates the history-based derivation path.
			if isResizeMode(tt.mode) {
				policy.Annotations = map[string]string{
					lastResizeAnnotation: time.Now().UTC().Format(time.RFC3339),
				}
			}
			policy.Status.ResizeHistory = tt.history

			deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
			pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}
			reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
			})
			require.NoError(t, err)

			var updated attunev1alpha1.AttunePolicy
			require.NoError(t, fakeClient.Get(context.Background(),
				types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated))

			assert.Equal(t, tt.wantResized, updated.Status.Workloads.Resized,
				"Resized count should match derived value from history")
		})
	}
}

// ---------- Reconcile with opt-out annotation ----------

func TestSafeInt32_Normal(t *testing.T) {
	assert.Equal(t, int32(42), safeInt32(42))
	assert.Equal(t, int32(0), safeInt32(0))
}

func TestSafeInt32_Overflow(t *testing.T) {
	assert.Equal(t, int32(math.MaxInt32), safeInt32(math.MaxInt32+1))
	assert.Equal(t, int32(math.MaxInt32), safeInt32(math.MaxInt))
}

func TestExecuteResizes_SkipsQoSChange(t *testing.T) {
	// Pod is Guaranteed class (requests == limits).
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_GuaranteedQoS_MemoryClampAllowsCPUResize(t *testing.T) {
	// Regression test for the E2E failure TestE2E_GuaranteedQoS_RequestsAndLimits.
	// Guaranteed QoS pod (requests == limits). The recommendation decreases both
	// CPU and memory. The memory limit clamp preserves the current memory limit
	// (NotRequired resize policy forbids in-place memory limit decreases). Before
	// the fix, the clamped memory limit (256Mi) != recommended memory request (64Mi)
	// caused PreservesQoS to block the entire resize, including CPU.
	// After the fix, the memory request is raised to match the clamped limit,
	// preserving Guaranteed QoS and allowing CPU to resize.
	pod := newResizePod("qos-app", "500m", "256Mi", "500m", "256Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	// No resizePolicy set → defaults to NotRequired for memory.
	deploy := newTestDeployment("qos-app", "default", map[string]string{"app": "qos-app"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	// Recommend CPU decrease (500m → 50m) and memory decrease (256Mi → 64Mi).
	// Both limits also decrease (ControlledValues: RequestsAndLimits behavior).
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("qos-app", "500m", "256Mi", "500m", "256Mi", "50m", "64Mi", "50m", "64Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("qos-app", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should succeed: CPU changes even though memory is clamped")
	assert.NotEmpty(t, history, "should have resize history entries")
}

func TestExecuteResizes_QoSBlocked_EmitsResizeSkippedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "250m", "256Mi", "500m", "512Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "ResizeSkipped")
		assert.Contains(t, event, "controlledValues")
	default:
		t.Fatal("expected a ResizeSkipped event but channel was empty")
	}
}

func TestExecuteResizes_ResizeError(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	// Inject an error on UpdateResize calls.
	reconciler.Clientset.(*kubefake.Clientset).PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("node has insufficient resources")
		}
		return false, nil, nil
	})

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, history[0].Result)
}

func TestExecuteResizes_ResizeError_EmitsResizeFailedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	reconciler.Clientset.(*kubefake.Clientset).PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("node has insufficient resources")
		}
		return false, nil, nil
	})

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)

	// Drain all events and check for at least one ResizeFailed event.
	foundFailed := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeFailed") {
				assert.Contains(t, event, "api-server")
				foundFailed = true
			}
		default:
			goto doneFailed
		}
	}
doneFailed:
	assert.True(t, foundFailed, "expected at least one ResizeFailed event")
}

func TestExecuteResizes_AutoRevert_SafeVerdictNoRevert(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// Resize was attempted. The safety check runs immediately but with a
	// fake clientset the pod won't have conditions set, so CheckPod will
	// return Safe (no restart detected). This exercises the autoRevert
	// code path even though it does not trigger a revert.
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result)
}

func TestReconcile_WorkloadOptedOut(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Annotations = map[string]string{conflict.AnnotationSkip: "true"}
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	// Workload was discovered but skipped, so no recommendations.
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
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

	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	})
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
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-restart-count":       "3",
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
				// RestartCount 4 is within threshold (baseline 3 + 2 = 5),
				// so the pod should be considered safe and annotations removed.
				{Name: "main", RestartCount: 4},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

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
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-restart-count":       "3",
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
				"attune.io/resized-at":                   resizedAt,
				"attune.io/resized-workload":             "api-server",
				"attune.io/resized-containers":           "main",
				"attune.io/original-cpu-request.main":    "500m",
				"attune.io/original-memory-request.main": "512Mi",
				"attune.io/original-restart-count":       "not-a-number",
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
				// RestartCount 1 with baseline defaulting to 0 (parse failed):
				// 1 < 0+2 = safe, annotations should be removed normally.
				{Name: "main", RestartCount: 1},
			},
		},
	}

	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "bad-annotation-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	// Invalid restart count should default to 0; pod with 1 restart is safe.
	_, hasResizedAt := updated.Annotations["attune.io/resized-at"]
	assert.False(t, hasResizedAt, "pod should complete observation despite invalid restart count")
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

// ---------- getPodsForWorkload error path ----------

func TestGetPodsForWorkload_EmptySelectorLabels(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "no-selector", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{},
	}
	reconciler := newReconcilerWithClient(deploy)

	_, err := reconciler.getPodsForWorkload(context.Background(), deploy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no pod selector labels")
}

// ---------- buildPrometheusQuery unknown metric ----------

func TestBuildPrometheusQuery_UnknownMetric(t *testing.T) {
	query := buildPrometheusQuery("default", "api-server", "main", "disk", 5*time.Minute)
	assert.Empty(t, query)
}

func TestEscapePromQL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"clean string", "my-namespace", "my-namespace"},
		{"double quote", `my"ns`, `my\"ns`},
		{"backslash", `my\ns`, `my\\ns`},
		{"both", `a"b\c`, `a\"b\\c`},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rsmetrics.EscapePromQL(tt.input))
		})
	}
}

func TestBuildPrometheusQuery_EscapesSpecialChars(t *testing.T) {
	// Namespace and container are escaped by buildPrometheusQuery.
	// Pod regex is pre-built and passed through as-is.
	query := buildPrometheusQuery(`ns"test`, `pod-regex-[a-z]+`, `con"tainer`, "cpu", 5*time.Minute)
	assert.Contains(t, query, `ns\"test`)
	assert.Contains(t, query, `pod-regex-[a-z]+`)
	assert.Contains(t, query, `con\"tainer`)
}

func TestGetPodRegex_EscapesSpecialCharsInName(t *testing.T) {
	// The dot in "my.app" should be regex-escaped then PromQL-string-escaped
	// by getPodRegex so the PromQL regex matches a literal dot.
	r := NewAttunePolicyReconciler()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "my.app"}}
	regex := r.getPodRegex(dep)
	assert.Equal(t, `my\\.app-[a-z0-9]+-[a-z0-9]{5}`, regex)
}

func TestGetPodRegex_BatchPatternsDoNotMatchSimilarlyNamedWorkloads(t *testing.T) {
	r := NewAttunePolicyReconciler()

	jobRegex := regexp.MustCompile("^" + r.getPodRegex(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "data-migrate"}}) + "$")
	assert.True(t, jobRegex.MatchString("data-migrate-abc12"))
	assert.False(t, jobRegex.MatchString("data-migrate-v2-abc12"))

	cronRegex := regexp.MustCompile("^" + r.getPodRegex(&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly-report"}}) + "$")
	assert.True(t, cronRegex.MatchString("nightly-report-1716116400-abc12"))
	assert.False(t, cronRegex.MatchString("nightly-report-v2-1716116400-abc12"))
}

// ---------- listWorkloadsBySelector invalid selector ----------

func TestListWorkloadsBySelector_InvalidSelector(t *testing.T) {
	r := newReconcilerWithClient()

	// matchExpressions with invalid operator to trigger parse error.
	invalidSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tier", Operator: "BadOperator", Values: []string{"api"}},
		},
	}
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "Deployment", invalidSelector)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing label selector")
}

// ---------- discoverWorkloads with missing name ----------

func TestDiscoverWorkloads_NoNameOrSelector(t *testing.T) {
	r := newReconcilerWithClient()

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
			},
		},
	}

	_, err := r.discoverWorkloads(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must specify either name or selector")
}

// ---------- Reconcile with MetricsFactory error ----------

func TestReconcile_MetricsFactoryError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("TLS handshake timeout")
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "PrometheusUnavailable", updated.Status.Conditions[0].Reason)
	assert.Contains(t, updated.Status.Conditions[0].Message, "TLS handshake timeout")
}

func TestReconcile_BearerTokenSecretReadErrorIncludesSecretRef(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &attunev1alpha1.SecretKeyRef{
		Name: "prom-token",
		Key:  "token",
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy, deploy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonPrometheusUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "prom-token/token")
	assert.Contains(t, cond.Message, "reading secret default/prom-token")
}

// ---------- Reconcile with workload discovery error ----------

func TestReconcile_DiscoverWorkloadsError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Kind = "ConfigMap"

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err, "discovery errors should be surfaced via status condition, not returned")
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	// Verify the error is visible in the policy status condition.
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonWorkloadDiscoveryFailed, cond.Reason)
	assert.Contains(t, cond.Message, "Failed to discover workloads")
}

func TestReconcile_FetchDefaultsErrorFailsClosed(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	clusterDefaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prometheus.default.svc:9090"},
			},
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, clusterDefaults, deploy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*attunev1alpha1.AttuneNamespaceDefaultsList); ok {
					return fmt.Errorf("simulated namespace defaults API failure")
				}
				return cw.List(ctx, list, opts...)
			},
		}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, failingClient, scheme)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}
	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, failingClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "Failed to fetch defaults")
	assert.Contains(t, cond.Message, "simulated namespace defaults API failure")
}

// ---------- Reconcile with AutoRevert checking safety observations ----------

func TestReconcile_AutoRevertCallsSafetyObservations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
}

// ---------- Reconcile mid-rollout skip ----------

func TestReconcile_SkipsMidRolloutWorkload(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Status.UpdatedReplicas = 1 // Only 1 of 2 updated (mid-rollout).
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
}

// ---------- excludedContainers ----------

func TestComputeRecommendations_ExcludedContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"istio-proxy"}

	// Deployment with two containers: main + istio-proxy sidecar.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)

	// Only "main" should have a recommendation; "istio-proxy" is excluded.
	assert.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
}

func TestComputeRecommendations_ExcludeAllContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"main"}

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "all containers excluded, should return nil")
}

// ---------- node capacity pre-check ----------

func TestExecuteResizes_SkipsWhenExceedsNodeCapacity(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("600m"), // less than recommended 750m
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy, node)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be skipped when total requests exceed node allocatable")
	assert.Empty(t, history)
}

func TestExecuteResizes_ProceedsWhenWithinNodeCapacity(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy, node)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should proceed when within node capacity")
}

// ---------- discoverPrometheus ----------

func TestDiscoverPrometheus_WellKnownService(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 9090}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:9090", addr)
}

func TestDiscoverPrometheus_WellKnownService_CustomPort(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:80", addr)
}

func TestDiscoverPrometheus_NoServiceFound(t *testing.T) {
	reconciler := newReconcilerWithClient()

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Empty(t, addr, "should return empty when no Prometheus service is found")
}

func TestDiscoverPrometheus_CachedResult(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	// First call discovers and caches.
	addr1 := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:80", addr1)

	// Second call returns cached result even after the service is deleted.
	require.NoError(t, reconciler.Delete(context.Background(), svc))
	addr2 := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, addr1, addr2, "should return cached address")
}

func TestDiscoverPrometheus_OperatorCRD_DefaultPort(t *testing.T) {
	prom := &unstructured.Unstructured{}
	prom.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus",
	})
	prom.SetName("k8s")
	prom.SetNamespace("monitoring")

	s := testScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusList"},
		&unstructured.UnstructuredList{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"},
		&unstructured.Unstructured{},
	)
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(prom).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = s

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-k8s.monitoring:9090", addr)
}

func TestDiscoverPrometheus_OperatorCRD_CustomPort(t *testing.T) {
	prom := &unstructured.Unstructured{}
	prom.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus",
	})
	prom.SetName("k8s")
	prom.SetNamespace("monitoring")
	require.NoError(t, unstructured.SetNestedField(prom.Object, int64(8080), "spec", "port"))

	s := testScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusList"},
		&unstructured.UnstructuredList{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"},
		&unstructured.Unstructured{},
	)
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(prom).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = s

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-k8s.monitoring:8080", addr)
}

func TestResolvePrometheusAddress_FallsBackToAutoDiscovery(t *testing.T) {
	// Policy has no Prometheus address, no AttuneDefaults, but a well-known service exists.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-prometheus",
			Namespace: "monitoring",
		},
	}
	reconciler := newReconcilerWithClient(svc)

	config, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus-kube-prometheus-prometheus.monitoring:9090", config.Address)
}

// ---------- Event emission ----------

func TestExecuteResizes_EmitsResizedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)

	// Drain all events and check for at least one Resized event.
	foundResized := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Resized") {
				foundResized = true
			}
		default:
			goto done
		}
	}
done:
	assert.True(t, foundResized, "expected at least one Resized event")
}

// ---------- Warning suppression ----------

func TestIsSuppressed(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		reason      string
		expected    bool
	}{
		{"no annotation", nil, "HPAConflict", false},
		{"empty annotation", map[string]string{"attune.io/suppress-warnings": ""}, "HPAConflict", false},
		{"single match", map[string]string{"attune.io/suppress-warnings": "HPAConflict"}, "HPAConflict", true},
		{"single no match", map[string]string{"attune.io/suppress-warnings": "VPAConflict"}, "HPAConflict", false},
		{"comma-separated match", map[string]string{"attune.io/suppress-warnings": "ConfigClamped,HPAConflict,CooldownActive"}, "HPAConflict", true},
		{"comma-separated no match", map[string]string{"attune.io/suppress-warnings": "ConfigClamped,VPAConflict"}, "HPAConflict", false},
		{"whitespace trimmed", map[string]string{"attune.io/suppress-warnings": "ConfigClamped, HPAConflict , CooldownActive"}, "HPAConflict", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isSuppressed(tt.annotations, tt.reason))
		})
	}
}

func TestEventDedup_SuppressesDuplicates(t *testing.T) {
	d := newEventDedup(time.Hour)
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "first should emit")
	assert.False(t, d.shouldEmit("policy1/HPAConflict/msg"), "duplicate within TTL should suppress")
	assert.True(t, d.shouldEmit("policy1/VPAConflict/msg"), "different reason should emit")
	assert.True(t, d.shouldEmit("policy2/HPAConflict/msg"), "different policy should emit")
}

func TestEventDedup_ReEmitsAfterTTL(t *testing.T) {
	d := newEventDedup(1 * time.Millisecond)
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "first should emit")
	time.Sleep(5 * time.Millisecond)
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "should re-emit after TTL")
}

func TestEventDedup_PrunesExpiredEntries(t *testing.T) {
	d := newEventDedup(1 * time.Millisecond)

	// Insert 5 entries that will expire.
	for i := 0; i < 5; i++ {
		d.shouldEmit(fmt.Sprintf("expired-%d", i))
	}
	time.Sleep(5 * time.Millisecond)

	// Add entries up to the 1000-call sweep threshold.
	for i := 5; i < 999; i++ {
		d.shouldEmit(fmt.Sprintf("filler-%d", i))
	}
	// At call 1000, the sweep should remove the 5 expired entries.
	d.shouldEmit("trigger-sweep")

	d.mu.Lock()
	for i := 0; i < 5; i++ {
		_, exists := d.seen[fmt.Sprintf("expired-%d", i)]
		assert.False(t, exists, "expired entry %d should have been pruned", i)
	}
	// Recent entries should still exist.
	_, exists := d.seen["trigger-sweep"]
	assert.True(t, exists, "recent entry should survive pruning")
	d.mu.Unlock()
}

func TestEventDedup_ConcurrentAccess(t *testing.T) {
	d := newEventDedup(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				d.shouldEmit(fmt.Sprintf("goroutine-%d", j))
			}
		}()
	}
	wg.Wait()
}

// ---------- Throttle integration ----------

// mockThrottleCollector extends mockCollector with ThrottleChecker.
type mockThrottleCollector struct {
	mockCollector
	throttleRatio float64
}

func (m *mockThrottleCollector) GetThrottleRatio(_ context.Context, _, _, _ string, _ time.Time) (float64, error) {
	return m.throttleRatio, nil
}

func TestExecuteResizes_ThrottleNotRevertedDuringGracePeriod(t *testing.T) {
	// The immediate post-resize safety check should NOT revert for throttle
	// because the Prometheus rate(…[5m]) window still contains 100% pre-resize
	// data. Throttle reverts should only happen during deferred observations
	// (>5 minutes after resize). See safety/monitor.go throttleGrace.
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	// Collector reports 60% throttle (above 50% threshold).
	// Despite this, the immediate check should skip the throttle evaluation
	// because the resize just happened (within the 5-minute grace period).
	collector := &mockThrottleCollector{throttleRatio: 0.6}

	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), collector, nil)
	// Resize should succeed and NOT be immediately reverted.
	assert.Equal(t, 1, count, "resize should succeed without immediate throttle revert")

	// History should show success, not revert.
	for _, h := range history {
		assert.NotEqual(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"should not have a throttle revert within the grace period")
	}
}

// ---------- annotation persistence and RestartCount capture (#27) ----------

// newResizePodWithStatus creates a pod with container statuses, suitable for
// testing annotation persistence where RestartCount needs to be captured.
func newResizePodWithStatus(deployName string, cpuReq, memReq, cpuLim, memLim string, restartCount int32) *corev1.Pod {
	pod := newResizePod(deployName, cpuReq, memReq, cpuLim, memLim)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:         "main",
			RestartCount: restartCount,
			Ready:        true,
		},
	}
	return pod
}

func TestExecuteResizes_PersistsAnnotations(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 7)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count, "resize should succeed")
	require.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result)

	// Verify annotations were persisted on the pod.
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.NotEmpty(t, updated.Annotations[annotationResizedAt], "resized-at annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, updated.Annotations[annotationResizedAt])
	assert.NoError(t, parseErr, "resized-at should be valid RFC3339")

	assert.Contains(t, updated.Annotations[annotationResizedContainers], "main")
	assert.Equal(t, "api-server", updated.Annotations[annotationResizedWorkload])
	assert.Equal(t, "500m", updated.Annotations[annotationOriginalCPUPrefix+"main"])
	assert.Equal(t, "512Mi", updated.Annotations[annotationOriginalMemoryPrefix+"main"])
	assert.Equal(t, "7", updated.Annotations[annotationOriginalRestartCountPrefix+"main"],
		"RestartCount should be captured from pre-resize container status")
}

func TestExecuteResizes_CapturesZeroRestartCount(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count)

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "0", updated.Annotations[annotationOriginalRestartCountPrefix+"main"],
		"zero RestartCount should still be persisted")
}

func TestExecuteResizes_PreservesExistingPodAnnotations(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	pod.Annotations = map[string]string{"existing-key": "existing-value"}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count)

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "existing-value", updated.Annotations["existing-key"],
		"pre-existing annotations must not be lost")
	assert.NotEmpty(t, updated.Annotations[annotationResizedAt],
		"resize annotations must be added alongside existing ones")
}

// ---------- revert on annotation persistence failure (#35) ----------

func TestExecuteResizes_RevertsOnAnnotationUpdateFailure(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Wrap the controller-runtime fake client to fail on the pod Update call
	// that persists annotations, while letting all other operations succeed.
	wrappedClient := &failOnPodUpdateClient{Client: fakeClient}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// The resize should have been reverted because annotation update failed.
	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	// History should show Reverted entries.
	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a Reverted entry")

	// Verify that a Reverted event was emitted.
	foundRevert := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "annotation-persist-failed") {
				foundRevert = true
			}
		default:
			goto checkRevert
		}
	}
checkRevert:
	assert.True(t, foundRevert, "expected a Reverted event mentioning annotation-persist-failed")

	// Verify the revert was issued via UpdateResize (second call: first is
	// the original resize, second is the revert).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original + revert")
}

// failOnPodUpdateClient wraps a client.Client and fails on Update calls for Pods.
// The first Get after a resize (re-fetch) succeeds, but the Update for
// annotation persistence fails. This simulates a 409 Conflict or similar error.
type failOnPodUpdateClient struct {
	client.Client
}

func (f *failOnPodUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return fmt.Errorf("simulated annotation update failure")
	}
	return f.Client.Update(ctx, obj, opts...)
}

type failOnNamedPodUpdateClient struct {
	client.Client
	failPodName string
}

func (f *failOnNamedPodUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if pod, ok := obj.(*corev1.Pod); ok && pod.Name == f.failPodName {
		f.failPodName = ""
		return fmt.Errorf("simulated annotation update failure")
	}
	return f.Client.Update(ctx, obj, opts...)
}

// conflictThenSucceedClient returns a 409 Conflict on the first N pod
// Update calls, then delegates to the real client. This simulates the
// kubelet bumping resourceVersion concurrently during multi-container resizes.
type conflictThenSucceedClient struct {
	client.Client
	mu            sync.Mutex
	conflictsLeft int
	conflictsSeen int
}

func (c *conflictThenSucceedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.mu.Lock()
		if c.conflictsLeft > 0 {
			c.conflictsLeft--
			c.conflictsSeen++
			c.mu.Unlock()
			return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(), fmt.Errorf("resourceVersion changed"))
		}
		c.mu.Unlock()
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestExecuteResizes_AnnotationConflictRetrySucceeds(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 1}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// Despite a conflict on the first annotation update attempt, the retry
	// should succeed and the resize should NOT be reverted.
	assert.Equal(t, 1, count, "resize should succeed after conflict retry")
	require.NotEmpty(t, history)
	for _, h := range history {
		assert.NotEqual(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"history should not contain Reverted entries after successful retry")
	}
	assert.Equal(t, 1, wrappedClient.conflictsSeen, "should have seen exactly 1 conflict")
}

func TestExecuteResizes_RevertFailureMarksHistoryAsFailed(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Wrap the controller-runtime fake client to fail on Pod Update
	// (annotation persistence), which triggers the revert path.
	wrappedClient := &failOnPodUpdateClient{Client: fakeClient}

	// Also make the revert's UpdateResize fail. The first UpdateResize call
	// (the original resize) should succeed, but the second one (the revert)
	// should fail.
	resizeCallCount := 0
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(k8stesting.UpdateAction)
		if !ok || updateAction.GetSubresource() != "resize" {
			return false, nil, nil
		}
		resizeCallCount++
		if resizeCallCount >= 2 {
			// Fail the revert resize call.
			return true, nil, fmt.Errorf("simulated revert failure")
		}
		return false, nil, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	_, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// History should show Failed (not Reverted or Success) because both
	// annotation persist and revert failed.
	require.NotEmpty(t, history)
	for _, h := range history {
		if h.Workload == "api-server" {
			assert.Equal(t, attunev1alpha1.ResizeResultFailed, h.Result,
				"history should be Failed when revert also fails, got %s", h.Result)
		}
	}
}

func TestExecuteResizes_RevertsOnReFetchFailure(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Inject failure on typed clientset Get for pods. ResizePod now does a
	// pre-resize re-fetch (call 1), then persistResizeAnnotations does a
	// post-resize re-fetch (call 2). Fail call 2 to test annotation-persist
	// revert. Subsequent Gets (revert's pod lookup) pass through.
	getCount := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getCount++
		if getCount == 2 {
			return true, nil, fmt.Errorf("simulated re-fetch failure")
		}
		return false, nil, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a Reverted entry for re-fetch failure")

	// Verify revert event.
	foundRevert := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "re-fetch-failed") {
				foundRevert = true
			}
		default:
			goto checkReFetch
		}
	}
checkReFetch:
	assert.True(t, foundRevert, "expected a Reverted event mentioning re-fetch-failed")

	// Verify revert UpdateResize was called.
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original + revert")
}

func TestBuildResizeTarget_OmitsLimitsWhenZero(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target, clamped := buildResizeTarget(rec)
	assert.Equal(t, int64(100), target.Requests.Cpu().MilliValue())
	wantMem := resource.MustParse("128Mi")
	assert.Equal(t, wantMem.Value(), target.Requests.Memory().Value())
	assert.Nil(t, target.Limits, "Limits should be nil when recommendation limits are zero")
	assert.Empty(t, clamped, "nothing should be clamped when no limits present")
}

func TestBuildResizeTarget_IncludesLimitsWhenNonZero(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
			MemoryLimit:   resource.MustParse("256Mi"),
		},
	}
	target, clamped := buildResizeTarget(rec)
	require.NotNil(t, target.Limits)
	assert.Equal(t, int64(200), target.Limits.Cpu().MilliValue())
	wantMemLim := resource.MustParse("256Mi")
	assert.Equal(t, wantMemLim.Value(), target.Limits.Memory().Value())
	assert.Empty(t, clamped, "nothing should be clamped when requests are below limits")
}

func TestBuildResizeTarget_PartialLimits(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target, clamped := buildResizeTarget(rec)
	require.NotNil(t, target.Limits, "Limits should be non-nil when any limit is non-zero")
	assert.Equal(t, int64(200), target.Limits.Cpu().MilliValue())
	_, hasMemLimit := target.Limits[corev1.ResourceMemory]
	assert.False(t, hasMemLimit, "Memory limit should not be set when zero in recommendation")
	assert.Empty(t, clamped, "nothing should be clamped when requests are below limits")
}

func TestBuildResizeTarget_ClampsRequestsToLimits(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("600m"),
			MemoryRequest: resource.MustParse("1Gi"),
			CPULimit:      resource.MustParse("500m"),  // Limit < Request
			MemoryLimit:   resource.MustParse("512Mi"), // Limit < Request
		},
	}
	target, clamped := buildResizeTarget(rec)
	// Requests should be clamped to limits.
	assert.Equal(t, resource.MustParse("500m"), target.Requests[corev1.ResourceCPU],
		"CPU request should be clamped to limit")
	assert.Equal(t, resource.MustParse("512Mi"), target.Requests[corev1.ResourceMemory],
		"Memory request should be clamped to limit")
	assert.ElementsMatch(t, []string{"cpu", "memory"}, clamped,
		"both CPU and memory should be reported as clamped")
}

func TestBuildResizeTarget_NoClampsWhenRequestsBelowLimits(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
			CPULimit:      resource.MustParse("500m"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}
	target, clamped := buildResizeTarget(rec)
	assert.Equal(t, resource.MustParse("200m"), target.Requests[corev1.ResourceCPU],
		"CPU request should not be modified when below limit")
	assert.Equal(t, resource.MustParse("256Mi"), target.Requests[corev1.ResourceMemory],
		"Memory request should not be modified when below limit")
	assert.Empty(t, clamped, "no resources should be clamped when requests are below limits")
}

func TestBuildResizeTarget_PartialClamping(t *testing.T) {
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("800m"),
			MemoryRequest: resource.MustParse("256Mi"),
			CPULimit:      resource.MustParse("500m"),  // Limit < Request (clamped)
			MemoryLimit:   resource.MustParse("512Mi"), // Limit > Request (not clamped)
		},
	}
	target, clamped := buildResizeTarget(rec)
	assert.Equal(t, resource.MustParse("500m"), target.Requests[corev1.ResourceCPU],
		"CPU request should be clamped to limit")
	assert.Equal(t, resource.MustParse("256Mi"), target.Requests[corev1.ResourceMemory],
		"Memory request should not be modified when below limit")
	assert.Equal(t, []string{"cpu"}, clamped,
		"only CPU should be reported as clamped")
}

func TestComputeRecommendations_NanInfSamplesMetric(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	// Return NaN/Inf samples so BuildProfile yields 0 data points.
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{
				"main": {
					{Timestamp: time.Now().Add(-1 * time.Hour), Value: math.NaN()},
					{Timestamp: time.Now().Add(-2 * time.Hour), Value: math.Inf(1)},
					{Timestamp: time.Now().Add(-3 * time.Hour), Value: math.Inf(-1)},
				},
			}, nil
		},
	}

	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, rec, "should produce no recommendation when all data is NaN/Inf")
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	assert.Equal(t, before+1, after, "NanInfSamplesTotal should increment for CPU")
}

func TestExecuteResizes_RequestClampedMetric(t *testing.T) {
	pod := newResizePod("api-server", "600m", "1Gi", "500m", "512Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	clientset := kubefake.NewSimpleClientset(pod)

	reconciler := newReconcilerWithClient(pod, deploy)
	reconciler.Clientset = clientset

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("800m"), // Will be clamped to 500m limit
						MemoryRequest: resource.MustParse("2Gi"),  // Will be clamped to 512Mi limit
						CPULimit:      resource.MustParse("500m"),
						MemoryLimit:   resource.MustParse("512Mi"),
					},
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("600m"),
						MemoryRequest: resource.MustParse("1Gi"),
						CPULimit:      resource.MustParse("500m"),
						MemoryLimit:   resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	beforeCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	beforeMem := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "memory"))

	reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)

	afterCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	afterMem := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "memory"))
	assert.Equal(t, beforeCPU+1, afterCPU, "RequestClampedTotal should increment for CPU")
	assert.Equal(t, beforeMem+1, afterMem, "RequestClampedTotal should increment for memory")
}

func TestTryEvictionFallback_EvictsWhenMultipleReplicas(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "success"))
	resizeBefore := promtestutil.ToFloat64(operatormetrics.ResizeTotal.WithLabelValues("default", "api-server", "eviction", "success"))

	evicted := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.True(t, evicted, "should evict when multiple replicas exist")

	// Verify eviction was called.
	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
	}
	assert.Equal(t, 1, evictions)
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "success")))
	assert.Equal(t, resizeBefore, promtestutil.ToFloat64(operatormetrics.ResizeTotal.WithLabelValues("default", "api-server", "eviction", "success")),
		"eviction fallback should not increment in-place resize metrics")
}

func TestTryEvictionFallback_SkipsLastReplica(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should NOT evict the last replica")
}

func TestResizeContainer_InfeasiblePodEvictedDirectly(t *testing.T) {
	// A pod marked Infeasible with InPlaceOrRecreate should go directly to
	// eviction without attempting another in-place resize.
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Status.Conditions = append(pod1.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	// Second pod so eviction is not blocked by last-replica protection.
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod1,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeEvicted, outcome, "infeasible pod should be evicted")
	require.Len(t, entries, 1)
	assert.Equal(t, "Eviction", entries[0].Method)
	assert.Equal(t, attunev1alpha1.ResizeResultEvicted, entries[0].Result)

	// Verify an eviction was actually issued, not a resize attempt.
	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
	}
	assert.Equal(t, 1, evictions, "should have issued exactly one eviction")

	// Verify NO resize was attempted (the pod was Infeasible, so we skip UpdateResize).
	var resizes int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "pods" && a.GetSubresource() == "resize" {
			resizes++
		}
	}
	assert.Equal(t, 0, resizes, "should NOT have attempted in-place resize on Infeasible pod")
}

func TestResizeContainer_InfeasiblePodSkippedWithInPlaceOnly(t *testing.T) {
	// An Infeasible pod with InPlaceOnly should be skipped entirely
	// (no resize attempt, no eviction).
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod.Name = "api-server-abc-1"
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	// InPlaceOnly (default): no eviction allowed.

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeNone, outcome, "infeasible pod with InPlaceOnly should not be resized")
	assert.Empty(t, entries, "should produce no history entries")

	// Verify InfeasibleBlocked event was emitted.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "InfeasibleBlocked")
		assert.Contains(t, event, "api-server-abc-1")
		assert.Contains(t, event, "InPlaceOrRecreate")
	default:
		t.Error("expected InfeasibleBlocked event but none was emitted")
	}

	// Verify NO resize and NO eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Error("should NOT have attempted in-place resize on Infeasible pod")
		}
		if a.GetVerb() == "create" && a.GetSubresource() == "eviction" {
			t.Error("should NOT have attempted eviction with InPlaceOnly")
		}
	}
}

func TestBudgetIncrease_PositiveIncrease(t *testing.T) {
	pod := newResizePod("api-server", "200m", "256Mi", "0", "0")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	cpu, mem := budgetIncrease(pod, "main", target)
	assert.Equal(t, int64(300), cpu, "CPU increase should be 300m")
	assert.Equal(t, int64(256*1024*1024), mem, "Memory increase should be 256Mi")
}

func TestBudgetIncrease_DecreaseClampsToZero(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "0", "0")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	cpu, mem := budgetIncrease(pod, "main", target)
	assert.Equal(t, int64(0), cpu, "CPU decrease should not count as budget increase")
	assert.Equal(t, int64(0), mem, "Memory decrease should not count as budget increase")
}

func TestBudgetIncrease_ContainerNotFound(t *testing.T) {
	pod := newResizePod("api-server", "200m", "256Mi", "0", "0")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	cpu, mem := budgetIncrease(pod, "nonexistent", target)
	assert.Equal(t, int64(0), cpu, "should return 0 for missing container")
	assert.Equal(t, int64(0), mem, "should return 0 for missing container")
}

func TestBudgetIncrease_MixedDirections(t *testing.T) {
	// CPU increases, memory decreases.
	pod := newResizePod("api-server", "200m", "512Mi", "0", "0")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	cpu, mem := budgetIncrease(pod, "main", target)
	assert.Equal(t, int64(300), cpu, "CPU increase should be 300m")
	assert.Equal(t, int64(0), mem, "Memory decrease should be clamped to 0")
}

func TestExecuteResizes_BudgetCapsDefersExcessiveIncrease(t *testing.T) {
	// Pod at 200m CPU, recommendation is 800m (increase of 600m).
	// Budget cap is 500m, so the resize should be skipped.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "800m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be deferred when CPU increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsAllowsWithinBudget(t *testing.T) {
	// Pod at 200m CPU, recommendation is 500m (increase of 300m).
	// Budget cap is 500m, so the resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should proceed when within budget")
}

func TestExecuteResizes_BudgetCapsDecreasesFree(t *testing.T) {
	// Pod at 800m CPU, recommendation is 400m (decrease of 400m).
	// Budget cap is 100m. Decreases should NOT consume budget.
	pod := newResizePod("api-server", "800m", "256Mi", "800m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("100m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "800m", "256Mi", "0", "0", "400m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "decreases should not consume budget")
}

func TestExecuteResizes_BudgetCapsMemory(t *testing.T) {
	// Pod at 256Mi memory, recommendation is 1Gi (increase of 768Mi).
	// Memory budget is 512Mi, so the resize should be deferred.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	memBudget := resource.MustParse("512Mi")
	policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = &memBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "200m", "1Gi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be deferred when memory increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsExactlyEqualsPasses(t *testing.T) {
	// Increase of exactly 500m with budget of 500m should pass (not strict >).
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "700m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "increase exactly equal to budget should proceed")
}

func TestExecuteResizes_BudgetCapsClampedTargetUsesAppliedIncrease(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "600m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("100m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "600m", "256Mi", "800m", "256Mi", "600m", "256Mi"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "budget should use the clamped applied increase, not the raw recommendation delta")
}

func TestExecuteResizes_BudgetCapsSkipDoesNotConsumeBudget(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod1, pod2), nil, nil)
	assert.Equal(t, 1, count, "a skipped pod should not consume budget needed by another pod")
}

func TestExecuteResizes_BudgetCapsResizeFailureDoesNotConsumeBudget(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	failedPod := pod1.Name
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		if updated.Name == failedPod {
			failedPod = ""
			return true, nil, fmt.Errorf("simulated resize failure")
		}
		return false, nil, nil
	})
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 1, count, "a failed resize should not consume budget needed by another pod")
	require.NotEmpty(t, history)
	assert.Contains(t, []attunev1alpha1.ResizeResult{history[0].Result}, attunev1alpha1.ResizeResultFailed)
}

func TestExecuteResizes_EvictionDoesNotConsumeBudgetNeededByNextPod(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Status.Conditions = append(pod1.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 1, count, "eviction fallback should not consume budget needed by the next pod")
	evicted := false
	succeeded := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultEvicted {
			evicted = true
		}
		if h.Method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			succeeded = true
		}
	}
	assert.True(t, evicted, "history should record the fallback eviction explicitly")
	assert.True(t, succeeded, "the next pod should still resize successfully in the same cycle")
}

func TestExecuteResizes_MixedOutcomePodDoesNotLeakSuccessOrBudget(t *testing.T) {
	apiPod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	apiPod1.Name = "api-server-abc-1"
	apiPod1.Spec.Containers = append(apiPod1.Spec.Containers, corev1.Container{
		Name:  "sidecar",
		Image: "busybox",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	})
	apiPod2 := apiPod1.DeepCopy()
	apiPod2.Name = "api-server-abc-2"
	workerPod := newResizePod("worker", "200m", "256Mi", "200m", "256Mi")
	workerPod.Name = "worker-abc-1"

	apiDeploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	workerDeploy := newTestDeployment("worker", "default", map[string]string{"app": "worker"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(apiDeploy, workerDeploy, apiPod1, apiPod2, workerPod).Build()
	clientset := kubefake.NewSimpleClientset(apiPod1.DeepCopy(), apiPod2.DeepCopy(), workerPod.DeepCopy())
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		if updated.Name != apiPod1.Name {
			return false, nil, nil
		}
		for _, c := range updated.Spec.Containers {
			if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
				return true, nil, fmt.Errorf("simulated sidecar resize failure")
			}
		}
		return false, nil, nil
	})
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 1
	cpuBudget := resource.MustParse("400m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("64Mi"),
					},
				},
			},
		},
		newResizeRecommendation("worker", "200m", "256Mi", "200m", "256Mi", "500m", "256Mi", "500m", "256Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{apiDeploy, workerDeploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*apiPod1}, "worker": {*workerPod}}, nil, nil)

	assert.Equal(t, 1, count, "only the worker workload should count as resized after api-server falls back to eviction")

	apiSuccesses := 0
	workerSuccesses := 0
	apiEvictions := 0
	for _, h := range history {
		if h.Workload == "api-server" && h.Method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			apiSuccesses++
		}
		if h.Workload == "worker" && h.Method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			workerSuccesses++
		}
		if h.Workload == "api-server" && h.Method == "Eviction" && h.Result == attunev1alpha1.ResizeResultEvicted {
			apiEvictions++
		}
	}
	assert.Equal(t, 0, apiSuccesses, "eviction fallback should clear earlier in-place success history for the same pod")
	assert.Equal(t, 2, workerSuccesses, "worker workload should still resize after api-server refunds its reserved budget")
	assert.Equal(t, 1, apiEvictions, "api-server should record the fallback eviction explicitly")
}

func TestExecuteResizes_BudgetCapsRevertDoesNotConsumeBudget(t *testing.T) {
	pod1 := newResizePodWithStatus("api-server", "200m", "256Mi", "200m", "256Mi", 0)
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePodWithStatus("api-server", "200m", "256Mi", "200m", "256Mi", 0)
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	wrappedClient := &failOnNamedPodUpdateClient{Client: fakeClient, failPodName: pod1.Name}
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 1, count, "a reverted resize should not consume budget needed by another pod")
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a reverted entry for the failed first pod")
}

func TestExecuteResizes_ConcurrentResizes(t *testing.T) {
	// Test that maxConcurrentResizes > 1 processes multiple pods without races.
	pod1 := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 5 // allow parallelism

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "0", "0", "750m", "384Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 1, count, "workload should count as resized once")
	assert.NotEmpty(t, history, "should produce resize history entries")
}

func TestExecuteResizes_MultiContainerSequential(t *testing.T) {
	// A pod with two containers should be resized sequentially.
	// persistResizeAnnotations propagates the fresh pod back to the caller
	// so the second container uses an up-to-date resourceVersion.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)
	assert.Equal(t, 1, count, "workload should be resized")

	// Both containers should have UpdateResize called.
	// In tests, the kubefake and controller-runtime fake are separate stores,
	// so the second container's annotation persistence may conflict. We verify
	// correctness by checking that UpdateResize was called for both containers
	// via the clientset actions.
	resizedContainers := make(map[string]bool)
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			for _, c := range updated.Spec.Containers {
				if c.Name == "main" && c.Resources.Requests.Cpu().MilliValue() == 750 {
					resizedContainers["main"] = true
				}
				if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
					resizedContainers["sidecar"] = true
				}
			}
		}
	}
	assert.True(t, resizedContainers["main"], "main container should have UpdateResize called")
	assert.True(t, resizedContainers["sidecar"], "sidecar container should have UpdateResize called")
}

func TestExecuteResizes_MultiContainer_BudgetExhaustion(t *testing.T) {
	// A pod with two containers where the CPU budget is exhausted after the
	// first container resize. The second container should be skipped (budget
	// check returns false), and the budget consumed by the first container
	// should NOT be refunded. The second workload also exceeds the remaining
	// budget and is similarly deferred.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	// Second workload to verify budget is consumed and not refunded.
	workerPod := newResizePod("worker", "200m", "128Mi", "0", "0")
	workerPod.Name = "worker-abc-1"

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	workerDeploy := newTestDeployment("worker", "default", map[string]string{"app": "worker"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, workerDeploy, pod, workerPod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy(), workerPod.DeepCopy())

	autoRevert := false
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = &autoRevert
	// Budget: 300m CPU. First container increases by 250m (500m→750m),
	// leaving 50m. Worker needs 150m (200m→350m) which exceeds remaining.
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
		newResizeRecommendation("worker", "200m", "128Mi", "0", "0", "350m", "128Mi", "0", "0"),
	}

	workloads := []client.Object{deploy, workerDeploy}
	podsByWorkload := map[string][]corev1.Pod{
		"api-server": {*pod},
		"worker":     {*workerPod},
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		workloads, recommendations, podsByWorkload, nil, nil)

	// 1. totalResized = 1: the api-server workload was resized (first container
	//    succeeded), but worker was not (budget exhausted).
	assert.Equal(t, 1, count, "exactly one workload should be counted as resized")

	// 2. History entries only contain the first container's resize.
	require.NotEmpty(t, history, "history should contain entries for the resized container")
	for _, h := range history {
		assert.Equal(t, "main", h.Container,
			"only main container should appear in history, got container %q", h.Container)
		assert.Equal(t, "api-server", h.Workload,
			"only api-server workload should appear in history, got workload %q", h.Workload)
	}

	// 3. UpdateResize was only called for main (not sidecar, not worker).
	resizedContainers := make(map[string]bool)
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			for _, c := range updated.Spec.Containers {
				if c.Name == "main" && c.Resources.Requests.Cpu().MilliValue() == 750 {
					resizedContainers["main"] = true
				}
				if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
					resizedContainers["sidecar"] = true
				}
			}
			if updated.Name == "worker-abc-1" {
				resizedContainers["worker"] = true
			}
		}
	}
	assert.True(t, resizedContainers["main"], "main container should have UpdateResize called")
	assert.False(t, resizedContainers["sidecar"], "sidecar container should NOT have UpdateResize called")
	assert.False(t, resizedContainers["worker"], "worker should NOT have UpdateResize called")

	// 4. Budget consumed for main (+250m) and NOT refunded: worker needs
	//    +150m but only 50m remains, so no worker history entries exist.
	for _, h := range history {
		if h.Workload == "worker" {
			t.Error("worker workload should not appear in history (budget exhausted)")
		}
	}
}

func TestReconcile_NowFuncControlsScheduleGate(t *testing.T) {
	// A policy with a schedule window of 02:00-06:00 UTC on Wednesdays.
	// When NowFunc returns a time outside the window, no resize should happen.
	// When NowFunc returns a time inside the window, resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
		DaysOfWeek: []string{"Wednesday"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod, policy).
		WithStatusSubresource(policy).Build()

	// Wednesday 10:00 UTC -- outside the 02:00-06:00 window.
	outsideWindow := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return outsideWindow })

	result := r.now()
	assert.Equal(t, outsideWindow, result)
	assert.False(t, isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now()),
		"10:00 should be outside 02:00-06:00 window")

	// Wednesday 03:00 UTC -- inside the window.
	insideWindow := time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return insideWindow })
	assert.True(t, isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now()),
		"03:00 should be inside 02:00-06:00 window")
}

func TestIsWithinResizeWindow_NoSchedule(t *testing.T) {
	assert.True(t, isWithinResizeWindow(nil, time.Now()))
}

func TestIsWithinResizeWindow_DayOfWeek(t *testing.T) {
	// Wednesday 10:00 UTC
	wed := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)
	schedule := &attunev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
	}
	assert.True(t, isWithinResizeWindow(schedule, wed))

	// Thursday should be blocked
	thu := time.Date(2026, 1, 8, 10, 0, 0, 0, time.UTC)
	assert.False(t, isWithinResizeWindow(schedule, thu))
}

func TestIsWithinResizeWindow_TimeWindow(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows: []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
	}
	// 03:00 is inside
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_OvernightWindow(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows: []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
	}
	// 23:00 is inside (after start)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 23, 0, 0, 0, time.UTC)))
	// 03:00 is inside (before end, wraps past midnight)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_OvernightWindowWithDayOfWeek(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
		DaysOfWeek: []string{"Wednesday"},
	}
	// Wed 23:00: pre-midnight portion, today is Wednesday -> allowed
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 23, 0, 0, 0, time.UTC)))
	// Thu 03:00: post-midnight portion, window opened on Wednesday -> allowed
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 8, 3, 0, 0, 0, time.UTC)))
	// Thu 23:00: pre-midnight portion, today is Thursday (not in list) -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 8, 23, 0, 0, 0, time.UTC)))
	// Fri 03:00: post-midnight portion, window would have opened Thu (not in list) -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 9, 3, 0, 0, 0, time.UTC)))
	// Wed 10:00: outside the window entirely -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_InvalidTimezoneFailsOpen(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Timezone: "Invalid/Zone",
	}
	// Invalid timezone should fail open (allow resize)
	assert.True(t, isWithinResizeWindow(schedule, time.Now()))
}

func TestParseHHMM(t *testing.T) {
	assert.Equal(t, 120, parseHHMM("02:00"))
	assert.Equal(t, 1380, parseHHMM("23:00"))
	assert.Equal(t, 0, parseHHMM("00:00"))
	assert.Equal(t, -1, parseHHMM("25:00"))
	assert.Equal(t, -1, parseHHMM("bad"))
}

func TestProgressPercent(t *testing.T) {
	tests := []struct {
		name                      string
		collected, required, want int
	}{
		{"zero required returns zero", 5, 0, 0},
		{"negative required returns zero", 5, -1, 0},
		{"partial progress", 50, 100, 50},
		{"exactly at required clamps to 99", 100, 100, 99},
		{"over required clamps to 99", 200, 100, 99},
		{"zero collected", 0, 100, 0},
		{"one sample", 1, 100, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, progressPercent(tt.collected, tt.required))
		})
	}
}

func TestParseFloat64NonNeg(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		fallback float64
		want     float64
	}{
		{"empty returns fallback", "", 0.5, 0.5},
		{"valid value", "0.7", 0.5, 0.7},
		{"zero", "0", 0.5, 0.0},
		{"exactly one", "1.0", 0.5, 1.0},
		{"capped above one", "1.5", 0.5, 1.0},
		{"negative returns fallback", "-0.3", 0.5, 0.5},
		{"parse error returns fallback", "abc", 0.5, 0.5},
		{"NaN returns fallback", "NaN", 0.5, 0.5},
		{"Inf returns fallback", "Inf", 0.5, 0.5},
		{"-Inf returns fallback", "-Inf", 0.5, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFloat64NonNeg(tt.input, tt.fallback)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestTryEvictionFallback_EvictionDeniedByPDB(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	// Make eviction fail (simulates PDB denial).
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "eviction" {
			return true, nil, fmt.Errorf("Cannot evict pod as it would violate the pod's disruption budget")
		}
		return false, nil, nil
	})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should return false when eviction is denied by PDB")
}

func TestTryEvictionFallback_ListErrorSkipsEviction(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	// Use an interceptor to make List fail, simulating API server unreachable.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("connection refused")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should skip eviction when pod list fails")

	// Verify no eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			t.Error("eviction should not be attempted when List fails")
		}
	}
}

func TestTryEvictionFallback_NilSelectorSkipsEviction(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	// Create a deployment with nil Selector to exercise the nil-guard.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: nil,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
				},
			},
		},
	}
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "main", resizer)
	assert.False(t, evicted, "should skip eviction when workload has nil selector")

	// Verify no eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			t.Error("eviction should not be attempted when selector is nil")
		}
	}
}

func TestExportRecommendationConfigMaps_CreatesConfigMap(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.95,
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("250m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
				},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-policy-my-app-recommendations",
	}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "my-app", cm.Data["workload"])
	assert.Equal(t, "Deployment", cm.Data["kind"])
	assert.Equal(t, "250m", cm.Data["main.cpu-request"])
	assert.Equal(t, "256Mi", cm.Data["main.memory-request"])
	assert.Equal(t, "0.95", cm.Data["main.confidence"])
	assert.Equal(t, "test-policy", cm.Labels["attune.io/policy"])
}

func TestExportRecommendationConfigMaps_UpdatesExisting(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-my-app-recommendations",
			Namespace: "default",
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.99,
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-policy-my-app-recommendations",
	}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "500m", cm.Data["main.cpu-request"])
	assert.Equal(t, "0.99", cm.Data["main.confidence"])
}

func TestExportRecommendationConfigMaps_CreateFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return fmt.Errorf("simulated create failure")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.95, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("250m"), MemoryRequest: resource.MustParse("256Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "ConfigMap should not exist after create failure")
}

func TestExportRecommendationConfigMaps_GetFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("simulated API server error")
				}
				return nil
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.90, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("128Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)
}

func TestExportRecommendationConfigMaps_UpdateFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy-my-app-recommendations", Namespace: "default"},
		Data:       map[string]string{"old-key": "old-value"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("simulated patch failure")
				}
				return nil
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.85, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("300m"), MemoryRequest: resource.MustParse("384Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// The existing ConfigMap should still have old data since the update failed.
	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "old-value", cm.Data["old-key"])
}

func TestExportRecommendationConfigMaps_PreservesExistingLabels(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-policy-my-app-recommendations", Namespace: "default",
			Labels: map[string]string{"custom-label": "keep-me"},
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.92, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("400m"), MemoryRequest: resource.MustParse("512Mi"),
			}},
		}},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "400m", cm.Data["main.cpu-request"], "data should be updated")
	assert.Equal(t, "keep-me", cm.Labels["custom-label"], "existing labels should be preserved")
	assert.Equal(t, "test-policy", cm.Labels["attune.io/policy"], "operator labels should be set")
}

func TestExportRecommendationConfigMaps_OrphanCleanup(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}

	// Pre-create ConfigMaps for two workloads under this policy
	activeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-my-app-recommendations",
			Namespace: "default",
			Labels: map[string]string{
				"attune.io/policy":   "test-policy",
				"attune.io/workload": "my-app",
			},
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	orphanCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-old-workload-recommendations",
			Namespace: "default",
			Labels: map[string]string{
				"attune.io/policy":   "test-policy",
				"attune.io/workload": "old-workload",
			},
		},
		Data: map[string]string{"main.cpu-request": "50m"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, activeCM, orphanCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Confidence: 0.9, Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("200m"),
					MemoryRequest: resource.MustParse("256Mi"),
				}},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// Active workload ConfigMap should still exist and be updated
	var active corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &active)
	require.NoError(t, err)
	assert.Equal(t, "200m", active.Data["main.cpu-request"])

	// Orphaned workload ConfigMap should have been deleted
	var orphan corev1.ConfigMap
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-old-workload-recommendations"}, &orphan)
	assert.True(t, apierrors.IsNotFound(err), "orphaned ConfigMap should be deleted")
}

func TestExportRecommendationConfigMaps_SkipsLongName(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Build a workload name that makes the ConfigMap name exceed 253 chars.
	// Format: "test-policy-<workload>-recommendations" = 12 + len(workload) + 16 = 28 + len(workload)
	// Need 28 + len(workload) > 253, so len(workload) >= 226
	longWorkload := strings.Repeat("x", 226)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: longWorkload,
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Confidence: 0.9, Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("100m"),
					MemoryRequest: resource.MustParse("128Mi"),
				}},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// ConfigMap should NOT have been created because the name exceeds 253 chars.
	cmName := fmt.Sprintf("test-policy-%s-recommendations", longWorkload)
	assert.Greater(t, len(cmName), 253)
	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      cmName,
	}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "ConfigMap with name >253 chars should not be created")
}

func TestAdjustHPATargets_ScalesTargetUtilization(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// CPU went from 200m to 400m, so target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])
}

func TestAdjustHPATargets_PreservesThirdPartyAnnotations(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)

	// The stale HPA from the initial List has only our annotation.
	staleHPA := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}

	// The "fresh" HPA in the cluster has a third-party annotation added
	// by ArgoCD between the initial List and the re-fetch.
	freshHPA := staleHPA.DeepCopy()
	freshHPA.Annotations["argocd.argoproj.io/managed-by"] = "argo-controller"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(freshHPA).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{staleHPA},
		"my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)

	// Our annotations should be set.
	assert.Equal(t, "true", hpa.Annotations[annotationHPAAutoTune])
	assert.Equal(t, "80", hpa.Annotations[annotationHPAOriginalCPU])

	// Third-party annotation must survive (the bug was that the stale
	// copy's annotations overwrote the fresh copy, dropping this).
	assert.Equal(t, "argo-controller", hpa.Annotations["argocd.argoproj.io/managed-by"],
		"third-party annotations must not be overwritten by stale HPA copy")
}

func TestApplyStartupBoosts_AppliesBoostToNewPod(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)), // 30s old
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"),
					},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify resize was attempted via clientset actions and memory request preserved.
	actions := clientset.Actions()
	var foundResize bool
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			reqs := updatedPod.Spec.Containers[0].Resources.Requests
			assert.True(t, reqs.Cpu().Cmp(resource.MustParse("100m")) > 0, "CPU should be boosted above 100m")
			memReq := reqs[corev1.ResourceMemory]
			assert.Equal(t, resource.MustParse("128Mi"), memReq, "memory request should be preserved")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for startup boost")
}

func TestApplyStartupBoosts_NaNMultiplierSkipped(t *testing.T) {
	// NaN multiplier must be treated as invalid and skip the boost entirely.
	// Before the fix, NaN <= 1 evaluated to false, so NaN passed the guard.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "NaN",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// NaN multiplier should be skipped -- no resize action should occur.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("expected no resize action for NaN multiplier")
		}
	}
}

func TestApplyStartupBoosts_SkipsPodOutsideWindow(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-old",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)), // 5 min old, outside 2m window
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify no resize action was taken.
	actions := clientset.Actions()
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Error("should not resize pod outside boost window")
		}
	}
}

func TestAdjustHPATargets_IdempotentOnSecondCall(t *testing.T) {
	scheme := testScheme()
	origTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &origTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// First call: 200m -> 400m, target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	// Re-fetch the HPA to get updated state.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	require.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)

	// Second call with same args (e.g., canary promote). Target should stay 40.
	updatedHPAs := []autoscalingv2.HorizontalPodAutoscaler{hpa}
	r.adjustHPATargets(context.Background(), updatedHPAs, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	err = fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	// Should be 40 (idempotent), not 20 (double-adjusted).
	assert.Equal(t, int32(40), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestApplyStartupBoosts_ExpiresBoostAfterDuration(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC) // 5 minutes after boost
	boostTime := now.Add(-3 * time.Minute)             // boosted 3 min ago
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute}, // 2 min duration, expired
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-xyz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(boostTime),
			Annotations: map[string]string{
				annotationStartupBoostAt: boostTime.UTC().Format(time.RFC3339), // boost was applied
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"), // boosted
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify a resize action was taken (reducing back to steady-state).
	actions := clientset.Actions()
	var foundResize bool
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for boost expiry")
}

func TestApplyStartupBoosts_MalformedAnnotationSkipsGracefully(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	// Pod has a malformed boost timestamp annotation.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-xyz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
			Annotations: map[string]string{
				annotationStartupBoostAt: "not-a-valid-timestamp",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	// Should not panic and should not attempt any resize.
	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	actions := clientset.Actions()
	for _, a := range actions {
		assert.NotEqual(t, "resize", a.GetSubresource(),
			"no resize should be attempted for a pod with malformed boost annotation")
	}
}

func TestApplyStartupBoosts_SkipsWhenExceedsNodeAllocatable(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	// Node with only 1 CPU allocatable.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	// Pod consuming 500m, boost multiplier 3x = 1500m which exceeds node's 1 CPU.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-app-1", Namespace: "default",
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Second)),
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod)
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("500m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify no resize action was taken (boost would exceed node allocatable).
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("expected no resize action when boost exceeds node allocatable")
		}
	}
}

func TestApplyStartupBoosts_CapsAtCPULimit(t *testing.T) {
	// When the boosted CPU (3x 200m = 600m) exceeds the container's CPU limit
	// (500m), the boost should be capped at the limit to avoid API rejection
	// from requests > limits.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "limited-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "limited-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:        "main",
					Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"limited-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify resize was attempted and CPU was capped at the limit (500m).
	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpuReq := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			cpuLim := resource.MustParse("500m")
			assert.True(t, cpuReq.Cmp(cpuLim) <= 0,
				"boosted CPU request (%s) should not exceed limit (500m)", cpuReq.String())
			assert.True(t, cpuReq.Cmp(resource.MustParse("100m")) > 0,
				"CPU should be boosted above the original 100m")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for capped startup boost")
}

func TestApplyStartupBoosts_ExpiryKeepsAnnotationOnFailure(t *testing.T) {
	// When steady-state resize fails during boost expiry, the boost annotation
	// should be kept so the next reconciliation retries. Without this,
	// a transient failure leaves the pod permanently at boosted CPU.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	boostAt := now.Add(-3 * time.Minute) // boost applied 3min ago, duration 2min => expired
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boost-expire-pod",
			Namespace: "default",
			Annotations: map[string]string{
				annotationStartupBoostAt: boostAt.UTC().Format(time.RFC3339),
			},
			Labels:            map[string]string{labelTracked: "true"},
			CreationTimestamp: metav1.NewTime(boostAt.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"), // boosted
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	// Make UpdateResize fail to simulate transient error.
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("simulated resize failure")
		}
		return false, nil, nil
	})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "boost-expire",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	podsByWorkload := map[string][]corev1.Pod{"boost-expire": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify the boost annotation is still present (not removed after failure).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "boost-expire-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations[annotationStartupBoostAt]
	assert.True(t, has, "boost annotation should be kept when resize fails so next reconcile retries")
}

func TestStartupBoost_SkippedInObserveMode(t *testing.T) {
	// Verify that the reconcile-level guard prevents startup boosts when
	// the policy mode is Observe or Recommend.
	for _, mode := range []attunev1alpha1.UpdateType{
		attunev1alpha1.UpdateTypeObserve,
		attunev1alpha1.UpdateTypeRecommend,
	} {
		t.Run(string(mode), func(t *testing.T) {
			assert.False(t, isResizeMode(mode),
				"mode %s must not be a resize mode (startup boosts should be skipped)", mode)
		})
	}
	// Positive check: Auto, OneShot, and Canary are resize modes.
	for _, mode := range []attunev1alpha1.UpdateType{
		attunev1alpha1.UpdateTypeAuto,
		attunev1alpha1.UpdateTypeOneShot,
		attunev1alpha1.UpdateTypeCanary,
	} {
		t.Run(string(mode), func(t *testing.T) {
			assert.True(t, isResizeMode(mode),
				"mode %s must be a resize mode (startup boosts should fire)", mode)
		})
	}
}

func TestAdjustHPATargets_SkipsWithoutAnnotation(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-app-hpa",
				Namespace: "default",
				// No auto-tune annotation.
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpas[0]).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	// Target should be unchanged since no annotation.
	assert.Equal(t, int32(80), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_GetErrorDoesNotCrash(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	// HPA in the slice but NOT registered with the fake client, so Get returns NotFound.
	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ghost-hpa",
				Namespace: "default",
				Annotations: map[string]string{
					annotationHPAAutoTune: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: &oldTarget,
							},
						},
					},
				},
			},
		},
	}

	// Empty client: HPA does not exist, Get will fail.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Should not panic; logs the Get error and moves on.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})
}

func TestAdjustHPATargets_UpdateErrorPreservesOriginal(t *testing.T) {
	scheme := testScheme()
	oldTarget := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &oldTarget,
						},
					},
				},
			},
		},
	}

	// Inject an Update error.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return fmt.Errorf("simulated conflict")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Should not panic; logs the Update error.
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"), resource.Quantity{})

	// The stored HPA should still have the original target since update failed.
	var storedHPA autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "conflict-hpa",
	}, &storedHPA)
	require.NoError(t, err)
	assert.Equal(t, int32(80), *storedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_ClampsAbove100(t *testing.T) {
	// When CPU request decreases dramatically, the computed target can exceed
	// 100%. Verify it is clamped to 100.
	scheme := testScheme()
	target := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clamp-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=1000m, new=100m -> 80 * 1000/100 = 800, clamped to 100 (Guaranteed QoS: no limit)
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("1000m"), resource.MustParse("100m"), resource.Quantity{})

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "clamp-hpa"}, &stored))
	assert.Equal(t, int32(100), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_ClampsBelow1(t *testing.T) {
	// When CPU request increases dramatically, the computed target can drop
	// below 1. Verify it is clamped to 1.
	scheme := testScheme()
	target := int32(50)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clamp-low-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=10m, new=10000m -> 50 * 10/10000 = 0.05, int32 = 0, clamped to 1
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("10m"), resource.MustParse("10000m"), resource.Quantity{})

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "clamp-low-hpa"}, &stored))
	assert.Equal(t, int32(1), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestAdjustHPATargets_BurstableAllowsAbove100(t *testing.T) {
	// Burstable QoS: limit > request. The computed target can exceed 100%
	// and should be capped at floor(limit/request*100) instead of 100.
	scheme := testScheme()
	target := int32(70)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burstable-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=500m, new=300m, limit=1000m -> 70 * 500/300 = 116
	// Burstable cap: floor(1000/300 * 100) = 333
	// 116 < 333, so newTarget = 116 (not clamped to 100)
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("500m"), resource.MustParse("300m"), resource.MustParse("1000m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "burstable-hpa"}, &stored))
	assert.Equal(t, int32(116), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Burstable pod should allow target above 100")
}

func TestAdjustHPATargets_BurstableCapsAtLimitRatio(t *testing.T) {
	// Burstable QoS: verify the target is capped at floor(limit/request*100)
	// when the computed target exceeds the limit ratio.
	scheme := testScheme()
	target := int32(80)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burstable-cap-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=1000m, new=100m, limit=200m -> 80 * 1000/100 = 800
	// Burstable cap: floor(200/100 * 100) = 200
	// 800 > 200, so newTarget = 200
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("1000m"), resource.MustParse("100m"), resource.MustParse("200m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "burstable-cap-hpa"}, &stored))
	assert.Equal(t, int32(200), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Burstable target should be capped at floor(limit/request*100)")
}

func TestAdjustHPATargets_GuaranteedCapsAt100(t *testing.T) {
	// Guaranteed QoS: limit == request. Target should be capped at 100
	// even though cpuLimit is set (not zero).
	scheme := testScheme()
	target := int32(70)
	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guaranteed-hpa",
			Namespace: "default",
			Annotations: map[string]string{
				annotationHPAAutoTune: "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-app",
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &target,
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// old=500m, new=300m, limit=300m (Guaranteed: limit == request)
	// 70 * 500/300 = 116
	// Guaranteed cap: floor(300/300 * 100) = 100
	// 116 > 100, so newTarget = 100
	r.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"my-app", "Deployment",
		resource.MustParse("500m"), resource.MustParse("300m"), resource.MustParse("300m"))

	var stored autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "guaranteed-hpa"}, &stored))
	assert.Equal(t, int32(100), *stored.Spec.Metrics[0].Resource.Target.AverageUtilization,
		"Guaranteed pod (limit==request) should cap at 100")
}

func TestBuildRecommendationEngines_NilMaxChangePercent(t *testing.T) {
	// Exercise the defense-in-depth nil fallback: when CPU.MaxChangePercent
	// and Memory.MaxChangePercent are nil (bypassing applyBuiltInDefaults),
	// the function should fall back to DefaultCPUMaxChangePercent and
	// DefaultMemoryMaxChangePercent.
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.CPU.MaxChangePercent = nil
	policy.Spec.Memory.MaxChangePercent = nil

	cpuEngine, memEngine := buildRecommendationEngines(policy)

	// Use RecommendWithExplanation to inspect the maxChangePercent embedded
	// in each engine via the explanation struct.
	cpuProfile := rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P50: 500, P95: 800, Max: 1000},
		DataPoints:         100,
		Confidence:         1.0,
	}
	_, cpuExpl, _ := cpuEngine.RecommendWithExplanation(cpuProfile, resource.MustParse("500m"))
	assert.Equal(t, float64(attunev1alpha1.DefaultCPUMaxChangePercent), cpuExpl.MaxChangePercent)

	memProfile := rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P50: 256, P95: 512, Max: 1024},
		DataPoints:         100,
		Confidence:         1.0,
	}
	_, memExpl, _ := memEngine.RecommendWithExplanation(memProfile, resource.MustParse("256Mi"))
	assert.Equal(t, float64(attunev1alpha1.DefaultMemoryMaxChangePercent), memExpl.MaxChangePercent)
}

func TestBuildRecommendationEngines_ExplicitMaxChangePercent(t *testing.T) {
	// When CPU.MaxChangePercent and Memory.MaxChangePercent are set explicitly,
	// the engine should use those values instead of the defaults.
	policy := &attunev1alpha1.AttunePolicy{}
	cpuPct := int32(75)
	memPct := int32(40)
	policy.Spec.CPU.MaxChangePercent = &cpuPct
	policy.Spec.Memory.MaxChangePercent = &memPct

	cpuEngine, memEngine := buildRecommendationEngines(policy)

	cpuProfile := rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P50: 500, P95: 800, Max: 1000},
		DataPoints:         100,
		Confidence:         1.0,
	}
	_, cpuExpl, _ := cpuEngine.RecommendWithExplanation(cpuProfile, resource.MustParse("500m"))
	assert.Equal(t, float64(75), cpuExpl.MaxChangePercent)

	memProfile := rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P50: 256, P95: 512, Max: 1024},
		DataPoints:         100,
		Confidence:         1.0,
	}
	_, memExpl, _ := memEngine.RecommendWithExplanation(memProfile, resource.MustParse("256Mi"))
	assert.Equal(t, float64(40), memExpl.MaxChangePercent)
}

func TestResolveChangeCaps(t *testing.T) {
	tests := []struct {
		name           string
		rc             attunev1alpha1.ResourceConfig
		builtInDefault int32
		wantIncrease   float64
		wantDecrease   float64
	}{
		{
			name:           "all nil uses built-in default",
			rc:             attunev1alpha1.ResourceConfig{},
			builtInDefault: 50,
			wantIncrease:   50,
			wantDecrease:   50,
		},
		{
			name: "MaxChangePercent overrides default for both",
			rc: attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(30),
			},
			builtInDefault: 50,
			wantIncrease:   30,
			wantDecrease:   30,
		},
		{
			name: "MaxIncreasePercent overrides increase only",
			rc: attunev1alpha1.ResourceConfig{
				MaxChangePercent:   int32Ptr(30),
				MaxIncreasePercent: int32Ptr(80),
			},
			builtInDefault: 50,
			wantIncrease:   80,
			wantDecrease:   30,
		},
		{
			name: "MaxDecreasePercent overrides decrease only",
			rc: attunev1alpha1.ResourceConfig{
				MaxChangePercent:   int32Ptr(30),
				MaxDecreasePercent: int32Ptr(15),
			},
			builtInDefault: 50,
			wantIncrease:   30,
			wantDecrease:   15,
		},
		{
			name: "all three set uses directional overrides",
			rc: attunev1alpha1.ResourceConfig{
				MaxChangePercent:   int32Ptr(30),
				MaxIncreasePercent: int32Ptr(90),
				MaxDecreasePercent: int32Ptr(10),
			},
			builtInDefault: 50,
			wantIncrease:   90,
			wantDecrease:   10,
		},
		{
			name: "clamps below 1 to 1",
			rc: attunev1alpha1.ResourceConfig{
				MaxIncreasePercent: int32Ptr(0),
				MaxDecreasePercent: int32Ptr(-5),
			},
			builtInDefault: 50,
			wantIncrease:   1,
			wantDecrease:   1,
		},
		{
			name: "clamps above 100 to 100",
			rc: attunev1alpha1.ResourceConfig{
				MaxIncreasePercent: int32Ptr(200),
				MaxDecreasePercent: int32Ptr(150),
			},
			builtInDefault: 50,
			wantIncrease:   100,
			wantDecrease:   100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIncrease, gotDecrease := resolveChangeCaps(tt.rc, tt.builtInDefault)
			assert.Equal(t, tt.wantIncrease, gotIncrease, "increase")
			assert.Equal(t, tt.wantDecrease, gotDecrease, "decrease")
		})
	}
}

func TestShouldSkipResize_LimitRangeViolation(t *testing.T) {
	scheme := testScheme()
	// LimitRange requiring at least 100m CPU.
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lr", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("50m"), // below LimitRange min
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "quota/limitrange violation")
	assert.Contains(t, reason, "below LimitRange minimum")
}

func TestShouldSkipResize_QuotaHeadroomExceeded(t *testing.T) {
	scheme := testScheme()
	// ResourceQuota with only 300m CPU headroom (hard=1000m, used=700m).
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test-quota", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1000m")},
			Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("700m")},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("900m"), // increase of 700m, only 300m headroom
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("900m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "quota/limitrange violation")
	assert.Contains(t, reason, "would exceed ResourceQuota")
}

func TestShouldSkipResize_NodeAllocatableExceeded(t *testing.T) {
	scheme := testScheme()
	// Node with 2000m CPU and 4Gi memory allocatable.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2000m"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
				{Name: "sidecar", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("1600m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
	}
	// Total after resize: app=1600m + sidecar=500m = 2100m > node 2000m.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1600m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "exceed node allocatable")
}

func TestShouldSkipResize_NodeAllocatableNotExceeded(t *testing.T) {
	scheme := testScheme()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("1000m"),
			MemoryRequest: resource.MustParse("2Gi"),
		},
	}
	// Total after resize: 1000m < 4000m, 2Gi < 8Gi. Should not skip.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}

	skip, _ := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.False(t, skip)
}

func TestShouldSkipResize_AlreadyAtTarget(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target matches current pod resources exactly.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.True(t, skip, "should skip when pod already matches target")
	assert.Empty(t, reason, "reason should be empty for already-at-target skip")
}

func TestShouldSkipResize_PreChecksLimitRange(t *testing.T) {
	scheme := testScheme()
	// No objects in the client; LimitRange is passed via pre-fetched checks.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	checks := &resizePreChecks{
		limitRanges: []corev1.LimitRange{
			{Spec: corev1.LimitRangeSpec{
				Limits: []corev1.LimitRangeItem{
					{Type: corev1.LimitTypeContainer, Min: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					}},
				},
			}},
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, checks)
	assert.True(t, skip, "should skip when target violates pre-fetched LimitRange")
	assert.Contains(t, reason, "quota/limitrange violation")
}

func TestShouldSkipResize_NodeCacheHit(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target exceeds node allocatable.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5000m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Pre-populate the node cache.
	checks := &resizePreChecks{}
	cachedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	checks.nodeCache.Store("test-node", cachedNode)

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, checks)
	assert.True(t, skip, "should skip when target exceeds cached node allocatable")
	assert.Contains(t, reason, "exceed node allocatable")
}

func TestShouldSkipResize_NodeCacheMiss(t *testing.T) {
	scheme := testScheme()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2000m"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target within node allocatable.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
	// Empty cache; node should be fetched and stored.
	checks := &resizePreChecks{}

	skip, _ := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, checks)
	assert.False(t, skip, "should not skip when target fits in node allocatable")

	// Verify the node was cached.
	cached, ok := checks.nodeCache.Load("test-node")
	assert.True(t, ok, "node should be cached after miss")
	assert.NotNil(t, cached, "cached node should not be nil")
}

func TestShouldSkipResize_QoSClassChange(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}
	// Guaranteed pod: requests == limits for all resources.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSGuaranteed,
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target changes only requests without matching limits, breaking Guaranteed QoS.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, nil)
	assert.True(t, skip, "should skip when resize would change QoS class")
	assert.Contains(t, reason, "QoS class")
}

func TestFindContainerByName_RegularContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	c := findContainerByName(pod, "sidecar")
	require.NotNil(t, c)
	assert.Equal(t, "sidecar", c.Name)
}

func TestFindContainerByName_InitContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init-setup"},
			},
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	c := findContainerByName(pod, "init-setup")
	require.NotNil(t, c)
	assert.Equal(t, "init-setup", c.Name)
}

func TestFindContainerByName_NotFound(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init-setup"},
			},
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	assert.Nil(t, findContainerByName(pod, "missing"))
}

func TestFindContainerByName_InitShadowsRegular(t *testing.T) {
	// If a name exists in both init and regular containers, init wins
	// because it is searched first.
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "shared-name", Image: "init-image"},
			},
			Containers: []corev1.Container{
				{Name: "shared-name", Image: "regular-image"},
			},
		},
	}
	c := findContainerByName(pod, "shared-name")
	require.NotNil(t, c)
	assert.Equal(t, "init-image", c.Image, "init container should be returned when names collide")
}

func TestSpecOrDeletePredicate_Update(t *testing.T) {
	now := metav1.Now()
	p := specOrDeletePredicate{}

	tests := []struct {
		name     string
		oldGen   int64
		newGen   int64
		oldDel   *metav1.Time
		newDel   *metav1.Time
		oldAnnot map[string]string
		newAnnot map[string]string
		want     bool
	}{
		{
			name:   "spec change (generation bump) triggers reconcile",
			oldGen: 1, newGen: 2,
			want: true,
		},
		{
			name:   "status-only update (same generation) filtered out",
			oldGen: 1, newGen: 1,
			want: false,
		},
		{
			name:   "deletion timestamp set triggers reconcile",
			oldGen: 1, newGen: 1,
			newDel: &now,
			want:   true,
		},
		{
			name:   "already deleting (both have timestamp) filtered out",
			oldGen: 1, newGen: 1,
			oldDel: &now, newDel: &now,
			want: false,
		},
		{
			name:   "annotation-only change (same generation) filtered out",
			oldGen: 1, newGen: 1,
			oldAnnot: map[string]string{"attune.io/last-resize-time": "2024-01-01T00:00:00Z"},
			newAnnot: map[string]string{"attune.io/last-resize-time": "2024-01-01T01:00:00Z"},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := &attunev1alpha1.AttunePolicy{}
			old.SetGeneration(tt.oldGen)
			if tt.oldDel != nil {
				old.SetDeletionTimestamp(tt.oldDel)
			}
			if tt.oldAnnot != nil {
				old.SetAnnotations(tt.oldAnnot)
			}
			new := &attunev1alpha1.AttunePolicy{}
			new.SetGeneration(tt.newGen)
			if tt.newDel != nil {
				new.SetDeletionTimestamp(tt.newDel)
			}
			if tt.newAnnot != nil {
				new.SetAnnotations(tt.newAnnot)
			}
			got := p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: new})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSpecOrDeletePredicate_NilObjects(t *testing.T) {
	p := specOrDeletePredicate{}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: &attunev1alpha1.AttunePolicy{}}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &attunev1alpha1.AttunePolicy{}, ObjectNew: nil}))
}

func TestSetReadyCondition(t *testing.T) {
	tests := []struct {
		name              string
		workloadCount     int
		workloadsWithRecs int32
		totalQueryErrors  int
		queryErrorTypes   map[string]struct{}
		maxDataPoints     int
		promTimedOut      bool
		promTimeout       time.Duration
		wantStatus        metav1.ConditionStatus
		wantReason        string
		wantMsgContains   string
	}{
		{
			name:              "ready with recommendations",
			workloadCount:     3,
			workloadsWithRecs: 2,
			queryErrorTypes:   map[string]struct{}{},
			wantStatus:        metav1.ConditionTrue,
			wantReason:        attunev1alpha1.ReasonMonitoring,
			wantMsgContains:   "Watching 3 workloads, 2 with recommendations",
		},
		{
			name:              "ready with recommendations and CPU query errors",
			workloadCount:     5,
			workloadsWithRecs: 3,
			totalQueryErrors:  2,
			queryErrorTypes:   map[string]struct{}{"CPU": {}},
			wantStatus:        metav1.ConditionTrue,
			wantReason:        attunev1alpha1.ReasonMonitoring,
			wantMsgContains:   "Prometheus query errors (2) prevented CPU data collection",
		},
		{
			name:              "ready with recommendations and both CPU and memory errors",
			workloadCount:     4,
			workloadsWithRecs: 1,
			totalQueryErrors:  5,
			queryErrorTypes:   map[string]struct{}{"CPU": {}, "memory": {}},
			wantStatus:        metav1.ConditionTrue,
			wantReason:        attunev1alpha1.ReasonMonitoring,
			wantMsgContains:   "CPU and memory data collection",
		},
		{
			name:              "ready with recommendations and prometheus timeout",
			workloadCount:     10,
			workloadsWithRecs: 5,
			queryErrorTypes:   map[string]struct{}{},
			promTimedOut:      true,
			wantStatus:        metav1.ConditionTrue,
			wantReason:        attunev1alpha1.ReasonMonitoring,
			wantMsgContains:   "Prometheus query timeout exceeded",
		},
		{
			name:              "not ready collecting data",
			workloadCount:     2,
			workloadsWithRecs: 0,
			queryErrorTypes:   map[string]struct{}{},
			maxDataPoints:     10,
			wantStatus:        metav1.ConditionFalse,
			wantReason:        attunev1alpha1.ReasonInsufficientData,
			wantMsgContains:   "Collecting data: 10/48 data points",
		},
		{
			name:              "not ready with memory query errors",
			workloadCount:     1,
			workloadsWithRecs: 0,
			totalQueryErrors:  1,
			queryErrorTypes:   map[string]struct{}{"memory": {}},
			maxDataPoints:     0,
			wantStatus:        metav1.ConditionFalse,
			wantReason:        attunev1alpha1.ReasonPrometheusUnavailable,
			wantMsgContains:   "Prometheus query errors (1) prevented memory data collection",
		},
		{
			name:              "not ready max data points exceeds minimum clamps remaining to 0",
			workloadCount:     1,
			workloadsWithRecs: 0,
			queryErrorTypes:   map[string]struct{}{},
			maxDataPoints:     100,
			wantStatus:        metav1.ConditionFalse,
			wantReason:        attunev1alpha1.ReasonInsufficientData,
			wantMsgContains:   "100/48 data points (99%)",
		},
		{
			name:              "not ready prometheus timeout with no recommendations",
			workloadCount:     5,
			workloadsWithRecs: 0,
			queryErrorTypes:   map[string]struct{}{},
			promTimedOut:      true,
			promTimeout:       5 * time.Minute,
			wantStatus:        metav1.ConditionFalse,
			wantReason:        attunev1alpha1.ReasonPrometheusUnavailable,
			wantMsgContains:   "Prometheus query timeout exceeded after 5m0s",
		},
		{
			name:              "ready with timeout and query errors combined",
			workloadCount:     10,
			workloadsWithRecs: 3,
			totalQueryErrors:  4,
			queryErrorTypes:   map[string]struct{}{"CPU": {}},
			promTimedOut:      true,
			promTimeout:       5 * time.Minute,
			wantStatus:        metav1.ConditionTrue,
			wantReason:        attunev1alpha1.ReasonMonitoring,
			wantMsgContains:   "Prometheus query timeout exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewAttunePolicyReconciler()
			r.PrometheusTimeout = tt.promTimeout
			policy := &attunev1alpha1.AttunePolicy{}
			policy.Generation = 5

			r.setReadyCondition(policy, tt.workloadCount, tt.workloadsWithRecs,
				tt.totalQueryErrors, tt.queryErrorTypes, tt.maxDataPoints, tt.promTimedOut, tt.promTimeout)

			cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
			require.NotNil(t, cond, "Ready condition must be set")
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
			assert.Contains(t, cond.Message, tt.wantMsgContains)
			assert.Equal(t, int64(5), cond.ObservedGeneration)
		})
	}
}

func TestProcessWorkloads_Parallel(t *testing.T) {
	// Track peak concurrent queries to prove parallelism.
	var inflight atomic.Int32
	var peakInflight atomic.Int32

	// Build 60 samples (enough to exceed the default minimumDataPoints of 48).
	now := time.Now()
	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}

	collector := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			cur := inflight.Add(1)
			// Track the peak concurrency.
			for {
				peak := peakInflight.Load()
				if cur <= peak || peakInflight.CompareAndSwap(peak, cur) {
					break
				}
			}
			// Simulate query latency to allow goroutines to overlap.
			time.Sleep(5 * time.Millisecond)
			inflight.Add(-1)
			return grouped, nil
		},
	}

	// Create 20 deployments to process in parallel.
	const numWorkloads = 20
	objs := make([]runtime.Object, 0, numWorkloads)
	workloads := make([]client.Object, 0, numWorkloads)
	for i := range numWorkloads {
		name := fmt.Sprintf("deploy-%d", i)
		dep := newTestDeployment(name, "default", map[string]string{"app": name})
		objs = append(objs, dep)
		workloads = append(workloads, dep)
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })

	result := r.processWorkloads(context.Background(), policy, workloads, collector, nil)

	// All 20 workloads should produce recommendations.
	assert.Equal(t, int32(numWorkloads), result.workloadsWithRecs,
		"all workloads should have recommendations")
	assert.Len(t, result.recommendations, numWorkloads)
	assert.Equal(t, 0, result.totalQueryErrors)

	// Verify actual concurrency occurred (peak > 1 proves parallelism).
	assert.Greater(t, peakInflight.Load(), int32(1),
		"expected concurrent queries (peak inflight > 1)")
}

func TestProcessWorkloads_ParallelPartialFailure(t *testing.T) {
	// Verify that partial query failures don't corrupt results under
	// concurrent access. Even-numbered workloads return errors; odd succeed.
	var inflight atomic.Int32
	var peakInflight atomic.Int32

	now := time.Now()
	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}

	// Track which workloads should fail based on query content.
	// Each workload's pod regex contains its name, so we can match on it.
	collector := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			cur := inflight.Add(1)
			for {
				peak := peakInflight.Load()
				if cur <= peak || peakInflight.CompareAndSwap(peak, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inflight.Add(-1)

			// Fail queries for even-numbered workloads.
			for i := 0; i < 20; i += 2 {
				if strings.Contains(query, fmt.Sprintf("deploy-%d-", i)) {
					return nil, fmt.Errorf("prometheus timeout for %d", i)
				}
			}
			return grouped, nil
		},
	}

	const numWorkloads = 20
	objs := make([]runtime.Object, 0, numWorkloads)
	workloads := make([]client.Object, 0, numWorkloads)
	for i := range numWorkloads {
		name := fmt.Sprintf("deploy-%d", i)
		dep := newTestDeployment(name, "default", map[string]string{"app": name})
		objs = append(objs, dep)
		workloads = append(workloads, dep)
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })

	result := r.processWorkloads(context.Background(), policy, workloads, collector, nil)

	// Odd-numbered workloads (10 of 20) should succeed.
	assert.Equal(t, int32(10), result.workloadsWithRecs,
		"only odd-numbered workloads should have recommendations")
	assert.Len(t, result.recommendations, 10)
	// Even-numbered workloads fail both CPU and memory queries.
	assert.Greater(t, result.totalQueryErrors, 0, "should have query errors")
	// Verify parallelism still occurred despite failures.
	assert.Greater(t, peakInflight.Load(), int32(1),
		"expected concurrent queries even with partial failures")
}

func TestReconcile_InsufficientDataRequeuesAtQueryStep(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	// Use a long cooldown to make the difference obvious.
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil // 20 samples, below 48 threshold
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// With InsufficientData, requeue should be the query step (5m),
	// not the cooldown (2h).
	assert.Equal(t, attunev1alpha1.DefaultQueryStep, result.RequeueAfter,
		"InsufficientData should requeue at queryStep interval, not cooldown")
}

func TestReconcile_SufficientDataRequeuesAtCooldown(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil // 200 samples, above 48 threshold
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	assert.NoError(t, err)
	// With sufficient data, requeue should be the full cooldown.
	assert.Equal(t, 2*time.Hour, result.RequeueAfter,
		"sufficient data should requeue at cooldown interval")
}

// --- Issue #437: persistResizeAnnotations exhausted-retries path ---

func TestExecuteResizes_AnnotationConflictExhaustedRetries(t *testing.T) {
	// When all 3 annotation persist retries are exhausted due to conflicts,
	// the resize should be reverted with reason "annotation-persist-conflict".
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Set conflictsLeft to 3 (matches maxRetries in persistResizeAnnotations),
	// so all retry attempts fail with 409 Conflict.
	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 3}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// The resize should have been reverted because all retries were exhausted.
	assert.Equal(t, 0, count, "net resized count should be 0 after conflict exhaustion revert")

	// History should show Reverted entries with annotation-persist-conflict reason.
	require.NotEmpty(t, history)
	var foundReverted bool
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted && h.Reason == "annotation-persist-conflict" {
			foundReverted = true
			break
		}
	}
	assert.True(t, foundReverted, "history should contain a Reverted entry with reason annotation-persist-conflict")

	// Verify that a Reverted event was emitted mentioning annotation-persist-conflict.
	var foundRevertEvent bool
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "annotation-persist-conflict") {
				foundRevertEvent = true
			}
		default:
			goto done437
		}
	}
done437:
	assert.True(t, foundRevertEvent, "expected a Reverted event mentioning annotation-persist-conflict")

	// All 3 conflict attempts should have been seen.
	assert.Equal(t, 3, wrappedClient.conflictsSeen, "should have exhausted all 3 retries")

	// Verify that a revert was issued via UpdateResize (2 calls: original + revert).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original resize + revert")
}

// --- Issue #441: annotation cleanup conflict retry in checkPendingSafetyObservations ---

func TestCheckPendingSafetyObservations_AnnotationCleanupConflictRetry(t *testing.T) {
	// Verify that annotation cleanup retries on 409 Conflict and succeeds.
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// First annotation cleanup Update returns 409 Conflict, second succeeds.
	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 1}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 5 * time.Minute}

	workloads := []client.Object{deploy}
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, workloads)

	assert.False(t, pending, "should not be pending after successful cleanup")
	assert.Equal(t, 1, wrappedClient.conflictsSeen, "should have retried once on conflict")

	// Verify annotations were removed.
	var updated corev1.Pod
	err := wrappedClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations[annotationResizedAt]
	assert.False(t, hasResizedAt, "tracking annotations should be removed after cleanup")
}

func TestCheckPendingSafetyObservations_AnnotationCleanupConflictExhausted(t *testing.T) {
	// Verify that exhausting all cleanup retries logs the error but doesn't crash.
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// All 3 cleanup attempts return 409 Conflict.
	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 3}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 5 * time.Minute}

	workloads := []client.Object{deploy}
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, workloads)

	// Should not crash and should not report pending (annotation cleanup is
	// best-effort; the annotations will be cleaned on the next reconcile).
	assert.False(t, pending)
	assert.Equal(t, 3, wrappedClient.conflictsSeen, "should have exhausted all 3 retries")

	// Annotations should still be present since all updates failed.
	var updated corev1.Pod
	err := wrappedClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations[annotationResizedAt]
	assert.True(t, hasResizedAt, "tracking annotations should still be present after exhausted retries")
}

// --- Issue #440: startup boost expiry memory regression test ---

func TestApplyStartupBoosts_ExpiryPassesShouldSkipResizeWithMemory(t *testing.T) {
	// Regression test: startup boost expiry must populate memory values in
	// the skip-check target so that LimitRange/node allocatable checks
	// don't fail due to zero-valued memory fields.
	boostApplied := time.Now().Add(-10 * time.Minute) // expired
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-abc-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "api-server"},
			Annotations: map[string]string{
				annotationStartupBoostAt: boostApplied.UTC().Format(time.RFC3339),
				annotationPolicy:         "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"), // boosted
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	// LimitRange requires minimum memory of 64Mi.
	limitRange := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lr", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod, node, limitRange).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	dur := metav1.Duration{Duration: 5 * time.Minute}
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   dur,
	}

	// Steady-state recommendation: 500m CPU (half the boosted value).
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"),
					},
				},
			},
		},
	}

	resizer := resize.NewPodResizer(clientset, logr.Discard())
	checks := reconciler.buildResizePreChecks(context.Background(), policy)

	reconciler.applyStartupBoosts(context.Background(), policy, podMap("api-server", pod), recommendations, resizer, checks)

	// Verify that UpdateResize was called (boost expired, resize to steady-state).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.GreaterOrEqual(t, resizeCalls, 1,
		"boost expiry should call UpdateResize to reduce CPU to steady-state")

	// Verify the boost annotation was removed (expiry completed successfully).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated)
	require.NoError(t, err)
	_, hasBoostAt := updated.Annotations[annotationStartupBoostAt]
	assert.False(t, hasBoostAt, "startup boost annotation should be removed after expiry")
}

func TestWarnConfigClamping(t *testing.T) {
	tests := []struct {
		name      string
		policy    *attunev1alpha1.AttunePolicy
		wantEvent string // substring expected in the event, "" means no event
	}{
		{
			name: "historyWindow below minimum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 30 * time.Minute}
				return p
			}(),
			wantEvent: "historyWindow 30m0s clamped to 1h",
		},
		{
			name: "historyWindow above maximum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 800 * time.Hour}
				return p
			}(),
			wantEvent: "historyWindow 800h0m0s clamped to 720h",
		},
		{
			name: "queryStep below minimum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 5 * time.Second}
				return p
			}(),
			wantEvent: "queryStep 5s clamped to 10s",
		},
		{
			name: "queryStep above maximum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 2 * time.Hour}
				return p
			}(),
			wantEvent: "queryStep 2h0m0s clamped to 1h",
		},
		{
			name: "rateWindow below minimum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.RateWindow = &metav1.Duration{Duration: 10 * time.Second}
				return p
			}(),
			wantEvent: "rateWindow 10s clamped to 30s",
		},
		{
			name: "cooldown below operator minimum emits event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 30 * time.Second}
				return p
			}(),
			wantEvent: "cooldown 30s raised to operator minimum",
		},
		{
			name: "valid config emits no event",
			policy: func() *attunev1alpha1.AttunePolicy {
				p := newTestPolicy("test", "default")
				p.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 24 * time.Hour}
				p.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: time.Minute}
				p.Spec.MetricsSource.RateWindow = &metav1.Duration{Duration: 2 * time.Minute}
				p.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 5 * time.Minute}
				return p
			}(),
			wantEvent: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newReconcilerWithClient()
			recorder := events.NewFakeRecorder(10)
			reconciler.Recorder = recorder

			reconciler.warnConfigClamping(tt.policy)

			if tt.wantEvent != "" {
				select {
				case event := <-recorder.Events:
					assert.Contains(t, event, "ConfigClamped")
					assert.Contains(t, event, tt.wantEvent)
				default:
					t.Fatalf("expected event containing %q but channel was empty", tt.wantEvent)
				}
			} else {
				select {
				case event := <-recorder.Events:
					t.Fatalf("expected no event but got: %s", event)
				default:
					// Good: no event emitted.
				}
			}
		})
	}
}

func TestApplyStartupBoosts_CapsAtMaxAllowed(t *testing.T) {
	// When the boosted CPU (3x 200m = 600m) exceeds the policy's
	// maxAllowed (400m), the boost should be capped at maxAllowed
	// to respect admin-configured ceilings.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	maxAllowed := resource.MustParse("400m")
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				MaxAllowed: &maxAllowed,
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "maxallowed-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "maxallowed-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:        "main",
					Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"maxallowed-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpuReq := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, cpuReq.Cmp(maxAllowed) <= 0,
				"boosted CPU (%s) should not exceed maxAllowed (400m)", cpuReq.String())
			assert.True(t, cpuReq.Cmp(resource.MustParse("100m")) > 0,
				"CPU should be boosted above original 100m")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for maxAllowed-capped startup boost")
}

func TestExportRecommendationConfigMaps_NaNInfConfidenceWritesZero(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	tests := []struct {
		name       string
		confidence float64
	}{
		{"NaN", math.NaN()},
		{"positive Inf", math.Inf(1)},
		{"negative Inf", math.Inf(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := []attunev1alpha1.WorkloadRecommendation{
				{
					Workload: "my-app",
					Kind:     "Deployment",
					Containers: []attunev1alpha1.ContainerRecommendation{
						{
							Name:       "main",
							Confidence: tt.confidence,
							Recommended: attunev1alpha1.ResourceValues{
								CPURequest:    resource.MustParse("250m"),
								MemoryRequest: resource.MustParse("256Mi"),
							},
						},
					},
				},
			}

			r.exportRecommendationConfigMaps(context.Background(), policy, recs)

			var cm corev1.ConfigMap
			err := fakeClient.Get(context.Background(), client.ObjectKey{
				Namespace: "default",
				Name:      "test-policy-my-app-recommendations",
			}, &cm)
			require.NoError(t, err)
			assert.Equal(t, "0.00", cm.Data["main.confidence"],
				"%s confidence should be written as 0.00", tt.name)

			// Clean up for next subtest.
			_ = fakeClient.Delete(context.Background(), &cm)
		})
	}
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
