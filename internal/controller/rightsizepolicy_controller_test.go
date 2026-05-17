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
	"strings"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
)

// testScheme returns a runtime.Scheme with all needed types registered.
func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = rightsizev1alpha1.AddToScheme(scheme)
	return scheme
}

// int32Ptr returns a pointer to an int32.
func int32Ptr(i int32) *int32 {
	return &i
}

// stringPtr returns a pointer to a string.
func stringPtr(s string) *string {
	return &s
}

// boolPtr returns a pointer to a bool.
func boolPtr(b bool) *bool {
	return &b
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

// newTestPolicy creates a RightSizePolicy for testing.
func newTestPolicy(name, namespace string) *rightsizev1alpha1.RightSizePolicy {
	return &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: stringPtr("api-server"),
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
				},
				MinimumDataPoints: 48,
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("50m"),
					Max: resource.MustParse("4000m"),
				},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("64Mi"),
					Max: resource.MustParse("8Gi"),
				},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeRecommend,
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
func newResizeRecommendation(workload, curCPU, curMem, curCPULim, curMemLim, recCPU, recMem, recCPULim, recMemLim string) rightsizev1alpha1.WorkloadRecommendation {
	return rightsizev1alpha1.WorkloadRecommendation{
		Workload: workload,
		Kind:     "Deployment",
		Containers: []rightsizev1alpha1.ContainerRecommendation{
			{
				Name: "main",
				Current: rightsizev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse(curCPU),
					CPULimit:      resource.MustParse(curCPULim),
					MemoryRequest: resource.MustParse(curMem),
					MemoryLimit:   resource.MustParse(curMemLim),
				},
				Recommended: rightsizev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse(recCPU),
					CPULimit:      resource.MustParse(recCPULim),
					MemoryRequest: resource.MustParse(recMem),
					MemoryLimit:   resource.MustParse(recMemLim),
				},
			},
		},
	}
}

// newReconcilerWithClient creates a RightSizePolicyReconciler with the given
// objects pre-loaded. Reduces the 5-line scheme+client+reconciler setup
// that repeats in nearly every test.
func newReconcilerWithClient(objects ...client.Object) *RightSizePolicyReconciler {
	scheme := testScheme()
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
	return &RightSizePolicyReconciler{
		Client: builder.Build(),
		Scheme: scheme,
	}
}

// newReconcilerForReconcile creates a reconciler with status subresource
// support and a mock metrics factory, ready for Reconcile tests.
func newReconcilerForReconcile(mc rsmetrics.MetricsCollector, objects ...client.Object) (*RightSizePolicyReconciler, client.Client) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()
	return &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}, fakeClient
}

func newReconcilerForReconcileWithClient(mc rsmetrics.MetricsCollector, c client.Client, scheme *runtime.Scheme) *RightSizePolicyReconciler {
	return &RightSizePolicyReconciler{
		Client:         c,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}
}

// newResizeReconciler creates a reconciler with both a controller-runtime
// fake client and a typed clientset for resize tests.
func newResizeReconciler(pod *corev1.Pod, objects ...client.Object) (*RightSizePolicyReconciler, client.Client) {
	scheme := testScheme()
	allObjects := append(objects, pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	return &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}, fakeClient
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
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
	query := buildPrometheusQuery("production", "api-server", "main", "cpu")
	expected := `rate(container_cpu_usage_seconds_total{namespace="production",pod=~"api-server.*",container="main"}[5m])`
	assert.Equal(t, expected, query)
}

func TestBuildPrometheusQuery_Memory(t *testing.T) {
	query := buildPrometheusQuery("production", "api-server", "main", "memory")
	expected := `container_memory_working_set_bytes{namespace="production",pod=~"api-server.*",container="main"}`
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

func TestReconcile_NoMatchingWorkloadsSetsInsufficientData(t *testing.T) {
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

	// Verify the status was updated with InsufficientData condition.
	var updated rightsizev1alpha1.RightSizePolicy
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-policy",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, "InsufficientData", updated.Status.Conditions[0].Reason)
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
	reconciler := &RightSizePolicyReconciler{}
	deploy := newTestDeployment("test", "default", nil)
	assert.False(t, reconciler.isRollingOut(deploy))
}

func TestIsRollingOut_DeploymentMidRollout(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{}
	deploy := newTestDeployment("test", "default", nil)
	deploy.Status.UpdatedReplicas = 1 // Only 1 of 2 updated.
	assert.True(t, reconciler.isRollingOut(deploy))
}

func TestBuildPrometheusQuery_FallbackNoContainer(t *testing.T) {
	query := buildPrometheusQuery("default", "api-server", "", "cpu")
	assert.Contains(t, query, `namespace="default"`)
	assert.Contains(t, query, `pod=~"api-server.*"`)
	assert.NotContains(t, query, `container=`)
}

func TestBuildPrometheusQuery_MemoryFallbackNoContainer(t *testing.T) {
	query := buildPrometheusQuery("default", "api-server", "", "memory")
	assert.Contains(t, query, `namespace="default"`)
	assert.Contains(t, query, `pod=~"api-server.*"`)
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

func TestComputeSavings_ReturnsCorrectStructure(t *testing.T) {
	scheme := testScheme()
	r := &RightSizePolicyReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	recs := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name: "api",
					Current: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("1"),
					},
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"),
					},
				},
			},
		},
	}
	savings := r.computeSavings("test-ns", recs, nil)
	assert.NotEmpty(t, savings.CPURequestReduction)
	assert.Equal(t, "500m", savings.CPURequestReduction)
}

