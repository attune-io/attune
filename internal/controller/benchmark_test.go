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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func BenchmarkBuildPrometheusQuery_CPU(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("production", "api-server-7f8c9d", "web-container", "cpu", 5*time.Minute)
	}
}

func BenchmarkBuildPrometheusQuery_Memory(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("production", "api-server-7f8c9d", "web-container", "memory", 5*time.Minute)
	}
}

func BenchmarkBuildPrometheusQuery_SpecialChars(b *testing.B) {
	for b.Loop() {
		buildPrometheusQuery("my-ns.test", "pod+name.v2[0]", "container(1)", "cpu", 5*time.Minute)
	}
}

func BenchmarkReconcile(b *testing.B) {
	policy := newTestPolicy("bench-policy", "default")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api"})
	pod := newTestPod("api-server-abc", "default", map[string]string{"app": "api"})

	samples := generateSamples(2016, 0.2)
	mc := &mockCollector{
		queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
			return map[string][]rsmetrics.Sample{"main": samples}, nil
		},
		queryFunc: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			return 0.1, nil
		},
	}
	r, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "bench-policy", Namespace: "default"}}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = r.Reconcile(context.Background(), req)
	}
}

func BenchmarkComputeRecommendations(b *testing.B) {
	policy := newTestPolicy("bench-policy", "default")
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient()

	samples := generateSamples(2016, 0.2) // 7 days at 5-min step
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return samples, nil
		},
	}

	b.ResetTimer()
	for b.Loop() {
		_, _, _, _, _ = reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
	}
}

// BenchmarkReconcile_ManyWorkloads tests a single policy with a label selector
// matching N deployments. Exercises processWorkloads, conflict detection,
// recommendation computation, and status updates at scale.
func BenchmarkReconcile_ManyWorkloads(b *testing.B) {
	for _, n := range []int{10, 50, 100, 250} {
		b.Run(fmt.Sprintf("%d_workloads", n), func(b *testing.B) {
			objects := make([]client.Object, 0, 2*n+1)

			policy := &attunev1alpha1.AttunePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "scale-policy", Namespace: "bench"},
				Spec: attunev1alpha1.AttunePolicySpec{
					TargetRef: attunev1alpha1.TargetRef{
						Kind: "Deployment",
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"bench": "true"},
						},
					},
					MetricsSource: attunev1alpha1.MetricsSource{
						Prometheus:        &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
						MinimumDataPoints: int32Ptr(48),
					},
					CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
					Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
					UpdateStrategy: &attunev1alpha1.UpdateStrategy{
						Type: attunev1alpha1.UpdateTypeRecommend,
					},
				},
			}
			objects = append(objects, policy)

			for i := 0; i < n; i++ {
				name := fmt.Sprintf("deploy-%04d", i)
				labels := map[string]string{"bench": "true", "app": name}
				deploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "bench", Labels: labels},
					Spec: appsv1.DeploymentSpec{
						Replicas: int32Ptr(2),
						Selector: &metav1.LabelSelector{MatchLabels: labels},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: labels},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
									},
								}},
							},
						},
					},
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: name + "-abc", Namespace: "bench", Labels: labels,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				}
				objects = append(objects, deploy, pod)
			}

			samples := generateSamples(2016, 0.2)
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					return map[string][]rsmetrics.Sample{"main": samples}, nil
				},
				queryFunc: func(_ context.Context, _ string, _ time.Time) (float64, error) {
					return 0.1, nil
				},
			}

			scheme := testScheme()
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
				Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.MetricsFactory = mockMetricsFactory(mc)

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "scale-policy", Namespace: "bench"}}

			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				_, _ = r.Reconcile(context.Background(), req)
			}
		})
	}
}

