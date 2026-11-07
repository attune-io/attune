/*
Copyright 2026.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestLabelsMatch(t *testing.T) {
	assert.True(t, labelsMatch(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1"}))
	assert.False(t, labelsMatch(map[string]string{"a": "1"}, map[string]string{"a": "2"}))
	assert.False(t, labelsMatch(map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}))
	assert.False(t, labelsMatch(nil, map[string]string{"a": "1"}))
	assert.False(t, labelsMatch(map[string]string{"a": "1"}, nil))
}

func TestSamplePodsForMetrics_EvenSpacing(t *testing.T) {
	pods := make([]corev1.Pod, 0, 20)
	for i := 0; i < 20; i++ {
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p-%02d", i)}})
	}
	out := samplePodsForMetrics(pods, 5)
	require.Len(t, out, 5)
	// Deterministic across calls
	out2 := samplePodsForMetrics(pods, 5)
	assert.Equal(t, out[0].Name, out2[0].Name)
	assert.Equal(t, out[4].Name, out2[4].Name)
	// Under limit returns all
	assert.Len(t, samplePodsForMetrics(pods[:3], 5), 3)
	assert.Equal(t, pods, samplePodsForMetrics(pods, 0))
	// Empty / nil input stays empty (no panic, no synthetic pods).
	assert.Empty(t, samplePodsForMetrics(nil, 5))
	assert.Empty(t, samplePodsForMetrics([]corev1.Pod{}, 5))
	// Negative max is treated as unlimited (return input unchanged).
	assert.Equal(t, pods[:2], samplePodsForMetrics(pods[:2], -1))
}

func TestPodRegexFromNames(t *testing.T) {
	re := podRegexFromNames([]string{"app-b", "app-a"})
	assert.Equal(t, "app-a|app-b", re)
	assert.Empty(t, podRegexFromNames(nil))
}

func TestListPodsForWorkloads_SingleNSList(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))

	d1 := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app1", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "1"}},
		},
	}
	d2 := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app2", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "2"}},
		},
	}
	p1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", Labels: map[string]string{"app": "1"}}}
	p2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns", Labels: map[string]string{"app": "2"}}}
	p3 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "ns", Labels: map[string]string{"app": "1"}}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d1, d2, p1, p2, p3).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c

	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{d1, d2})
	assert.Empty(t, failed)
	require.Len(t, out["app1"], 2)
	require.Len(t, out["app2"], 1)
	assert.Equal(t, "p2", out["app2"][0].Name)
}

func TestListPodsForWorkloads_OverlappingSelectorsCountPodOnce(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))

	// b is first in the slice. The claim is still the earlier name, a.
	b := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	a := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	onlyB := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "only-b", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
		},
	}
	shared := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns", UID: "uid-shared", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			}},
		}}},
	}
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", UID: "uid-other", Labels: map[string]string{"app": "other"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b, a, onlyB, shared, other).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c

	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{b, a, onlyB})
	assert.Empty(t, failed)
	require.Len(t, out["a"], 1)
	assert.Equal(t, "shared", out["a"][0].Name)
	assert.Empty(t, out["b"])
	require.Len(t, out["only-b"], 1)
	assert.Equal(t, "other", out["only-b"][0].Name)

	savings, _ := r.computeSavings([]attunev1alpha1.WorkloadRecommendation{
		savingsRec("a", "1", "500m"),
		savingsRec("b", "1", "500m"),
		savingsRec("only-b", "1", "500m"),
	}, nil, out, failed)
	assert.Equal(t, "1", savings.CPURequestReduction)
}

func TestMetricsPodRegex_SamplesWhenLarge(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.MaxPodsInMetricsQuery = 3
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "ns"}}
	pods := make([]corev1.Pod, 10)
	for i := 0; i < 10; i++ {
		pods[i] = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("myapp-xxx-%d", i)}}
	}
	re := r.metricsPodRegex(d, pods)
	// Evenly spaced sample of 3 from 10 sorted names: indices 0, 3, 6.
	assert.Equal(t, "myapp-xxx-0|myapp-xxx-3|myapp-xxx-6", re)
	assert.NotEqual(t, r.getPodRegex(d), re)
	// Unlimited path keeps workload regex.
	r.MaxPodsInMetricsQuery = -1
	assert.Equal(t, r.getPodRegex(d), r.metricsPodRegex(d, pods))
}

func TestListPodsForWorkloads_Empty(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(testScheme()).Build()
	out, failed := r.listPodsForWorkloads(context.Background(), nil)
	assert.Empty(t, out)
	assert.Empty(t, failed)
	out, failed = r.listPodsForWorkloads(context.Background(), []client.Object{})
	assert.Empty(t, out)
	assert.Empty(t, failed)
}

func TestListPodsForWorkloads_EmptySelectorMarksUnlisted(t *testing.T) {
	scheme := testScheme()
	// Deployment without MatchLabels has empty pod selector.
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nosel", Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{},
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Labels: map[string]string{"app": "x"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, p).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{d})
	assert.Equal(t, errNoPodSelector, failed["nosel"])
	_, ok := out["nosel"]
	assert.False(t, ok, "workload without selector labels must not match every pod")

	savings, _ := r.computeSavings([]attunev1alpha1.WorkloadRecommendation{savingsRec("nosel", "1", "400m")}, nil, out, failed)
	assert.Equal(t, "600m", savings.CPURequestReduction)
}

func TestListPodsForWorkloads_BadSelectorMarksUnlisted(t *testing.T) {
	scheme := testScheme()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app",
					Operator: "NotAnOperator",
					Values:   []string{"x"},
				}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{d})
	_, ok := out["bad"]
	assert.False(t, ok)
	assert.Contains(t, failed["bad"], "pod selector:")

	savings, _ := r.computeSavings([]attunev1alpha1.WorkloadRecommendation{savingsRec("bad", "1", "400m")}, nil, out, failed)
	assert.Equal(t, "600m", savings.CPURequestReduction)
}

func TestTrimStatusError_RuneBoundary(t *testing.T) {
	short := "pod selector: bad op"
	assert.Equal(t, short, trimStatusError(short))

	// 158 ASCII bytes plus a two-byte character is exactly 160 bytes.
	exact := strings.Repeat("a", 158) + "é" + "MORE"
	got := trimStatusError(exact)
	require.True(t, utf8.ValidString(got))
	assert.Equal(t, strings.Repeat("a", 158)+"é", got)
	assert.LessOrEqual(t, len(got), 160)

	split := strings.Repeat("a", 159) + "éMORE"
	got = trimStatusError(split)
	require.True(t, utf8.ValidString(got))
	assert.Equal(t, strings.Repeat("a", 159), got)
	assert.LessOrEqual(t, len(got), 160)

	const lead = "pod selector: "
	errText := "parsing pod selector: "
	pad := 160 - len(lead) - len(errText) - 1
	require.Positive(t, pad)
	errText += strings.Repeat("b", pad) + "éTAIL"
	require.False(t, utf8.ValidString((lead + errText)[:160]))
	msg := listOrSelectorMessage(fmt.Errorf("%s", errText))
	require.True(t, utf8.ValidString(msg))
	assert.LessOrEqual(t, len(msg), 160)
	assert.True(t, strings.HasPrefix(msg, lead))
}

func TestListOrSelectorMessage(t *testing.T) {
	assert.Equal(t, errCouldNotListPods, listOrSelectorMessage(nil))
	assert.Equal(t, errCouldNotListPods, listOrSelectorMessage(fmt.Errorf("listing pods for workload app: boom")))
	assert.Equal(t, errNoPodSelector, listOrSelectorMessage(fmt.Errorf("workload ns/app has no pod selector labels")))
	assert.Contains(t, listOrSelectorMessage(fmt.Errorf("parsing pod selector for workload app: bad op")), "pod selector:")
}

func TestCapWorkloadErrors_PrefersUnlistedWithinMax(t *testing.T) {
	existing := make([]attunev1alpha1.WorkloadError, attunev1alpha1.MaxWorkloadErrors)
	for i := range existing {
		existing[i] = attunev1alpha1.WorkloadError{Workload: fmt.Sprintf("w%d", i), Error: "query"}
	}
	extra := []attunev1alpha1.WorkloadError{{Workload: "nosel", Error: errNoPodSelector}}
	existing[9] = attunev1alpha1.WorkloadError{Workload: "rollout", Error: "workloadRef Deployment ns/missing: not found"}
	got := capWorkloadErrors(existing, extra, attunev1alpha1.MaxWorkloadErrors)
	require.Len(t, got, attunev1alpha1.MaxWorkloadErrors)
	assert.Equal(t, "nosel", got[0].Workload)
	assert.Equal(t, errNoPodSelector, got[0].Error)
	assert.Equal(t, "rollout", got[1].Workload)
	assert.Equal(t, "w0", got[2].Workload)
	assert.NotContains(t, workloadNames(got), "w8")

	dup := capWorkloadErrors(
		[]attunev1alpha1.WorkloadError{{Workload: "nosel", Error: "query failed"}},
		extra,
		attunev1alpha1.MaxWorkloadErrors,
	)
	require.Len(t, dup, 1)
	assert.Equal(t, errNoPodSelector, dup[0].Error)

	onlyExtra := make([]attunev1alpha1.WorkloadError, 12)
	for i := range onlyExtra {
		onlyExtra[i] = attunev1alpha1.WorkloadError{Workload: fmt.Sprintf("u%d", i), Error: errCouldNotListPods}
	}
	capped := capWorkloadErrors(existing, onlyExtra, attunev1alpha1.MaxWorkloadErrors)
	require.Len(t, capped, attunev1alpha1.MaxWorkloadErrors)
	assert.Equal(t, "u0", capped[0].Workload)
	assert.Equal(t, "u9", capped[9].Workload)

	assert.Nil(t, capWorkloadErrors(nil, nil, attunev1alpha1.MaxWorkloadErrors))
	assert.Nil(t, capWorkloadErrors(existing, extra, 0))

	sameName := capWorkloadErrors(
		[]attunev1alpha1.WorkloadError{{Workload: "rollout", Error: "workloadRef Deployment ns/missing: not found"}},
		[]attunev1alpha1.WorkloadError{{Workload: "rollout", Error: "pod selector: bad"}},
		attunev1alpha1.MaxWorkloadErrors,
	)
	require.Len(t, sameName, 1)
	assert.Equal(t, "workloadRef Deployment ns/missing: not found", sameName[0].Error)
}

func workloadNames(errs []attunev1alpha1.WorkloadError) []string {
	names := make([]string, len(errs))
	for i := range errs {
		names[i] = errs[i].Workload
	}
	return names
}

func TestListPodsForWorkloads_NSListErrorFallsBackPerWorkload(t *testing.T) {
	scheme := testScheme()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", Labels: map[string]string{"app": "app"}}}

	// Base client has objects; interceptor fails only the namespace-wide Pod list
	// (no MatchingLabels), then getPodsForWorkload succeeds with MatchingLabels.
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, pod).Build()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					// Fail only bare namespace lists (no MatchingLabels option).
					// Heuristic: if MatchingLabels is absent from opts, fail.
					hasMatch := false
					for _, o := range opts {
						if _, ok := o.(client.MatchingLabels); ok {
							hasMatch = true
							break
						}
						if _, ok := o.(client.MatchingLabelsSelector); ok {
							hasMatch = true
							break
						}
					}
					if !hasMatch {
						return fmt.Errorf("simulated ns list failure")
					}
				}
				return base.List(ctx, list, opts...)
			},
		}).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = c
	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{d})
	assert.Empty(t, failed)
	require.Len(t, out["app"], 1)
	assert.Equal(t, "p1", out["app"][0].Name)
}

func TestListPodsForWorkloads_BothListsFailMarksUnlisted(t *testing.T) {
	scheme := testScheme()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).Build()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return fmt.Errorf("simulated pod list failure")
				}
				return base.List(ctx, list, opts...)
			},
		}).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = c

	out, failed := r.listPodsForWorkloads(context.Background(), []client.Object{d})
	_, ok := out["app"]
	assert.False(t, ok, "a failed list must not look like zero pods")
	assert.Equal(t, errCouldNotListPods, failed["app"])
	assert.Equal(t, []string{"app"}, unlistedWorkloadNames(failed))
}

func TestMaxWorkloadWorkers(t *testing.T) {
	r := NewAttunePolicyReconciler()
	assert.Equal(t, defaultMaxWorkloadWorkers, r.maxWorkloadWorkers())
	r.MaxWorkloadWorkers = 8
	assert.Equal(t, 8, r.maxWorkloadWorkers())
	r.MaxWorkloadWorkers = 0
	assert.Equal(t, defaultMaxWorkloadWorkers, r.maxWorkloadWorkers())
}

func TestAbsInt64(t *testing.T) {
	assert.Equal(t, int64(0), absInt64(0))
	assert.Equal(t, int64(5), absInt64(5))
	assert.Equal(t, int64(5), absInt64(-5))
}