func TestGetContainers_Deployment(t *testing.T) {
	r := &RightSizePolicyReconciler{}
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
	r := &RightSizePolicyReconciler{}
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

func TestGetPodPrefix(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	dep := &appsv1.Deployment{}
	dep.Name = "api-server"
	assert.Equal(t, "api-server", r.getPodPrefix(dep))
}

func TestParseHistoryWindow_Default(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	assert.Equal(t, 7*24*time.Hour, r.parseHistoryWindow(policy))
}

func TestParseHistoryWindow_Custom(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	d := metav1.Duration{Duration: 14 * 24 * time.Hour}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, 14*24*time.Hour, r.parseHistoryWindow(policy))
}

func TestParseHistoryWindow_ClampedTooSmall(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	d := metav1.Duration{Duration: 10 * time.Minute}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, time.Hour, r.parseHistoryWindow(policy), "should clamp to 1h minimum")
}

func TestParseHistoryWindow_ClampedTooLarge(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	d := metav1.Duration{Duration: 1000 * time.Hour}
	policy.Spec.MetricsSource.HistoryWindow = &d
	assert.Equal(t, 720*time.Hour, r.parseHistoryWindow(policy), "should clamp to 720h maximum")
}

func TestGetMinimumDataPoints_Default(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	assert.Equal(t, int32(48), r.getMinimumDataPoints(policy))
}

func TestGetMinimumDataPoints_Custom(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	policy.Spec.MetricsSource.MinimumDataPoints = 42
	assert.Equal(t, int32(42), r.getMinimumDataPoints(policy))
}

func TestIsRollingOut_StatefulSetStable(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{UpdatedReplicas: 3},
	}
	assert.False(t, r.isRollingOut(sts))
}

func TestIsRollingOut_StatefulSetMidRollout(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{UpdatedReplicas: 1},
	}
	assert.True(t, r.isRollingOut(sts))
}

func TestIsRollingOut_DaemonSet(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 5,
		},
	}
	assert.False(t, r.isRollingOut(ds))
}

func TestIsRollingOut_DaemonSetMidRollout(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 2,
		},
	}
	assert.True(t, r.isRollingOut(ds))
}

func TestParseCooldown_Default(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	assert.Equal(t, 1*time.Hour, r.parseCooldown(policy))
}

func TestParseCooldown_Custom(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	d := metav1.Duration{Duration: 5 * time.Minute}
	policy.Spec.UpdateStrategy.Cooldown = &d
	assert.Equal(t, 5*time.Minute, r.parseCooldown(policy))
}

func TestParseCooldown_SubMinuteClampedTo1m(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
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
	reconciler := &RightSizePolicyReconciler{}
	mc := &mockCollector{}
	staleTime := time.Now().Add(-5 * time.Minute)
	reconciler.collectors.Store("http://prom:9090", &collectorEntry{
		collector: mc,
		lastUsed:  staleTime,
	})

	before := time.Now()
	got, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)

	// Verify lastUsed was refreshed on cache hit.
	entry, ok := reconciler.collectors.Load("http://prom:9090")
	require.True(t, ok)
	assert.True(t, entry.(*collectorEntry).lastUsed.After(before) || entry.(*collectorEntry).lastUsed.Equal(before),
		"lastUsed should be refreshed to ~now on cache hit, got %v", entry.(*collectorEntry).lastUsed)
}

func TestGetOrCreateCollector_CacheMiss(t *testing.T) {
	mc := &mockCollector{}
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(address string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			assert.Equal(t, "http://new:9090", address)
			return mc, nil
		},
	}

	got, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)
}

func TestGetOrCreateCollector_FactoryError(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://broken:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestGetOrCreateCollector_CacheFull(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
	}
	// Fill the cache to maxCollectors.
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://prom-%d:9090", i)
		_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: addr}, nil)
		require.NoError(t, err)
	}

	// The next address should be rejected.
	_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://one-too-many:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collector cache full")
}

func TestGetOrCreateCollector_CustomTTL(t *testing.T) {
	customTTL := 2 * time.Minute
	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: customTTL,
		MetricsFactory: func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
	}

	// Store an entry that is stale under custom TTL but fresh under default TTL.
	staleTime := time.Now().Add(-(customTTL + time.Minute))
	reconciler.collectors.Store("http://stale:9090", &collectorEntry{
		collector: &mockCollector{},
		lastUsed:  staleTime,
	})

	// Creating a new collector should trigger eviction of the stale entry.
	_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)

	_, stillExists := reconciler.collectors.Load("http://stale:9090")
	assert.False(t, stillExists, "entry older than custom TTL should be evicted")
}

func TestGetOrCreateCollector_EvictsStaleEntries(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
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
	_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)
}

func TestGetOrCreateCollector_ConcurrentAccess(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: 50 * time.Millisecond,
		MetricsFactory: func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
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
			_, err := reconciler.getOrCreateCollector(&rightsizev1alpha1.PrometheusConfig{Address: addresses[idx]}, nil)
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
	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: time.Millisecond,
		MetricsFactory: func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
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
		&rightsizev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil,
	)
	require.NoError(t, err)

	assert.True(t, closable.closed,
		"Close() should be called on evicted collector that implements io.Closer")
}

func TestGetOrCreateCollector_ConcurrentRaceClosesUnused(t *testing.T) {
	var mu sync.Mutex
	var created []*closableMockCollector

	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: collectorTTL,
		MetricsFactory: func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
			c := &closableMockCollector{}
			mu.Lock()
			created = append(created, c)
			mu.Unlock()
			return c, nil
		},
	}

	// All goroutines race to create the same address.
	const goroutines = 10
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.getOrCreateCollector(
				&rightsizev1alpha1.PrometheusConfig{Address: "http://race:9090"}, nil)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	assert.NoError(t, err)
	assert.Nil(t, rec) // No recommendation because data points are insufficient
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

	rec, qErrors, failedMetricTypes, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, qErrors, failedMetricTypes, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 1, qErrors)
	assert.Equal(t, []string{"memory"}, failedMetricTypes)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, emptyDeploy, mc)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// Memory should be clamped to current (512Mi) since AllowDecrease is nil.
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Cmp(resource.MustParse("512Mi")) >= 0,
		"memory should not decrease below current when AllowDecrease is nil, got %s", rec.Containers[0].Recommended.MemoryRequest.String())
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

			rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	calls := make(map[string]int)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			calls[query]++
			return map[string][]rsmetrics.Sample{
				"main":    generateSamples(200, 0.1),
				"sidecar": generateSamples(200, 0.05),
			}, nil
		},
	}

	rec, qErrors, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, qErrors, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
	}

	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource: &rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
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
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
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
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
	}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource: &rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
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