// BenchmarkReconcile_ManyPolicies tests N independent policies, each targeting
// a different deployment. Measures per-policy overhead and shared state
// (gauge keys, collector cache) performance.
func BenchmarkReconcile_ManyPolicies(b *testing.B) {
	for _, n := range []int{10, 50, 100, 250} {
		b.Run(fmt.Sprintf("%d_policies", n), func(b *testing.B) {
			objects := make([]client.Object, 0, 3*n)
			reqs := make([]ctrl.Request, n)

			for i := 0; i < n; i++ {
				name := fmt.Sprintf("deploy-%04d", i)
				policyName := fmt.Sprintf("policy-%04d", i)
				labels := map[string]string{"app": name}

				objects = append(objects,
					&attunev1alpha1.AttunePolicy{
						ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: "bench"},
						Spec: attunev1alpha1.AttunePolicySpec{
							TargetRef: attunev1alpha1.TargetRef{
								Kind: "Deployment",
								Name: stringPtr(name),
							},
							MetricsSource: attunev1alpha1.MetricsSource{
								Prometheus:        &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
								MinimumDataPoints: int32Ptr(48),
							},
							CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
							Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
							UpdateStrategy: &attunev1alpha1.UpdateStrategy{
								Type: attunev1alpha1.UpdateTypeRecommend,
							},
						},
					},
					&appsv1.Deployment{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "bench", Labels: labels},
						Spec: appsv1.DeploymentSpec{
							Replicas: int32Ptr(1),
							Selector: &metav1.LabelSelector{MatchLabels: labels},
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{Labels: labels},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{
										Name: "main", Image: "nginx",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("200m"),
												corev1.ResourceMemory: resource.MustParse("256Mi"),
											},
										},
									}},
								},
							},
						},
					},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: name + "-abc", Namespace: "bench", Labels: labels,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
						},
						Status: corev1.PodStatus{Phase: corev1.PodRunning},
					},
				)
				reqs[i] = ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: "bench"}}
			}

			samples := generateSamples(2016, 0.15)
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					return map[string][]rsmetrics.Sample{"main": samples}, nil
				},
				queryFunc: func(_ context.Context, _ string, _ time.Time) (float64, error) {
					return 0.05, nil
				},
			}

			scheme := testScheme()
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
				Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.MetricsFactory = mockMetricsFactory(mc)

			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				for _, req := range reqs {
					_, _ = r.Reconcile(context.Background(), req)
				}
			}
		})
	}
}

// BenchmarkReconcile_ConcurrentPolicies tests parallel reconciliation of N
// policies, simulating MaxConcurrentReconciles > 1. Exercises thread safety
// of shared state (gauge keys, collector cache, operator metrics).
func BenchmarkReconcile_ConcurrentPolicies(b *testing.B) {
	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("%d_parallel", n), func(b *testing.B) {
			objects := make([]client.Object, 0, 3*n)
			reqs := make([]ctrl.Request, n)

			for i := 0; i < n; i++ {
				name := fmt.Sprintf("deploy-%04d", i)
				policyName := fmt.Sprintf("policy-%04d", i)
				labels := map[string]string{"app": name}

				objects = append(objects,
					&attunev1alpha1.AttunePolicy{
						ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: "bench"},
						Spec: attunev1alpha1.AttunePolicySpec{
							TargetRef: attunev1alpha1.TargetRef{
								Kind: "Deployment",
								Name: stringPtr(name),
							},
							MetricsSource: attunev1alpha1.MetricsSource{
								Prometheus:        &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"},
								MinimumDataPoints: int32Ptr(48),
							},
							CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
							Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
							UpdateStrategy: &attunev1alpha1.UpdateStrategy{
								Type: attunev1alpha1.UpdateTypeRecommend,
							},
						},
					},
					&appsv1.Deployment{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "bench", Labels: labels},
						Spec: appsv1.DeploymentSpec{
							Replicas: int32Ptr(1),
							Selector: &metav1.LabelSelector{MatchLabels: labels},
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{Labels: labels},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{
										Name: "main", Image: "nginx",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("200m"),
												corev1.ResourceMemory: resource.MustParse("256Mi"),
											},
										},
									}},
								},
							},
						},
					},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: name + "-abc", Namespace: "bench", Labels: labels,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
						},
						Status: corev1.PodStatus{Phase: corev1.PodRunning},
					},
				)
				reqs[i] = ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: "bench"}}
			}

			samples := generateSamples(2016, 0.15)
			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					return map[string][]rsmetrics.Sample{"main": samples}, nil
				},
				queryFunc: func(_ context.Context, _ string, _ time.Time) (float64, error) {
					return 0.05, nil
				},
			}

			scheme := testScheme()
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
				Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.MetricsFactory = mockMetricsFactory(mc)

			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					_, _ = r.Reconcile(context.Background(), reqs[i%n])
					i++
				}
			})
		})
	}
}

// BenchmarkComputeRecommendations_ManyContainers tests recommendation
// computation for pods with varying container counts (sidecars, init
// containers). Exercises per-container Prometheus queries and the
// recommendation chain.
func BenchmarkComputeRecommendations_ManyContainers(b *testing.B) {
	for _, nc := range []int{1, 3, 10} {
		b.Run(fmt.Sprintf("%d_containers", nc), func(b *testing.B) {
			containers := make([]corev1.Container, nc)
			samplesByContainer := make(map[string][]rsmetrics.Sample, nc)
			samples := generateSamples(2016, 0.2)
			for i := 0; i < nc; i++ {
				name := fmt.Sprintf("container-%d", i)
				containers[i] = corev1.Container{
					Name:  name,
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				}
				samplesByContainer[name] = samples
			}

			policy := newTestPolicy("bench-policy", "default")
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-container", Namespace: "default"},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: containers},
					},
				},
			}
			reconciler := newReconcilerWithClient()

			mc := &mockCollector{
				queryRangeGroupedFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (map[string][]rsmetrics.Sample, error) {
					return samplesByContainer, nil
				},
			}

			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				_, _, _, _, _ = reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil)
			}
		})
	}
}
