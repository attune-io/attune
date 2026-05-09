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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardif/kube-rightsize/internal/metrics"
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
				MinimumDataPoints: 168,
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
	queryRangeFunc func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]rsmetrics.Sample, error)
	queryFunc      func(ctx context.Context, query string, ts time.Time) (float64, error)
}

func (m *mockCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]rsmetrics.Sample, error) {
	if m.queryRangeFunc != nil {
		return m.queryRangeFunc(ctx, query, start, end, step)
	}
	return nil, nil
}

func (m *mockCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, ts)
	}
	return 0, nil
}

func TestDiscoverWorkloads_FindsDeploymentByName(t *testing.T) {
	scheme := testScheme()
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: client,
		Scheme: scheme,
	}

	policy := newTestPolicy("test-policy", "default")

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "api-server", workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsDeploymentsByLabelSelector(t *testing.T) {
	scheme := testScheme()
	deploy1 := newTestDeployment("api-server-1", "default", map[string]string{"tier": "api"})
	deploy2 := newTestDeployment("api-server-2", "default", map[string]string{"tier": "api"})
	deploy3 := newTestDeployment("worker", "default", map[string]string{"tier": "worker"})

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy1, deploy2, deploy3).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: client,
		Scheme: scheme,
	}

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
	scheme := testScheme()
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: client,
		Scheme: scheme,
	}

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
	scheme := testScheme()
	deploy := newTestDeployment("api-server", "default", nil)

	pod1 := newTestPod("api-server-abc-123", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-456", "default", map[string]string{"app": "api-server"})
	pod3 := newTestPod("worker-def-789", "default", map[string]string{"app": "worker"})

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy, pod1, pod2, pod3).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: client,
		Scheme: scheme,
	}

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
	scheme := testScheme()
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: client,
		Scheme: scheme,
	}

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
	scheme := testScheme()

	policy := newTestPolicy("test-policy", "default")

	// Use a fake client with status subresource support.
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	mc := &mockCollector{}
	reconciler := &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}

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
			name:       "zero current req returns new req as limit",
			currentReq: "0",
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

func TestComputeSavings_ReturnsCorrectStructure(t *testing.T) {
	r := &RightSizePolicyReconciler{}
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
	savings := r.computeSavings("test-ns", recs)
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
	assert.Equal(t, int32(168), r.getMinimumDataPoints(policy))
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

func TestDiscoverWorkloads_FindsStatefulSetByName(t *testing.T) {
	scheme := testScheme()
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

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

// ---------- computeRecommendations ----------

func TestComputeRecommendations_HappyPath(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	rec, err := reconciler.computeRecommendations(context.Background(), policy, deploy, nil, mc)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, "main", rec.Containers[0].Name)
	assert.Greater(t, rec.Containers[0].DataPoints, int32(0))
	assert.Greater(t, rec.Containers[0].Confidence, 0.0)
}

func TestComputeRecommendations_InsufficientDataPoints(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(50, 0.1), nil // Only 50 samples, below 168 threshold
		},
	}

	rec, err := reconciler.computeRecommendations(context.Background(), policy, deploy, nil, mc)
	assert.NoError(t, err)
	assert.Nil(t, rec) // No recommendation because data points are insufficient
}

func TestComputeRecommendations_QueryError(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	rec, err := reconciler.computeRecommendations(context.Background(), policy, deploy, nil, mc)
	assert.NoError(t, err) // Errors are logged but do not bubble up
	assert.Nil(t, rec)     // No data means no recommendations
}

func TestComputeRecommendations_EmptyContainers(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")

	emptyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	mc := &mockCollector{}

	rec, err := reconciler.computeRecommendations(context.Background(), policy, emptyDeploy, nil, mc)
	assert.NoError(t, err)
	assert.Nil(t, rec)
}

// ---------- resolvePrometheusAddress ----------

func TestResolvePrometheusAddress_PolicyHasAddress(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	// newTestPolicy sets Prometheus.Address to "http://prometheus:9090".

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	addr, err := reconciler.resolvePrometheusAddress(context.Background(), policy)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus:9090", addr)
}

func TestResolvePrometheusAddress_FallsBackToDefaults(t *testing.T) {
	scheme := testScheme()

	// Policy WITHOUT a Prometheus address.
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
	}

	// Cluster-scoped RightSizeDefaults with a Prometheus address.
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	addr, err := reconciler.resolvePrometheusAddress(context.Background(), policy)
	assert.NoError(t, err)
	assert.Equal(t, "http://defaults-prometheus:9090", addr)
}

func TestResolvePrometheusAddress_NoAddressAnywhere(t *testing.T) {
	scheme := testScheme()

	// Policy WITHOUT a Prometheus address.
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       rightsizev1alpha1.RightSizePolicySpec{MetricsSource: rightsizev1alpha1.MetricsSource{}},
	}

	// No RightSizeDefaults in the cluster.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.resolvePrometheusAddress(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no Prometheus address configured")
}

// ---------- updateStatusWithRetry ----------

func TestUpdateStatusWithRetry_SuccessFirstAttempt(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

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
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

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

// ---------- markResizeTime ----------

func TestMarkResizeTime_NoExistingAnnotations(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	// policy.Annotations is nil by default.

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &updated))

	resizeTime, ok := updated.Annotations[lastResizeAnnotation]
	assert.True(t, ok, "last-resize-time annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, resizeTime)
	assert.NoError(t, parseErr, "annotation value should be valid RFC3339")
}