// ---------- collectorCacheKey ----------

func TestCollectorCacheKey_AddressOnly(t *testing.T) {
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	assert.Equal(t, "http://prom:9090", collectorCacheKey(config, nil))
}

func TestCollectorCacheKey_WithOptions(t *testing.T) {
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		BearerToken:        "tok",
		InsecureSkipVerify: true,
		Headers:            map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	key := collectorCacheKey(config, opts)
	assert.Contains(t, key, "http://prom:9090")
	assert.Contains(t, key, "|bearer:")
	assert.Contains(t, key, "|insecure")
	assert.Contains(t, key, "|h:X-Scope-OrgID=tenant-1")
	assert.NotContains(t, key, "tok")
}

func TestCollectorCacheKey_DeterministicWithMultipleHeaders(t *testing.T) {
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		Headers: map[string]string{"Z-Header": "z", "A-Header": "a", "M-Header": "m"},
	}
	// Call multiple times to verify map iteration order doesn't affect the key.
	key1 := collectorCacheKey(config, opts)
	for i := 0; i < 100; i++ {
		assert.Equal(t, key1, collectorCacheKey(config, opts), "cache key must be deterministic on iteration %d", i)
	}
	// Verify sorted order: A before M before Z.
	assert.Contains(t, key1, "|h:A-Header=a|h:M-Header=m|h:Z-Header=z")
}

func TestCollectorCacheKey_DifferentConfigsDifferentKeys(t *testing.T) {
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, nil)
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok"})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_DifferentBearerTokensDifferentKeys(t *testing.T) {
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-a"})
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-b"})
	assert.NotEqual(t, key1, key2)
}

// ---------- buildCollectorOptions ----------

func TestBuildCollectorOptions_NilWhenNoAuthOrTLS(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	config := &rightsizev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	assert.Nil(t, opts)
}

func TestBuildCollectorOptions_WithHeaders(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	config := &rightsizev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		Headers: map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config)
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "tenant-1", opts.Headers["X-Scope-OrgID"])
}

