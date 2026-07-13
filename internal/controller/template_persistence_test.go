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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func boolPtrTP(v bool) *bool { return &v }

func TestTemplatePersistenceEnabled(t *testing.T) {
	assert.False(t, templatePersistenceEnabled(nil))
	assert.False(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{}))
	assert.False(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{},
	}))
	assert.True(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{Enabled: boolPtrTP(true)},
	}))
}

func TestTemplatePersistenceWhenDefault(t *testing.T) {
	assert.Equal(t, attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, templatePersistenceWhen(nil))
	assert.Equal(t, attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, templatePersistenceWhen(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{Enabled: boolPtrTP(true)},
	}))
	assert.Equal(t, attunev1alpha1.TemplatePersistenceOnRecommendation, templatePersistenceWhen(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{
			Enabled: boolPtrTP(true),
			When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
		},
	}))
}

func TestMaterializeContainerResources_AllowDecrease(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	falseVal := false
	policy.Spec.Memory.AllowDecrease = &falseVal
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// CPU allowDecrease default true → decrease OK; memory false → keep current
	got := materializeContainerResources(policy, c)
	assert.Equal(t, int64(200), got.Requests.Cpu().MilliValue())
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("512Mi")))
}

func TestMaterializeContainerResources_RequestsAndLimits(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("300m"),
			CPULimit:      resource.MustParse("600m"),
			MemoryRequest: resource.MustParse("400Mi"),
			MemoryLimit:   resource.MustParse("800Mi"),
		},
	}
	got := materializeContainerResources(policy, c)
	require.NotNil(t, got.Limits)
	assert.Equal(t, int64(600), got.Limits.Cpu().MilliValue())
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("800Mi")))
}

func TestApplyResourcesToPodSpec_NoOpWhenEqual(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		}},
	}
	desired := map[string]corev1.ResourceRequirements{
		"app": {
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("300m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	assert.False(t, applyResourcesToPodSpec(spec, desired))
}

func TestApplyResourcesToPodSpec_UpdatesContainer(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
		}},
	}
	desired := map[string]corev1.ResourceRequirements{
		"app": {
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	assert.True(t, applyResourcesToPodSpec(spec, desired))
	assert.Equal(t, int64(200), spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

func TestApplyTemplatePersistence_OnRecommendation_Deployment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
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
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtrTP(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}

	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("512Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("200m"),
				MemoryRequest: resource.MustParse("256Mi"),
			},
		}},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

func TestApplyTemplatePersistence_AfterSuccessfulResize_OnlyResizedWorkloads(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
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
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtrTP(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("512Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("200m"),
				MemoryRequest: resource.MustParse("256Mi"),
			},
		}},
	}}

	// Wrong mode / empty onlyWorkloads → no patch
	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{})
	assert.Empty(t, history)

	history = r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)
}

func TestApplyTemplatePersistence_StatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "db",
						Image: "postgres",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{Replicas: 1, UpdatedReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtrTP(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "db",
		Kind:     "StatefulSet",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "db",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("1"),
				MemoryRequest: resource.MustParse("1Gi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("512Mi"),
			},
		}},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{sts}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	require.Len(t, history, 1)

	var updated appsv1.StatefulSet
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(sts), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

func TestApplyTemplatePersistence_SkipsExcludedContainers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
							},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtrTP(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	// only sidecar differs — should no-op entire patch if app has no change
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name: "app",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("500m"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("500m"),
				},
			},
			{
				Name: "istio-proxy",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("100m"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("50m"),
				},
			},
		},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	assert.Empty(t, history, "only known sidecar would change; should not patch")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(100), updated.Spec.Template.Spec.Containers[1].Resources.Requests.Cpu().MilliValue())
}

func TestSuccessfulResizeWorkloads(t *testing.T) {
	got := successfulResizeWorkloads([]attunev1alpha1.ResizeHistoryEntry{
		{Workload: "a", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess},
		{Workload: "b", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed},
		{Workload: "c", Method: "TemplatePersistence", Result: attunev1alpha1.ResizeResultTemplatePatched},
	})
	assert.True(t, got["a"])
	assert.False(t, got["b"])
	assert.False(t, got["c"])
}
