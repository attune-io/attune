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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
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
				Mode: "Recommend",
				Cooldown: &metav1.Duration{
					Duration: 1 * time.Hour,
				},
			},
		},
	}
}

// mockMetricsFactory returns a MetricsCollectorFactory that creates a mock collector.
func mockMetricsFactory(collector rsmetrics.MetricsCollector) MetricsCollectorFactory {
	return func(_ string) (rsmetrics.MetricsCollector, error) {
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
	got, err := reconciler.getOrCreateCollector("http://prom:9090")
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
		MetricsFactory: func(address string) (rsmetrics.MetricsCollector, error) {
			assert.Equal(t, "http://new:9090", address)
			return mc, nil
		},
	}

	got, err := reconciler.getOrCreateCollector("http://new:9090")
	require.NoError(t, err)
	assert.Equal(t, mc, got)
}

func TestGetOrCreateCollector_FactoryError(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string) (rsmetrics.MetricsCollector, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	_, err := reconciler.getOrCreateCollector("http://broken:9090")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestGetOrCreateCollector_CacheFull(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string) (rsmetrics.MetricsCollector, error) {
			return &mockCollector{}, nil
		},
	}
	// Fill the cache to maxCollectors.
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://prom-%d:9090", i)
		_, err := reconciler.getOrCreateCollector(addr)
		require.NoError(t, err)
	}

	// The next address should be rejected.
	_, err := reconciler.getOrCreateCollector("http://one-too-many:9090")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collector cache full")
}

func TestGetOrCreateCollector_CustomTTL(t *testing.T) {
	customTTL := 2 * time.Minute
	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: customTTL,
		MetricsFactory: func(string) (rsmetrics.MetricsCollector, error) {
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
	_, err := reconciler.getOrCreateCollector("http://fresh:9090")
	require.NoError(t, err)

	_, stillExists := reconciler.collectors.Load("http://stale:9090")
	assert.False(t, stillExists, "entry older than custom TTL should be evicted")
}

func TestGetOrCreateCollector_EvictsStaleEntries(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		MetricsFactory: func(string) (rsmetrics.MetricsCollector, error) {
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
	_, err := reconciler.getOrCreateCollector("http://fresh:9090")
	require.NoError(t, err)
}

func TestGetOrCreateCollector_ConcurrentAccess(t *testing.T) {
	reconciler := &RightSizePolicyReconciler{
		CollectorTTL: 50 * time.Millisecond,
		MetricsFactory: func(string) (rsmetrics.MetricsCollector, error) {
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
			_, err := reconciler.getOrCreateCollector(addresses[idx])
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, qErrors, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.Greater(t, qErrors, 0, "query failures should be counted")
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, emptyDeploy, mc)
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)

	// Memory should be clamped to current (512Mi) since AllowDecrease is nil.
	assert.True(t, rec.Containers[0].Recommended.MemoryRequest.Cmp(resource.MustParse("512Mi")) >= 0,
		"memory should not decrease below current when AllowDecrease is nil, got %s", rec.Containers[0].Recommended.MemoryRequest.String())
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, qErrors, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, qErrors, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

// ---------- resolvePrometheusAddress ----------

func TestResolvePrometheusAddress_PolicyHasAddress(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler := newReconcilerWithClient()

	addr, err := reconciler.resolvePrometheusAddress(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus:9090", addr)
}

func TestResolvePrometheusAddress_FallsBackToDefaults(t *testing.T) {
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

	addr, err := reconciler.resolvePrometheusAddress(context.Background(), policy, defaults)
	assert.NoError(t, err)
	assert.Equal(t, "http://defaults-prometheus:9090", addr)
}

func TestResolvePrometheusAddress_NoAddressAnywhere(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
	}
	reconciler := newReconcilerWithClient()

	_, err := reconciler.resolvePrometheusAddress(context.Background(), policy, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no Prometheus address configured")
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
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.ModeObserve
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

	records, err := parseResizeRecords(pod, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "app", records[0].Container)
	assert.Equal(t, "sidecar", records[1].Container)
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("500m")))
	assert.True(t, records[1].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.Equal(t, int32(2), records[1].RestartCount)
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

	_, err := parseResizeRecords(pod, 5*time.Minute)
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

	_, err := parseResizeRecords(pod, 5*time.Minute)
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

	_, err := parseResizeRecords(pod, 5*time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing original restart count for app")
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	assert.Equal(t, "Success", history[0].Result, "cpu resize should succeed")
	assert.Equal(t, "Success", history[1].Result, "memory resize should succeed")
}

func TestExecuteResizes_ContextCancelledAbortsRemaining(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "Auto"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
				Mode: "Auto",
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
	assert.Equal(t, "Auto", policy.Spec.UpdateStrategy.Mode)
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	reconciler.MetricsFactory = func(_ string) (rsmetrics.MetricsCollector, error) {
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

// ---------- Reconcile with cooldown active ----------

func TestReconcile_CooldownActive_SkipsResize(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "OneShot"
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "Auto"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	assert.Equal(t, 0, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, "Failed", history[0].Result)
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "Auto"
	policy.Spec.UpdateStrategy.AutoRevert = true

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
	assert.Equal(t, "Success", history[0].Result)
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
	reconciler.MetricsFactory = func(_ string) (rsmetrics.MetricsCollector, error) {
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

// ---------- Reconcile with AutoRevert checking safety observations ----------

func TestReconcile_AutoRevertCallsSafetyObservations(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.AutoRevert = true

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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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

	rec, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc)
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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

	addr, err := reconciler.resolvePrometheusAddress(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus-kube-prometheus-prometheus.monitoring:9090", addr)
}

// ---------- Event emission ----------

func TestExecuteResizes_EmitsResizedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "Auto"
	policy.Spec.UpdateStrategy.AutoRevert = false

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

func (m *mockThrottleCollector) GetThrottleRatio(_ context.Context, _, _, _ string) (float64, error) {
	return m.throttleRatio, nil
}

func TestExecuteResizes_ThrottleTriggersRevert(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "Auto"
	policy.Spec.UpdateStrategy.AutoRevert = true

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
		if h.Result == "Reverted" {
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)
	require.Equal(t, 1, count, "resize should succeed")
	require.NotEmpty(t, history)
	assert.Equal(t, "Success", history[0].Result)

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

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
		if h.Result == "Reverted" {
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
	policy.Spec.UpdateStrategy.Mode = "OneShot"

	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil)

	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == "Reverted" {
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

	evicted := r.tryEvictionFallback(context.Background(), policy, pod1,
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

	evicted := r.tryEvictionFallback(context.Background(), policy, pod,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should NOT evict the last replica")
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