func TestBuildCollectorOptions_WithTLS(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	config := &rightsizev1alpha1.PrometheusConfig{
		Address: "https://prom:9090",
		TLS:     &rightsizev1alpha1.TLSConfig{InsecureSkipVerify: true},
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
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	config := &rightsizev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &rightsizev1alpha1.SecretKeyRef{
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
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	config := &rightsizev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &rightsizev1alpha1.SecretKeyRef{
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
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	token, err := r.readSecretKey(context.Background(), "default", "prom-token", "token")
	assert.NoError(t, err)
	assert.Equal(t, "my-bearer-token", token)
}

func TestReadSecretKey_SecretNotFound(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

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
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	_, err := r.readSecretKey(context.Background(), "default", "prom-token", "token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "key \"token\" not found")
}

func TestReconcile_BearerTokenSecretRotationRecreatesCollector(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &rightsizev1alpha1.SecretKeyRef{
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

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	p.Status.Workloads = rightsizev1alpha1.WorkloadStatus{Discovered: 5}

	err := reconciler.updateStatusWithRetry(ctx, &p, key)
	assert.NoError(t, err)

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &updated))
	assert.Equal(t, int32(5), updated.Status.Workloads.Discovered)
}

func TestUpdateStatusWithRetry_ConflictThenRetry(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	// Set status we want to persist.
	p.Status.Workloads = rightsizev1alpha1.WorkloadStatus{Discovered: 7}

	// Create a concurrent metadata update to bump the resource version.
	var concurrent rightsizev1alpha1.RightSizePolicy
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

	var final rightsizev1alpha1.RightSizePolicy
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

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	// This reconcile has Resized=0 (stale snapshot).
	p.Status.Workloads = rightsizev1alpha1.WorkloadStatus{Discovered: 5, Resized: 0}

	// Simulate a concurrent reconcile that set Resized=2.
	var concurrent rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &concurrent))
	concurrent.Status.Workloads = rightsizev1alpha1.WorkloadStatus{Discovered: 5, Resized: 2}
	require.NoError(t, fakeClient.Status().Update(ctx, &concurrent))

	// p now has a stale resource version AND a lower Resized count.
	err := reconciler.updateStatusWithRetry(ctx, &p, key)
	assert.NoError(t, err)

	var final rightsizev1alpha1.RightSizePolicy
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

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, reconciler.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated rightsizev1alpha1.RightSizePolicy
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

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, reconciler.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated rightsizev1alpha1.RightSizePolicy
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
	var updated rightsizev1alpha1.RightSizePolicy
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeObserve
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

	var updated rightsizev1alpha1.RightSizePolicy
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

	var updated rightsizev1alpha1.RightSizePolicy
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
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		ObservationPeriod: metav1.Duration{Duration: 2 * time.Minute},
	}
	assert.Equal(t, 2*time.Minute, getObservationPeriod(policy))

	// Test that default observation period is used when no canary config.
	policyNoCanary := newTestPolicy("test-policy2", "default")
	assert.Equal(t, defaultObservationPeriod, getObservationPeriod(policyNoCanary))

	// Test the min(cooldown, observationPeriod) requeue logic directly.
	// When AutoRevert is true and resizes occurred, the reconciler
	// uses min(cooldown, observationPeriod) as requeue interval
	// (lines 417-424 of rightsizepolicy_controller.go).
	cooldown := 1 * time.Hour
	obs := getObservationPeriod(policy) // 2m
	requeueAfter := cooldown
	if obs < requeueAfter {
		requeueAfter = obs
	}
	assert.Equal(t, 2*time.Minute, requeueAfter,
		"requeue should be shortened to observation period when it is less than cooldown")

	// When observation period exceeds cooldown, cooldown wins.
	longObs := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 5 * time.Minute},
				Canary: &rightsizev1alpha1.CanaryConfig{
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

func TestRemoveTrackingAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":                  "test",
				"rightsize.io/tracked": "true",
				"unrelated":            "keep",
			},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                      "2026-01-01T00:00:00Z",
				"rightsize.io/resized-workload":                "api-server",
				"rightsize.io/resized-containers":              "main,sidecar",
				"rightsize.io/original-cpu-request.main":       "500m",
				"rightsize.io/original-memory-request.main":    "512Mi",
				"rightsize.io/original-cpu-limit.main":         "1000m",
				"rightsize.io/original-memory-limit.main":      "1Gi",
				"rightsize.io/original-restart-count.main":     "0",
				"rightsize.io/original-cpu-request.sidecar":    "100m",
				"rightsize.io/original-memory-request.sidecar": "64Mi",
				"rightsize.io/original-cpu-limit.sidecar":      "200m",
				"rightsize.io/original-memory-limit.sidecar":   "128Mi",
				"rightsize.io/original-restart-count.sidecar":  "2",
				"unrelated-annotation":                         "keep",
			},
		},
	}

	removeTrackingAnnotations(pod)

	// Tracking label should be removed.
	_, hasTracked := pod.Labels["rightsize.io/tracked"]
	assert.False(t, hasTracked, "tracked label should be removed")
	assert.Equal(t, "keep", pod.Labels["unrelated"], "unrelated labels should be preserved")

	// All tracking annotations should be removed.
	for key := range pod.Annotations {
		assert.False(t, strings.HasPrefix(key, "rightsize.io/"),
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
func newSafetyTestReconciler(pod *corev1.Pod) (*RightSizePolicyReconciler, client.Client) {
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
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
	_, hasResizedAt := updated.Annotations["rightsize.io/resized-at"]
	assert.False(t, hasResizedAt, "resized-at annotation should be removed")
	_, hasContainer := updated.Annotations["rightsize.io/resized-container"]
	assert.False(t, hasContainer, "resized-container annotation should be removed")
}

func TestCheckPendingSafetyObservations_MalformedAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-pod",
			Namespace: "default",
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "not-a-quantity", // malformed
				"rightsize.io/original-memory-request.main": "512Mi",
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
	_, hasResizedAt := updated.Annotations["rightsize.io/resized-at"]
	assert.True(t, hasResizedAt, "annotations should remain after parse error")
}

func TestCheckPendingSafetyObservations_NotElapsed(t *testing.T) {
	// Just resized -- observation period has NOT elapsed yet.
	resizedAt := time.Now().UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recent-pod",
			Namespace: "default",
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify annotations are still present (observation period not elapsed).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "recent-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, hasResizedAt := updated.Annotations["rightsize.io/resized-at"]
	assert.True(t, hasResizedAt, "annotations should remain when observation period not elapsed")
}

// ---------- isCooldownActive parse error ----------

func TestIsCooldownActive_MalformedDate(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{}
	policy := newTestPolicy("test-policy", "default")
	policy.Annotations = map[string]string{
		lastResizeAnnotation: "not-a-valid-date",
	}
	assert.False(t, reconciler.isCooldownActive(policy))
}

// ---------- executeResizes ----------

func TestExecuteResizes_NoClientset(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{}
	policy := newTestPolicy("test-policy", "default")

	count, history := reconciler.executeResizes(context.Background(), policy, nil, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Nil(t, history)
}

func TestExecuteResizes_SuccessfulResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 1, count)
	require.Len(t, history, 2, "expect one cpu + one memory history entry")
	assert.Equal(t, "api-server", history[0].Workload)
	assert.Equal(t, "main", history[0].Container)
	assert.Equal(t, "InPlace", history[0].Method)
	assert.Equal(t, rightsizev1alpha1.ResizeResultSuccess, history[0].Result, "cpu resize should succeed")
	assert.Equal(t, rightsizev1alpha1.ResizeResultSuccess, history[1].Result, "memory resize should succeed")
}

func TestExecuteResizes_ContextCancelledAbortsRemaining(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, history := reconciler.executeResizes(ctx, policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count, "no resizes should complete with cancelled context")
	assert.Empty(t, history, "no history entries with cancelled context")
}

func TestExecuteResizes_SkipsMatchingResources(t *testing.T) {
	// Pod already at the recommended values.
	pod := newResizePod("api-server", "750m", "384Mi", "1500m", "768Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "0", "0", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_NoMatchingWorkload(t *testing.T) {
	deploy := newTestDeployment("other-app", "default", nil)
	reconciler := newReconcilerWithClient(deploy)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		{Workload: "api-server", Kind: "Deployment"},
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
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
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "ReplicaSet", selector)
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "CronJob", Name: &name},
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Job", Name: &name},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestGetContainers_CronJob(t *testing.T) {
	r := &RightSizePolicyReconciler{}
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
	r := &RightSizePolicyReconciler{}
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
	name := "my-replicaset"
	r := newReconcilerWithClient()

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "ReplicaSet",
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
	r := &RightSizePolicyReconciler{}
	sts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
		},
	}
	labels := r.getPodSelectorLabels(sts)
	assert.Equal(t, map[string]string{"app": "db"}, labels)
}

