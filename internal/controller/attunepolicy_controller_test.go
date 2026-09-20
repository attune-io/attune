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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/recommendation"
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

// ensureTestNamespaces adds a Namespace object for every namespaced object
// so Reconcile can Get the policy namespace (attune.io/freeze). Explicit
// Namespace objects in objects are left unchanged.
func ensureTestNamespaces(objects []client.Object) []client.Object {
	have := make(map[string]struct{})
	for _, o := range objects {
		if ns, ok := o.(*corev1.Namespace); ok {
			have[ns.Name] = struct{}{}
		}
	}
	var extra []client.Object
	for _, o := range objects {
		name := o.GetNamespace()
		if name == "" {
			continue
		}
		if _, ok := have[name]; ok {
			continue
		}
		have[name] = struct{}{}
		extra = append(extra, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	if len(extra) == 0 {
		return objects
	}
	return append(extra, objects...)
}

// newReconcilerForReconcile creates a reconciler with status subresource
// support and a mock metrics factory, ready for Reconcile tests.
func newReconcilerForReconcile(mc rsmetrics.MetricsCollector, objects ...client.Object) (*AttunePolicyReconciler, client.Client) {
	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ensureTestNamespaces(objects)...).
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

func TestBuildPrometheusQuery_CPU(t *testing.T) {
	query := buildPrometheusQuery("production", "api-server-[a-z0-9]+-[a-z0-9]{5}", "main", "cpu", 5*time.Minute)
	expected := `max by (container) (rate(container_cpu_usage_seconds_total{namespace="production",pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}",container="main"}[5m]))`
	assert.Equal(t, expected, query)
}

func TestBuildPrometheusQuery_Memory(t *testing.T) {
	query := buildPrometheusQuery("production", "api-server-[a-z0-9]+-[a-z0-9]{5}", "main", "memory", 5*time.Minute)
	expected := `max by (container) (container_memory_working_set_bytes{namespace="production",pod=~"api-server-[a-z0-9]+-[a-z0-9]{5}",container="main"})`
	assert.Equal(t, expected, query)
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

func TestMemoryFromCPURatioSet(t *testing.T) {
	assert.False(t, memoryFromCPURatioSet(nil))
	p := newTestPolicy("p", "default")
	assert.False(t, memoryFromCPURatioSet(p), "unset ratio is not set")
	empty := ""
	p.Spec.Memory.MemoryFromCPURatio = &empty
	assert.False(t, memoryFromCPURatioSet(p), "empty ratio is not set")
	for _, bad := range []string{"abc", "0", "-1", "NaN", "Inf", "1001"} {
		v := bad
		p.Spec.Memory.MemoryFromCPURatio = &v
		assert.False(t, memoryFromCPURatioSet(p), "invalid ratio %q must not gate the wait path", bad)
	}
	ok := "2.0"
	p.Spec.Memory.MemoryFromCPURatio = &ok
	assert.True(t, memoryFromCPURatioSet(p))
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

// generateSamples creates metric samples spread over hourly intervals for testing.
func generateSamples(count int, baseValue float64) []rsmetrics.Sample {
	samples := make([]rsmetrics.Sample, count)
	now := time.Now()
	for i := 0; i < count; i++ {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(count-1-i) * time.Hour),
			Value:     baseValue + float64(i%10)*0.01,
		}
	}
	return samples
}

func (c *closableMockCollector) Close() error {
	c.closed = true
	return nil
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

// recSizedAPIServerDeploy returns a fresh api-server Deployment whose template
// is already at the recommended size (250m/256Mi). Used by persist-on restore
// tests so AfterSuccessfulResize restore is visible.
func recSizedAPIServerDeploy() *appsv1.Deployment {
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	res := &deploy.Spec.Template.Spec.Containers[0].Resources
	res.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	res.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	res.Limits[corev1.ResourceCPU] = resource.MustParse("500m")
	res.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	return deploy
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

func TestIsBatchWorkload(t *testing.T) {
	assert.True(t, isBatchWorkload(&batchv1.CronJob{}))
	assert.True(t, isBatchWorkload(&batchv1.Job{}))
	assert.False(t, isBatchWorkload(&appsv1.Deployment{}))
	assert.False(t, isBatchWorkload(&appsv1.StatefulSet{}))
	assert.False(t, isBatchWorkload(&appsv1.DaemonSet{}))
	assert.False(t, isBatchWorkload(&appsv1.ReplicaSet{}))
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

// ---------- Reconcile with opt-out annotation ----------

func TestSafeInt32_Normal(t *testing.T) {
	assert.Equal(t, int32(42), safeInt32(42))
	assert.Equal(t, int32(0), safeInt32(0))
}

func TestSafeInt32_Overflow(t *testing.T) {
	assert.Equal(t, int32(math.MaxInt32), safeInt32(math.MaxInt32+1))
	assert.Equal(t, int32(math.MaxInt32), safeInt32(math.MaxInt))
}

func (c *cancelAwareResizeClientset) CoreV1() corev1client.CoreV1Interface {
	return &cancelAwareResizeCoreV1{CoreV1Interface: c.Interface.CoreV1()}
}

type cancelAwareResizeCoreV1 struct {
	corev1client.CoreV1Interface
}

func (c *cancelAwareResizeCoreV1) Pods(namespace string) corev1client.PodInterface {
	return &cancelAwareResizePods{PodInterface: c.CoreV1Interface.Pods(namespace)}
}

type cancelAwareResizePods struct {
	corev1client.PodInterface
}

func (p *cancelAwareResizePods) UpdateResize(ctx context.Context, name string, pod *corev1.Pod, opts metav1.UpdateOptions) (*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.PodInterface.UpdateResize(ctx, name, pod, opts)
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

func TestForgetPolicyRuntimeState_RecreateStartsFullBucket(t *testing.T) {
	r := NewAttunePolicyReconciler()
	now := time.Now()
	old := newIncreaseRateBucket(1000, -1, now)
	require.True(t, old.tryDraw(1000, 0, now))
	assert.False(t, old.tryDraw(1, 0, now), "drained bucket must reject another draw")
	r.increaseRates.Store("default/p", old)

	r.forgetPolicyRuntimeState("default", "p")
	_, ok := r.increaseRates.Load("default/p")
	assert.False(t, ok)

	fresh := newIncreaseRateBucket(1000, -1, now)
	r.increaseRates.Store("default/p", fresh)
	assert.True(t, fresh.tryDraw(1000, 0, now), "recreated policy must start with a full bucket")
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

func (m *mockThrottleCollector) GetThrottleRatio(_ context.Context, _, _, _ string, _ time.Time) (float64, error) {
	return m.throttleRatio, nil
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

func (f *failOnPodUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return fmt.Errorf("simulated annotation update failure")
	}
	return f.Client.Update(ctx, obj, opts...)
}

// commitThenTimeoutPodClient commits the pod Update, mirrors it into the
// Clientset (persist confirm Get is a Clientset read), then returns timeout
// on the first N pod Updates.
type commitThenTimeoutPodClient struct {
	client.Client
	cs           *kubefake.Clientset
	timeoutsLeft int
	timeoutsSeen int
}

func (c *commitThenTimeoutPodClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return c.Client.Update(ctx, obj, opts...)
	}
	if err := c.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Pods(pod.Namespace).Update(ctx, pod.DeepCopy(), metav1.UpdateOptions{}); err != nil {
		return err
	}
	if c.timeoutsLeft > 0 {
		c.timeoutsLeft--
		c.timeoutsSeen++
		return apierrors.NewTimeoutError("injected timeout after committed annotation persist", 0)
	}
	return nil
}

// cancelAwareGetClientset fails Get when ctx is already cancelled, except
// the first persist re-fetch (so confirm is what we are testing).
type cancelAwareGetClientset struct {
	kubernetes.Interface
	gets atomic.Int32
}

func (c *cancelAwareGetClientset) CoreV1() corev1client.CoreV1Interface {
	return &cancelAwareCoreV1{CoreV1Interface: c.Interface.CoreV1(), gets: &c.gets}
}

type cancelAwareCoreV1 struct {
	corev1client.CoreV1Interface
	gets *atomic.Int32
}

func (c *cancelAwareCoreV1) Pods(namespace string) corev1client.PodInterface {
	return &cancelAwarePods{PodInterface: c.CoreV1Interface.Pods(namespace), gets: c.gets}
}

type cancelAwarePods struct {
	corev1client.PodInterface
	gets *atomic.Int32
}

func (p *cancelAwarePods) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error) {
	n := p.gets.Add(1)
	if n > 1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return p.PodInterface.Get(ctx, name, opts)
}

func persistMainRec(t *testing.T) attunev1alpha1.ContainerRecommendation {
	t.Helper()
	parse := func(s string) resource.Quantity {
		t.Helper()
		q, err := resource.ParseQuantity(s)
		require.NoError(t, err, "parse quantity %q", s)
		return q
	}
	return attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    parse("500m"),
			CPULimit:      parse("1000m"),
			MemoryRequest: parse("512Mi"),
			MemoryLimit:   parse("1Gi"),
		},
	}
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
			wantMsgContains:   "Metrics query errors (2) prevented CPU data collection",
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
			wantMsgContains:   "Metrics query timeout exceeded",
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
			wantReason:        attunev1alpha1.ReasonMetricsUnavailable,
			wantMsgContains:   "Metrics query errors (1) prevented memory data collection",
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
			wantReason:        attunev1alpha1.ReasonMetricsUnavailable,
			wantMsgContains:   "Metrics query timeout exceeded after 5m0s",
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
			wantMsgContains:   "Metrics query timeout exceeded",
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

func TestRecommendationFreshnessBound(t *testing.T) {
	assert.Equal(t, 15*time.Minute, recommendationFreshnessBound(5*time.Minute))
	assert.Equal(t, 15*time.Minute, recommendationFreshnessBound(0))
	assert.Equal(t, 45*time.Second, recommendationFreshnessBound(15*time.Second))
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
