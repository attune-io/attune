/*
Copyright 2026.
*/

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

	out := r.listPodsForWorkloads(context.Background(), []client.Object{d1, d2})
	require.Len(t, out["app1"], 2)
	require.Len(t, out["app2"], 1)
	assert.Equal(t, "p2", out["app2"][0].Name)
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
	// Sampled regex uses exact names joined by |, not the full deployment pattern.
	assert.Contains(t, re, "|")
	assert.NotEqual(t, r.getPodRegex(d), re)
	// Unlimited path keeps workload regex.
	r.MaxPodsInMetricsQuery = -1
	assert.Equal(t, r.getPodRegex(d), r.metricsPodRegex(d, pods))
}