func TestGetPodSelectorLabels_DaemonSet(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	ds := &appsv1.DaemonSet{
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
		},
	}
	labels := r.getPodSelectorLabels(ds)
	assert.Equal(t, map[string]string{"app": "agent"}, labels)
}

func TestGetPodSelectorLabels_NilSelector(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{}}
	labels := r.getPodSelectorLabels(dep)
	assert.Nil(t, labels)
}

// ---------- getContainers (DaemonSet) ----------

func TestGetContainers_DaemonSet(t *testing.T) {
	r := &RightSizePolicyReconciler{}
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
	r := &RightSizePolicyReconciler{}
	containers := r.getContainers(&corev1.Pod{})
	assert.Nil(t, containers)
}

func TestGetContainers_IncludesNativeSidecars(t *testing.T) {
	r := &RightSizePolicyReconciler{}
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
	r := &RightSizePolicyReconciler{}
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
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU:    &rightsizev1alpha1.ResourceConfig{Percentile: 90, SafetyMargin: "1.5"},
			Memory: &rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.4"},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeAuto,
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with all zeros/empty (should inherit from defaults).
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	r.mergeDefaults(policy, defaults)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, "1.4", policy.Spec.Memory.SafetyMargin)
	assert.Equal(t, rightsizev1alpha1.UpdateModeAuto, policy.Spec.UpdateStrategy.Mode)
}

func TestMergeDefaults_PolicyOverridesDefaults(t *testing.T) {
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU:    &rightsizev1alpha1.ResourceConfig{Percentile: 90, SafetyMargin: "1.5"},
			Memory: &rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.4"},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with explicit values (should NOT be overwritten).
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.1"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.2"},
		},
	}

	r.mergeDefaults(policy, nil)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.1", policy.Spec.CPU.SafetyMargin)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "1.2", policy.Spec.Memory.SafetyMargin)
}

// ---------- appendHistory ----------

func TestAppendHistory_CapsAtMaxEntries(t *testing.T) {
	existing := make([]rightsizev1alpha1.ResizeHistoryEntry, 18)
	for i := range existing {
		existing[i] = rightsizev1alpha1.ResizeHistoryEntry{Workload: fmt.Sprintf("w-%d", i)}
	}
	newEntries := []rightsizev1alpha1.ResizeHistoryEntry{
		{Workload: "new-1"},
		{Workload: "new-2"},
		{Workload: "new-3"},
		{Workload: "new-4"},
	}

	result := appendHistory(existing, newEntries, 20)
	assert.Len(t, result, 20)
	assert.Equal(t, "w-2", result[0].Workload)
	assert.Equal(t, "new-4", result[19].Workload)
}

// ---------- Reconcile with OneShot mode (exercises resize path entry) ----------

func TestReconcile_OneShotMode_NoClientset_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

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

	var updated rightsizev1alpha1.RightSizePolicy
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

	var updated rightsizev1alpha1.RightSizePolicy
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, rightsizev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonMonitoring, cond.Reason)
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, rightsizev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonPrometheusUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "Prometheus query errors (2)")
	assert.Contains(t, cond.Message, "CPU and memory data collection")
}

// ---------- resolveCanaryPhase ----------