func TestMarkResizeTime_ExistingAnnotations(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	policy.Annotations = map[string]string{"existing-key": "existing-value"}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		Build()
	reconciler := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-policy", Namespace: "default"}

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &p))

	err := reconciler.markResizeTime(ctx, &p)
	require.NoError(t, err)

	var updated rightsizev1alpha1.RightSizePolicy
	require.NoError(t, fakeClient.Get(ctx, key, &updated))

	assert.Equal(t, "existing-value", updated.Annotations["existing-key"])
	resizeTime, ok := updated.Annotations[lastResizeAnnotation]
	assert.True(t, ok, "last-resize-time annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, resizeTime)
	assert.NoError(t, parseErr, "annotation value should be valid RFC3339")
}

// ---------- Reconcile happy path ----------

func TestReconcile_HappyPathWithRecommendations(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	reconciler := &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}

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
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)

	// Verify Ready condition.
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "Monitoring", updated.Status.Conditions[0].Reason)
}

// ---------- checkPendingSafetyObservations ----------

func TestCheckPendingSafetyObservations_ObservationElapsed(t *testing.T) {
	scheme := testScheme()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"rightsize.io/resized-at":              resizedAt,
				"rightsize.io/resized-container":       "main",
				"rightsize.io/original-cpu-request":    "500m",
				"rightsize.io/original-memory-request": "512Mi",
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	reconciler.checkPendingSafetyObservations(context.Background(), policy)

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
	scheme := testScheme()

	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"rightsize.io/resized-at":              resizedAt,
				"rightsize.io/resized-container":       "main",
				"rightsize.io/original-cpu-request":    "not-a-quantity", // malformed
				"rightsize.io/original-memory-request": "512Mi",
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	// Should not panic when the annotation value is unparseable.
	assert.NotPanics(t, func() {
		reconciler.checkPendingSafetyObservations(context.Background(), policy)
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
	scheme := testScheme()

	// Just resized -- observation period has NOT elapsed yet.
	resizedAt := time.Now().UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recent-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"rightsize.io/resized-at":              resizedAt,
				"rightsize.io/resized-container":       "main",
				"rightsize.io/original-cpu-request":    "500m",
				"rightsize.io/original-memory-request": "512Mi",
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := &RightSizePolicyReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Clientset: clientset,
	}

	reconciler.checkPendingSafetyObservations(context.Background(), policy)

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

	count, history := reconciler.executeResizes(context.Background(), policy, nil, nil)
	assert.Equal(t, 0, count)
	assert.Nil(t, history)
}

// ---------- listWorkloadsBySelector (StatefulSet + DaemonSet paths) ----------

func TestListWorkloadsBySelector_StatefulSets(t *testing.T) {
	scheme := testScheme()
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts1, sts2).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "db"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "StatefulSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 2)
}

func TestListWorkloadsBySelector_DaemonSets(t *testing.T) {
	scheme := testScheme()
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "log-agent", Namespace: "default", Labels: map[string]string{"role": "logging"}},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "log-agent"}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "logging"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "DaemonSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "log-agent", workloads[0].GetName())
}

func TestListWorkloadsBySelector_UnsupportedKind(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "CronJob", selector)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// ---------- getWorkloadByName (DaemonSet + unsupported kind) ----------

func TestDiscoverWorkloads_FindsDaemonSetByName(t *testing.T) {
	scheme := testScheme()
	name := "node-agent"
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

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

func TestDiscoverWorkloads_UnsupportedKind(t *testing.T) {
	scheme := testScheme()
	name := "my-job"
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "CronJob",
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

// ---------- mergeDefaults (more paths) ----------

func TestMergeDefaults_MergesAllFields(t *testing.T) {
	scheme := testScheme()

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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaults).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	// Policy with all zeros/empty (should inherit from defaults).
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	r.mergeDefaults(context.Background(), policy)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, "1.4", policy.Spec.Memory.SafetyMargin)
	assert.Equal(t, "Auto", policy.Spec.UpdateStrategy.Mode)
}

func TestMergeDefaults_PolicyOverridesDefaults(t *testing.T) {
	scheme := testScheme()

	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU:    &rightsizev1alpha1.ResourceConfig{Percentile: 90, SafetyMargin: "1.5"},
			Memory: &rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.4"},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaults).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	// Policy with explicit values (should NOT be overwritten).
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.1"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.2"},
		},
	}

	r.mergeDefaults(context.Background(), policy)

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
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "OneShot"

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	reconciler := &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
		// Clientset is nil, so executeResizes will log and return 0
	}

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
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	// Clear the Prometheus address to force an error.
	policy.Spec.MetricsSource.Prometheus = nil

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	reconciler := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
		MetricsFactory: func(_ string) (rsmetrics.MetricsCollector, error) {
			return nil, fmt.Errorf("connection refused")
		},
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
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Mode = "OneShot"
	// Set a recent resize time to activate cooldown.
	policy.Annotations = map[string]string{
		lastResizeAnnotation: time.Now().UTC().Format(time.RFC3339),
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	reconciler := &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}

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

func TestReconcile_WorkloadOptedOut(t *testing.T) {
	scheme := testScheme()
	policy := newTestPolicy("test-policy", "default")

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Annotations = map[string]string{"rightsize.io/skip": "true"}
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod).
		WithStatusSubresource(&rightsizev1alpha1.RightSizePolicy{}).
		Build()

	mc := &mockCollector{}

	reconciler := &RightSizePolicyReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		MetricsFactory: mockMetricsFactory(mc),
	}

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