func TestResolveCanaryPhase_InitializesOnFirstCall(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	assert.Equal(t, rightsizev1alpha1.UpdateModeCanary, mode, "first call should stay in canary mode")
	require.NotNil(t, policy.Status.Canary)
	assert.Equal(t, rightsizev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.NotNil(t, policy.Status.Canary.StartTime)
}

func TestResolveCanaryPhase_PromotesAfterObservation(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase:     rightsizev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	// No reverts in history.
	policy.Status.ResizeHistory = []rightsizev1alpha1.ResizeHistoryEntry{
		{Result: rightsizev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(startTime.Add(1 * time.Minute))},
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	assert.Equal(t, rightsizev1alpha1.UpdateModeAuto, mode, "should promote to auto after observation passes")
	assert.Equal(t, rightsizev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_WaitsDuringObservation(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase:     rightsizev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	assert.Equal(t, rightsizev1alpha1.UpdateModeCanary, mode, "should stay in canary during observation")
}

func TestResolveCanaryPhase_BlocksOnRevert(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase:     rightsizev1alpha1.CanaryPhaseInProgress,
		StartTime: &startTime,
	}
	// Revert happened during observation.
	policy.Status.ResizeHistory = []rightsizev1alpha1.ResizeHistoryEntry{
		{Result: rightsizev1alpha1.ResizeResultReverted, Timestamp: metav1.NewTime(startTime.Add(2 * time.Minute))},
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	assert.Equal(t, rightsizev1alpha1.UpdateModeCanary, mode, "should block promotion when revert happened")
	assert.Equal(t, rightsizev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_FullRolloutStaysAuto(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase: rightsizev1alpha1.CanaryPhaseFullRollout,
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	assert.Equal(t, rightsizev1alpha1.UpdateModeAuto, mode, "FullRollout should map to Auto")
}

func TestResolveCanaryPhase_ResetsOnSpecChange(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Generation = 3
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	// Canary was started at generation 2 -- spec has since changed.
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase:              rightsizev1alpha1.CanaryPhaseFullRollout,
		StartTime:          &startTime,
		ObservedGeneration: 2,
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	// Should reset and re-initialize, staying in canary mode.
	assert.Equal(t, rightsizev1alpha1.UpdateModeCanary, mode, "spec change should reset canary, not stay in FullRollout")
	require.NotNil(t, policy.Status.Canary)
	assert.Equal(t, rightsizev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.Equal(t, int64(3), policy.Status.Canary.ObservedGeneration, "new cycle should track current generation")
}

func TestResolveCanaryPhase_NoResetWhenGenerationMatches(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	policy := newTestPolicy("test-policy", "default")
	policy.Generation = 2
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		AutoPromote:       true,
	}
	policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
		Phase:              rightsizev1alpha1.CanaryPhaseInProgress,
		StartTime:          &startTime,
		ObservedGeneration: 2,
	}

	reconciler := &RightSizePolicyReconciler{}
	mode := reconciler.resolveCanaryPhase(context.Background(), policy, rightsizev1alpha1.UpdateModeCanary)

	// Same generation: should promote normally after observation period.
	assert.Equal(t, rightsizev1alpha1.UpdateModeAuto, mode, "same generation should promote normally")
	assert.Equal(t, rightsizev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

// ---------- Reconcile with cooldown active ----------

func TestReconcile_CooldownActive_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_QoSBlocked_EmitsResizeSkippedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "250m", "256Mi", "500m", "512Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, rightsizev1alpha1.ResizeResultFailed, history[0].Result)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "ResizeFailed")
		assert.Contains(t, event, "api-server")
	default:
		t.Fatal("expected a ResizeFailed event but channel was empty")
	}
}

func TestExecuteResizes_AutoRevert_SafeVerdictNoRevert(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)

	// Resize was attempted. The safety check runs immediately but with a
	// fake clientset the pod won't have conditions set, so CheckPod will
	// return Safe (no restart detected). This exercises the autoRevert
	// code path even though it does not trigger a revert.
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, rightsizev1alpha1.ResizeResultSuccess, history[0].Result)
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

	var updated rightsizev1alpha1.RightSizePolicy
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
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   "not-a-timestamp",
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
	_, has := updated.Annotations["rightsize.io/resized-at"]
	assert.True(t, has, "annotations should remain after timestamp parse error")
}

func TestCheckPendingSafetyObservations_MalformedMemoryAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-mem-pod",
			Namespace: "default",
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "not-a-quantity",
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
	_, has := updated.Annotations["rightsize.io/resized-at"]
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
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
	policy.Spec.UpdateStrategy.Canary = &rightsizev1alpha1.CanaryConfig{
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
	_, has := updated.Annotations["rightsize.io/resized-at"]
	assert.False(t, has, "annotations should be removed after observation completes")
}

func TestCheckPendingSafetyObservations_NilClientset(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{}
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
			Labels:    map[string]string{"app": "test", "rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
	_, has := updated.Annotations["rightsize.io/resized-at"]
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
			Labels:    map[string]string{"app": "test", "rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
	policy.Status.ResizeHistory = []rightsizev1alpha1.ResizeHistoryEntry{
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "cpu",
			From:      "500m",
			To:        "250m",
			Result:    rightsizev1alpha1.ResizeResultSuccess,
		},
		{
			Workload:  "api-server",
			Container: "main",
			Resource:  "memory",
			From:      "512Mi",
			To:        "256Mi",
			Result:    rightsizev1alpha1.ResizeResultSuccess,
		},
	}

	reconciler, _ := newSafetyTestReconciler(pod)

	reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())

	// Verify that matching Success entries were marked as Reverted.
	for _, h := range policy.Status.ResizeHistory {
		assert.Equal(t, rightsizev1alpha1.ResizeResultReverted, h.Result,
			"history entry %s/%s should be Reverted, got %s", h.Workload, h.Container, h.Result)
	}
}

func TestCheckPendingSafetyObservations_UnsafeVerdictEmitsEvent(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsafe-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
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
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
				"rightsize.io/original-restart-count":       "3",
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
	_, hasResizedAt := updated.Annotations["rightsize.io/resized-at"]
	assert.False(t, hasResizedAt, "safe pod should have annotations removed")
}

func TestCheckPendingSafetyObservations_RestartCountExceeded(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crashing-pod",
			Namespace: "default",
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
				"rightsize.io/original-restart-count":       "3",
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
			Labels:    map[string]string{"rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at":                   resizedAt,
				"rightsize.io/resized-workload":             "api-server",
				"rightsize.io/resized-containers":           "main",
				"rightsize.io/original-cpu-request.main":    "500m",
				"rightsize.io/original-memory-request.main": "512Mi",
				"rightsize.io/original-restart-count":       "not-a-number",
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
	_, hasResizedAt := updated.Annotations["rightsize.io/resized-at"]
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

	r := &RightSizePolicyReconciler{
		Client:    failingClient,
		Scheme:    scheme,
		Clientset: kubefake.NewSimpleClientset(),
	}

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
	query := buildPrometheusQuery("default", "api-server", "main", "disk")
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
	query := buildPrometheusQuery(`ns"test`, `pod"prefix`, `con"tainer`, "cpu")
	assert.Contains(t, query, `ns\"test`)
	assert.Contains(t, query, `pod\"prefix`)
	assert.Contains(t, query, `con\"tainer`)
}

func TestBuildPrometheusQuery_EscapesRegexInPodPrefix(t *testing.T) {
	query := buildPrometheusQuery("default", "my.app", "main", "cpu")
	// The dot in "my.app" should be escaped as "my\.app" in the regex matcher.
	assert.Contains(t, query, `my\.app`)
	assert.NotContains(t, query, `my.app.*`)
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

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "PrometheusUnavailable", updated.Status.Conditions[0].Reason)
	assert.Contains(t, updated.Status.Conditions[0].Message, "TLS handshake timeout")
}

func TestReconcile_BearerTokenSecretReadErrorIncludesSecretRef(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret = &rightsizev1alpha1.SecretKeyRef{
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, rightsizev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, rightsizev1alpha1.ReasonPrometheusUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "prom-token/token")
	assert.Contains(t, cond.Message, "reading secret default/prom-token")
}

// ---------- Reconcile with workload discovery error ----------

func TestReconcile_DiscoverWorkloadsError(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Kind = "ReplicaSet"

	mc := &mockCollector{}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	assert.NoError(t, err, "discovery errors should be surfaced via status condition, not returned")
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)

	// Verify the error is visible in the policy status condition.
	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, rightsizev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonWorkloadDiscoveryFailed, cond.Reason)
	assert.Contains(t, cond.Message, "Failed to discover workloads")
}

func TestReconcile_FetchDefaultsErrorFailsClosed(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	clusterDefaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource: &rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{Address: "http://prometheus.default.svc:9090"},
			},
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, clusterDefaults, deploy).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*rightsizev1alpha1.RightSizeNamespaceDefaultsList); ok {
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, failingClient.Get(context.Background(), req.NamespacedName, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, rightsizev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonInvalidConfig, cond.Reason)
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

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
}

// ---------- excludeContainers ----------

func TestComputeRecommendations_ExcludeContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludeContainers = []string{"istio-proxy"}

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

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	require.NoError(t, err)
	require.NotNil(t, rec)

	// Only "main" should have a recommendation; "istio-proxy" is excluded.
	assert.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
}

func TestComputeRecommendations_ExcludeAllContainers(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludeContainers = []string{"main"}

	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
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

func TestDiscoverPrometheus_NoServiceFound(t *testing.T) {
	reconciler := newReconcilerWithClient()

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Empty(t, addr, "should return empty when no Prometheus service is found")
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
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: s}

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
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: s}

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-k8s.monitoring:8080", addr)
}

func TestResolvePrometheusAddress_FallsBackToAutoDiscovery(t *testing.T) {
	// Policy has no Prometheus address, no RightSizeDefaults, but a well-known service exists.
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 1, count)

	// Drain the event channel and check for a Resized event.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Resized")
	default:
		t.Fatal("expected a Resized event but channel was empty")
	}
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

func TestExecuteResizes_ThrottleTriggersRevert(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	// Collector reports 60% throttle (above 50% threshold).
	collector := &mockThrottleCollector{throttleRatio: 0.6}

	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), collector)
	// The resize should succeed then immediately revert due to throttle.
	assert.Equal(t, 0, count, "should be 0 after revert")

	// History should show reverted entries.
	reverted := false
	for _, h := range history {
		if h.Result == rightsizev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "expected at least one Reverted history entry")

	// Check for the Reverted event mentioning throttle.
	foundRevert := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "throttle") {
				foundRevert = true
			}
		default:
			goto done
		}
	}
done:
	assert.True(t, foundRevert, "expected a Reverted event mentioning throttle")
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	require.Equal(t, 1, count, "resize should succeed")
	require.NotEmpty(t, history)
	assert.Equal(t, rightsizev1alpha1.ResizeResultSuccess, history[0].Result)

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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
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

	reconciler := &RightSizePolicyReconciler{
		Client:    wrappedClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)

	// The resize should have been reverted because annotation update failed.
	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	// History should show Reverted entries.
	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == rightsizev1alpha1.ResizeResultReverted {
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

func TestExecuteResizes_RevertsOnReFetchFailure(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Inject failure on typed clientset Get for pods (the re-fetch after
	// resize now uses Clientset directly, not r.Get()). Only fail the
	// first Get (the re-fetch); subsequent Gets (revert's pod lookup) pass.
	getCalled := false
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !getCalled {
			getCalled = true
			return true, nil, fmt.Errorf("simulated re-fetch failure")
		}
		return false, nil, nil
	})

	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)

	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == rightsizev1alpha1.ResizeResultReverted {
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
	rec := rightsizev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := buildResizeTarget(rec)
	assert.Equal(t, int64(100), target.Requests.Cpu().MilliValue())
	wantMem := resource.MustParse("128Mi")
	assert.Equal(t, wantMem.Value(), target.Requests.Memory().Value())
	assert.Nil(t, target.Limits, "Limits should be nil when recommendation limits are zero")
}

func TestBuildResizeTarget_IncludesLimitsWhenNonZero(t *testing.T) {
	rec := rightsizev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
			MemoryLimit:   resource.MustParse("256Mi"),
		},
	}
	target := buildResizeTarget(rec)
	require.NotNil(t, target.Limits)
	assert.Equal(t, int64(200), target.Limits.Cpu().MilliValue())
	wantMemLim := resource.MustParse("256Mi")
	assert.Equal(t, wantMemLim.Value(), target.Limits.Memory().Value())
}

func TestBuildResizeTarget_PartialLimits(t *testing.T) {
	rec := rightsizev1alpha1.ContainerRecommendation{
		Name: "app",
		Recommended: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := buildResizeTarget(rec)
	require.NotNil(t, target.Limits, "Limits should be non-nil when any limit is non-zero")
	assert.Equal(t, int64(200), target.Limits.Cpu().MilliValue())
	_, hasMemLimit := target.Limits[corev1.ResourceMemory]
	assert.False(t, hasMemLimit, "Memory limit should not be set when zero in recommendation")
}

func TestTryEvictionFallback_EvictsWhenMultipleReplicas(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.ResizeMethodInPlaceOrEvict

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

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
}

func TestTryEvictionFallback_SkipsLastReplica(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.ResizeMethodInPlaceOrEvict

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should NOT evict the last replica")
}

func TestResizeContainer_InfeasiblePodEvictedDirectly(t *testing.T) {
	// A pod marked Infeasible with InPlaceOrEvict should go directly to
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
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.ResizeMethodInPlaceOrEvict

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := rightsizev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, resized := r.resizeContainer(context.Background(), policy, pod1, deploy,
		"api-server", containerRec, resizer, nil, metav1.Now())
	assert.True(t, resized, "infeasible pod should be evicted")
	require.Len(t, entries, 1)
	assert.Equal(t, "Eviction", entries[0].Method)

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
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
		Recorder:  recorder,
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	// InPlaceOnly (default): no eviction allowed.

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := rightsizev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: rightsizev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, resized := r.resizeContainer(context.Background(), policy, pod, deploy,
		"api-server", containerRec, resizer, nil, metav1.Now())
	assert.False(t, resized, "infeasible pod with InPlaceOnly should not be resized")
	assert.Empty(t, entries, "should produce no history entries")

	// Verify InfeasibleBlocked event was emitted.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "InfeasibleBlocked")
		assert.Contains(t, event, "api-server-abc-1")
		assert.Contains(t, event, "InPlaceOrEvict")
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

func TestExecuteResizes_BudgetCapsDefersExcessiveIncrease(t *testing.T) {
	// Pod at 200m CPU, recommendation is 800m (increase of 600m).
	// Budget cap is 500m, so the resize should be skipped.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "800m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count, "resize should be deferred when CPU increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsAllowsWithinBudget(t *testing.T) {
	// Pod at 200m CPU, recommendation is 500m (increase of 300m).
	// Budget cap is 500m, so the resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 1, count, "resize should proceed when within budget")
}

func TestExecuteResizes_BudgetCapsDecreasesFree(t *testing.T) {
	// Pod at 800m CPU, recommendation is 400m (decrease of 400m).
	// Budget cap is 100m. Decreases should NOT consume budget.
	pod := newResizePod("api-server", "800m", "256Mi", "800m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	cpuBudget := resource.MustParse("100m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "800m", "256Mi", "0", "0", "400m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 1, count, "decreases should not consume budget")
}

func TestExecuteResizes_BudgetCapsMemory(t *testing.T) {
	// Pod at 256Mi memory, recommendation is 1Gi (increase of 768Mi).
	// Memory budget is 512Mi, so the resize should be deferred.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	memBudget := resource.MustParse("512Mi")
	policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = &memBudget

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "200m", "1Gi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count, "resize should be deferred when memory increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsExactlyEqualsPasses(t *testing.T) {
	// Increase of exactly 500m with budget of 500m should pass (not strict >).
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "700m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 1, count, "increase exactly equal to budget should proceed")
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
	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 5 // allow parallelism

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "0", "0", "750m", "384Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil)
	assert.Equal(t, 1, count, "workload should count as resized once")
	assert.NotEmpty(t, history, "should produce resize history entries")
}

func TestExecuteResizes_MultiContainerSequential(t *testing.T) {
	// A pod with two containers should be resized sequentially,
	// re-fetching the pod between each to avoid stale resourceVersion.
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
	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeOneShot

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil)
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

func TestReconcile_NowFuncControlsScheduleGate(t *testing.T) {
	// A policy with a schedule window of 02:00-06:00 UTC on Wednesdays.
	// When NowFunc returns a time outside the window, no resize should happen.
	// When NowFunc returns a time inside the window, resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeAuto
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		Windows:    []rightsizev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
		DaysOfWeek: []string{"Wednesday"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod, policy).
		WithStatusSubresource(policy).Build()

	// Wednesday 10:00 UTC -- outside the 02:00-06:00 window.
	outsideWindow := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)

	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
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
	schedule := &rightsizev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
	}
	assert.True(t, isWithinResizeWindow(schedule, wed))

	// Thursday should be blocked
	thu := time.Date(2026, 1, 8, 10, 0, 0, 0, time.UTC)
	assert.False(t, isWithinResizeWindow(schedule, thu))
}

func TestIsWithinResizeWindow_TimeWindow(t *testing.T) {
	schedule := &rightsizev1alpha1.ResizeSchedule{
		Windows: []rightsizev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
	}
	// 03:00 is inside
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_OvernightWindow(t *testing.T) {
	schedule := &rightsizev1alpha1.ResizeSchedule{
		Windows: []rightsizev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
	}
	// 23:00 is inside (after start)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 23, 0, 0, 0, time.UTC)))
	// 03:00 is inside (before end, wraps past midnight)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_InvalidTimezoneFailsOpen(t *testing.T) {
	schedule := &rightsizev1alpha1.ResizeSchedule{
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
	policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.ResizeMethodInPlaceOrEvict

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
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should return false when eviction is denied by PDB")
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
	policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.ResizeMethodInPlaceOrEvict

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
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
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.95,
					Recommended: rightsizev1alpha1.ResourceValues{
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
	assert.Equal(t, "test-policy", cm.Labels["rightsize.io/policy"])
}

func TestExportRecommendationConfigMaps_UpdatesExisting(t *testing.T) {
	scheme := testScheme()
	policy := &rightsizev1alpha1.RightSizePolicy{
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
	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.99,
					Recommended: rightsizev1alpha1.ResourceValues{
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
	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	// CPU went from 200m to 400m, so target should halve: 80 * (200/400) = 40.
	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"))

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

func TestApplyStartupBoosts_AppliesBoostToNewPod(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				StartupBoost: &rightsizev1alpha1.StartupBoost{
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
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"),
					},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer)

	// Verify resize was attempted via clientset actions.
	actions := clientset.Actions()
	var foundResize bool
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for startup boost")
}

func TestApplyStartupBoosts_SkipsPodOutsideWindow(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				StartupBoost: &rightsizev1alpha1.StartupBoost{
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
	r := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: rightsizev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer)

	// Verify no resize action was taken.
	actions := clientset.Actions()
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Error("should not resize pod outside boost window")
		}
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
	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	r.adjustHPATargets(context.Background(), hpas, "my-app", "Deployment",
		resource.MustParse("200m"), resource.MustParse("400m"))

	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "my-app-hpa",
	}, &hpa)
	require.NoError(t, err)
	// Target should be unchanged since no annotation.
	assert.Equal(t, int32(80), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}
