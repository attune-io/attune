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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/safety"
)

func TestTemplatePersistenceEnabled(t *testing.T) {
	assert.False(t, templatePersistenceEnabled(nil))
	assert.False(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{}))
	assert.False(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{},
	}))
	assert.True(t, templatePersistenceEnabled(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{Enabled: boolPtr(true)},
	}))
}

func TestTemplatePersistenceWhenDefault(t *testing.T) {
	assert.Equal(t, attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, templatePersistenceWhen(nil))
	assert.Equal(t, attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, templatePersistenceWhen(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{Enabled: boolPtr(true)},
	}))
	assert.Equal(t, attunev1alpha1.TemplatePersistenceOnRecommendation, templatePersistenceWhen(&attunev1alpha1.UpdateStrategy{
		TemplatePersistence: &attunev1alpha1.TemplatePersistence{
			Enabled: boolPtr(true),
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
	assert.False(t, applyResourcesToPodSpec(spec, desired, false))
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
	assert.True(t, applyResourcesToPodSpec(spec, desired, false))
	assert.Equal(t, int64(200), spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

func TestApplyResourcesToPodSpec_NativeSidecarInitContainer(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}},
		InitContainers: []corev1.Container{
			{
				Name:          "mesh",
				RestartPolicy: &always,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				},
			},
			{
				// Regular init (no Always restart) must not be patched.
				Name: "setup",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
				},
			},
		},
	}
	desired := map[string]corev1.ResourceRequirements{
		"mesh": {
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("80m")},
		},
		"setup": {
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("99m")},
		},
	}
	assert.True(t, applyResourcesToPodSpec(spec, desired, false))
	assert.Equal(t, int64(80), spec.InitContainers[0].Resources.Requests.Cpu().MilliValue())
	assert.Equal(t, int64(10), spec.InitContainers[1].Resources.Requests.Cpu().MilliValue(),
		"non-native init container must not be modified")
}

func TestApplyResourcesToPodSpec_RequestsOnlyKeepsLimitsAndNoOps(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		}},
		InitContainers: []corev1.Container{{
			Name:          "mesh",
			RestartPolicy: &always,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
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
		"mesh": {
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	assert.False(t, applyResourcesToPodSpec(spec, desired, false),
		"RequestsOnly with matching requests must not treat leftover limits as a change")
	assert.Equal(t, int64(200), spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("256Mi")))
	require.NotNil(t, spec.Containers[0].Resources.Limits)
	assert.Equal(t, int64(1000), spec.Containers[0].Resources.Limits.Cpu().MilliValue())
	assert.True(t, spec.Containers[0].Resources.Limits.Memory().Equal(resource.MustParse("1Gi")))
	require.NotNil(t, spec.InitContainers[0].Resources.Limits)
	assert.Equal(t, int64(1000), spec.InitContainers[0].Resources.Limits.Cpu().MilliValue())
	assert.True(t, spec.InitContainers[0].Resources.Limits.Memory().Equal(resource.MustParse("1Gi")))
}

func TestApplyResourcesToPodSpec_RequestsOnlyUpdatesRequestsKeepsLimits(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
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

	assert.True(t, applyResourcesToPodSpec(spec, desired, false))
	assert.Equal(t, int64(200), spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("256Mi")))
	require.NotNil(t, spec.Containers[0].Resources.Limits)
	assert.Equal(t, int64(1000), spec.Containers[0].Resources.Limits.Cpu().MilliValue())
	assert.True(t, spec.Containers[0].Resources.Limits.Memory().Equal(resource.MustParse("1Gi")))
}

func TestMergeTemplateResources_PreservesUncontrolledLimits(t *testing.T) {
	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	// RequestsOnly payload: requests only.
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	got := mergeTemplateResources(current, want)
	assert.Equal(t, int64(200), got.Requests.Cpu().MilliValue())
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("256Mi")))
	require.NotNil(t, got.Limits)
	assert.Equal(t, int64(500), got.Limits.Cpu().MilliValue(), "existing CPU limit preserved")
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("512Mi")), "existing memory limit preserved")
}

func TestQuantityEqual_MissingAsZero(t *testing.T) {
	a := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}
	b := corev1.ResourceList{}
	assert.True(t, quantityEqual(a, b, corev1.ResourceCPU))
	assert.True(t, quantityEqual(b, a, corev1.ResourceCPU))
	assert.False(t, quantityEqual(
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		b,
		corev1.ResourceCPU,
	))
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
		Enabled: boolPtr(true),
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
	assert.Equal(t, "template", history[0].Resource)
	assert.Equal(t, "TemplatePersistence", history[0].Method)
	assert.Equal(t, "workload-template", history[0].From)
	assert.Equal(t, "recommended", history[0].To)

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
		Enabled: boolPtr(true),
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

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"default AllowDecrease=false keeps template memory 512Mi")
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
		Enabled: boolPtr(true),
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
		Enabled: boolPtr(true),
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
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"app container should remain untouched")
	assert.Equal(t, int64(100), updated.Spec.Template.Spec.Containers[1].Resources.Requests.Cpu().MilliValue(),
		"known sidecar should remain untouched")
}

func TestSuccessfulResizeWorkloads(t *testing.T) {
	got := successfulResizeWorkloads([]attunev1alpha1.ResizeHistoryEntry{
		{Workload: "a", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess},
		{Workload: "b", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed},
		{Workload: "c", Method: "TemplatePersistence", Result: attunev1alpha1.ResizeResultTemplatePatched},
		{Workload: "d", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted},
	})
	assert.True(t, got["a"])
	assert.False(t, got["b"])
	assert.False(t, got["c"])
	assert.True(t, got["d"], "Evicted InPlaceOrRecreate must trigger AfterSuccessfulResize persist")
}

func TestApplyTemplatePersistence_AfterSuccessfulResize_EvictedGetsTemplate(t *testing.T) {
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
		Enabled: boolPtr(true),
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
	only := successfulResizeWorkloads([]attunev1alpha1.ResizeHistoryEntry{{
		Workload: "api",
		Method:   "Eviction",
		Result:   attunev1alpha1.ResizeResultEvicted,
	}})

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, only)
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"default AllowDecrease=false keeps template memory 512Mi")
}

func TestApplyTemplatePersistence_AfterSuccessfulResize_EvictedMissingDeploymentFailed(t *testing.T) {
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
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
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
	only := successfulResizeWorkloads([]attunev1alpha1.ResizeHistoryEntry{{
		Workload: "api",
		Method:   "Eviction",
		Result:   attunev1alpha1.ResizeResultEvicted,
	}})

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, only)
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, history[0].Result)
}

func TestLaggingAfterResizeWorkloads(t *testing.T) {
	now := metav1.Now()
	t0 := metav1.NewTime(now.Add(-2 * time.Hour))
	t1 := metav1.NewTime(now.Add(-1 * time.Hour))
	t2 := now

	got := laggingAfterResizeWorkloads(
		[]attunev1alpha1.ResizeHistoryEntry{
			{Workload: "cycle", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess},
		},
		[]attunev1alpha1.ResizeHistoryEntry{
			// prior: success without later TemplatePatched → lagging
			{Workload: "prior", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: t0},
			// failed resize alone does not lag
			{Workload: "failed", Method: "InPlace", Result: attunev1alpha1.ResizeResultFailed, Timestamp: t0},
			// done: success then TemplatePatched → no lag
			{Workload: "done", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: t0},
			{Workload: "done", Method: "TemplatePersistence", Result: attunev1alpha1.ResizeResultTemplatePatched, Timestamp: t1},
			// retry: success after a prior TemplatePatched (new resize) → lag again
			{Workload: "retry", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: t0},
			{Workload: "retry", Method: "TemplatePersistence", Result: attunev1alpha1.ResizeResultTemplatePatched, Timestamp: t1},
			{Workload: "retry", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: t2},
			// prior Evicted without later TemplatePatched → lagging
			{Workload: "evicted-prior", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: t0},
			// Evicted then TemplatePatched → not lagging
			{Workload: "evicted-done", Method: "Eviction", Result: attunev1alpha1.ResizeResultEvicted, Timestamp: t0},
			{Workload: "evicted-done", Method: "TemplatePersistence", Result: attunev1alpha1.ResizeResultTemplatePatched, Timestamp: t1},
		},
	)
	assert.True(t, got["cycle"])
	assert.True(t, got["prior"])
	assert.False(t, got["failed"])
	assert.False(t, got["done"])
	assert.True(t, got["retry"])
	assert.True(t, got["evicted-prior"], "Evicted without later TemplatePatched must lag")
	assert.False(t, got["evicted-done"], "Evicted then TemplatePatched must not lag")
}

func TestCanaryBlocksTemplatePersistence(t *testing.T) {
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	assert.False(t, canaryBlocksTemplatePersistence(policy))

	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	assert.True(t, canaryBlocksTemplatePersistence(policy), "nil canary status blocks")

	policy.Status.Canary = &attunev1alpha1.CanaryStatus{Phase: attunev1alpha1.CanaryPhaseInProgress}
	assert.True(t, canaryBlocksTemplatePersistence(policy))

	policy.Status.Canary.Phase = attunev1alpha1.CanaryPhaseFullRollout
	assert.False(t, canaryBlocksTemplatePersistence(policy))
}

func TestMaterializeContainerResources_ClampsRequestsOnlyDefault(t *testing.T) {
	// Default ControlledValues is RequestsOnly. Recommended limits carry the
	// current limits (resize path parity) so growing requests clamp to them.
	policy := &attunev1alpha1.AttunePolicy{}
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
			MemoryLimit:   resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"), // > limit 200m
			CPULimit:      resource.MustParse("200m"), // current limit (RequestsOnly)
			MemoryRequest: resource.MustParse("512Mi"),
			MemoryLimit:   resource.MustParse("256Mi"),
		},
	}
	got := materializeContainerResources(policy, c)
	assert.Equal(t, int64(200), got.Requests.Cpu().MilliValue(), "CPU request clamped to current limit")
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("256Mi")), "memory request clamped to current limit")
	assert.Nil(t, got.Limits, "RequestsOnly must not write limits into the template payload")
}

func TestMaterializeContainerResources_MemoryUsageFloor(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.Memory.ControlledValues = &cv
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("64Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("64Mi"),
			MemoryLimit:   resource.MustParse("64Mi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("200Mi"),
			},
		},
	}
	got := materializeContainerResources(policy, c)
	require.NotNil(t, got.Limits)
	gotLim := got.Limits[corev1.ResourceMemory]
	assert.True(t, gotLim.Cmp(resource.MustParse("64Mi")) > 0,
		"floored limit must exceed raw recommended 64Mi, got %s", gotLim.String())
	// Default 10% margin: 200Mi * 1.10 = 220Mi
	assert.True(t, gotLim.Cmp(resource.MustParse("220Mi")) >= 0,
		"limit %s must be >= usage floor 220Mi", gotLim.String())

	reqOnly := &attunev1alpha1.AttunePolicy{}
	reqOnly.Spec.Memory.AllowDecrease = &memDec
	gotRO := materializeContainerResources(reqOnly, c)
	assert.Nil(t, gotRO.Limits, "RequestsOnly must still leave Limits nil")
}

func TestMaterializeContainerResources_MemoryUsageFloor_GuaranteedRaisesRequest(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("1Gi"),
			MemoryLimit:   resource.MustParse("1Gi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			CPULimit:      resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("200Mi"),
			MemoryLimit:   resource.MustParse("200Mi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("500Mi"),
			},
		},
	}
	got := materializeContainerResources(policy, c)
	require.NotNil(t, got.Limits)
	want := resource.MustParse("550Mi")
	gotLim := got.Limits[corev1.ResourceMemory]
	gotReq := got.Requests[corev1.ResourceMemory]
	assert.True(t, gotLim.Equal(want), "limit %s want %s", gotLim.String(), want.String())
	assert.True(t, gotReq.Equal(want),
		"Guaranteed request %s want %s (not 200/550 Burstable)",
		gotReq.String(), want.String())

	reqOnly := &attunev1alpha1.AttunePolicy{}
	reqOnly.Spec.Memory.AllowDecrease = &memDec
	gotRO := materializeContainerResources(reqOnly, c)
	assert.Nil(t, gotRO.Limits, "RequestsOnly must still leave Limits nil")
	gotROReq := gotRO.Requests[corev1.ResourceMemory]
	assert.True(t, gotROReq.Equal(resource.MustParse("200Mi")),
		"RequestsOnly must not raise request without a memory limit, got %s",
		gotROReq.String())
}

func TestMaterializeContainerResources_MemoryUsageFloor_ZeroMargin(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.Memory.ControlledValues = &cv
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	zeroMargin := int32(0)
	policy.Spec.Memory.DecreaseUsageMarginPercent = &zeroMargin
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("64Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("64Mi"),
			MemoryLimit:   resource.MustParse("64Mi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("200Mi"),
			},
		},
	}
	got := materializeContainerResources(policy, c)
	require.NotNil(t, got.Limits)
	gotLim := got.Limits[corev1.ResourceMemory]
	usage := resource.MustParse("200Mi")
	want := *resource.NewQuantity(usage.Value()+1, resource.BinarySI)
	assert.True(t, gotLim.Equal(want), "zero-margin floor must be usage+1 byte, got %s want %s", gotLim.String(), want.String())

	reqOnly := &attunev1alpha1.AttunePolicy{}
	reqOnly.Spec.Memory.AllowDecrease = &memDec
	reqOnly.Spec.Memory.DecreaseUsageMarginPercent = &zeroMargin
	gotRO := materializeContainerResources(reqOnly, c)
	assert.Nil(t, gotRO.Limits, "RequestsOnly must still leave Limits nil")
}

func TestMaterializeContainerResources_FloorsAgainstUsageWhenNotDecreaseVsCurrent(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.Memory.ControlledValues = &cv
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryLimit: resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryLimit: resource.MustParse("1Gi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("1536Mi"),
			},
		},
	}
	got := materializeContainerResources(policy, c)
	require.NotNil(t, got.Limits)
	gotLim := got.Limits[corev1.ResourceMemory]
	// Recommended 1Gi is an increase vs stale Current 512Mi, but still
	// below usage 1.5Gi + 10%. Persist must not write 1Gi.
	assert.True(t, gotLim.Cmp(resource.MustParse("1Gi")) > 0,
		"limit %s must exceed recommended 1Gi", gotLim.String())
	usage := resource.MustParse("1536Mi")
	want := *resource.NewQuantity(int64(math.Ceil(float64(usage.Value())*1.1)), resource.BinarySI)
	assert.True(t, gotLim.Equal(want), "got %s want %s (1536Mi * 1.1)", gotLim.String(), want.String())

	reqOnly := &attunev1alpha1.AttunePolicy{}
	reqOnly.Spec.Memory.AllowDecrease = &memDec
	gotRO := materializeContainerResources(reqOnly, c)
	assert.Nil(t, gotRO.Limits, "RequestsOnly must not invent limits")
}

func TestMaterializeContainerResources_ClampsRequestsAndLimits(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	c := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("800m"),
			CPULimit:      resource.MustParse("400m"),
			MemoryRequest: resource.MustParse("1Gi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}
	got := materializeContainerResources(policy, c)
	assert.Equal(t, int64(400), got.Requests.Cpu().MilliValue(), "CPU request clamped to limit")
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("512Mi")), "memory request clamped to limit")
	require.NotNil(t, got.Limits)
	assert.Equal(t, int64(400), got.Limits.Cpu().MilliValue())
}

func TestApplyTemplatePersistence_SkipsObserveMode(t *testing.T) {
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
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
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
	assert.Empty(t, history, "Observe mode must not patch templates")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

func TestApplyTemplatePersistence_SkipsCanaryInProgress(t *testing.T) {
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
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{Phase: attunev1alpha1.CanaryPhaseInProgress}
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
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

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	assert.Empty(t, history, "canary InProgress must not patch templates")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"canary InProgress must leave template CPU request unchanged")
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"canary InProgress must leave template memory request unchanged")

	// FullRollout allows patch.
	policy.Status.Canary.Phase = attunev1alpha1.CanaryPhaseFullRollout
	history = r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"default AllowDecrease=false keeps template memory 512Mi")
}

func TestApplyTemplatePersistence_SkipsMidRollout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
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
		// Mid-rollout: not all replicas updated.
		Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
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
	assert.Empty(t, history, "mid-rollout must not patch")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"mid-rollout must leave template CPU request unchanged")
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"mid-rollout must leave template memory request unchanged")
}

func TestApplyTemplatePersistence_SkipsStale(t *testing.T) {
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
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Stale:    true,
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
	assert.Empty(t, history, "stale rec must not patch the workload template")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"stale rec must leave template CPU request unchanged")
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"stale rec must leave template memory request unchanged")
}

func TestApplyTemplatePersistence_SkipsStale_AfterSuccessfulResize(t *testing.T) {
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
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Stale:    true,
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
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	assert.Empty(t, history, "stale rec must not patch after a successful resize")

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"stale rec must leave template CPU request unchanged")
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"stale rec must leave template memory request unchanged")
}

func TestApplyTemplatePersistence_PatchesTemplateWhenRecCurrentEqualsRecommended(t *testing.T) {
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
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	// holdMissing copies live/prior into both Current and Recommended.
	// Rec equality must not skip persist when the template is still stale.
	held := attunev1alpha1.ResourceValues{
		CPURequest:    resource.MustParse("200m"),
		MemoryRequest: resource.MustParse("256Mi"),
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "app",
			Current:     held,
			Recommended: held,
		}},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("256Mi")))
}

func TestApplyTemplatePersistence_NoOpWhenTemplateMatches(t *testing.T) {
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
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	counter := &getCountingReader{Reader: cl}
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	r.APIReader = counter

	policy := newTestPolicy("p", "default")
	// Allow memory decrease so materialize keeps recommended 256Mi (matches template).
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	// Recommendation differs from "current" (live pod) but template already has desired.
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
	assert.Empty(t, history, "template already matches desired; no history entry")
	assert.Equal(t, 0, counter.n, "cached template match must skip the live workload Get")
}

type getCountingReader struct {
	client.Reader
	n int
}

func (g *getCountingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	g.n++
	return g.Reader.Get(ctx, key, obj, opts...)
}

func TestApplyTemplatePersistence_RequestsOnlyPreservesTemplateLimits(t *testing.T) {
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
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
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
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
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

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, map[string]bool{"api": true})
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	res := updated.Spec.Template.Spec.Containers[0].Resources
	assert.Equal(t, int64(200), res.Requests.Cpu().MilliValue())
	assert.True(t, res.Requests.Memory().Equal(resource.MustParse("256Mi")))
	require.NotNil(t, res.Limits)
	assert.Equal(t, int64(1000), res.Limits.Cpu().MilliValue())
	assert.True(t, res.Limits.Memory().Equal(resource.MustParse("1Gi")))
}

func TestApplyTemplatePersistence_DisabledByDefault(t *testing.T) {
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
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
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
	// TemplatePersistence nil / disabled
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Current: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("500m"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("200m"),
			},
		}},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	assert.Empty(t, history)

	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(false),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	history = r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	assert.Empty(t, history)
}

func TestMergeTemplateResources_NilRequestsAndLimits(t *testing.T) {
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	got := mergeTemplateResources(corev1.ResourceRequirements{}, want)
	require.NotNil(t, got.Requests)
	assert.Equal(t, "100m", got.Requests.Cpu().String())
	assert.Equal(t, "128Mi", got.Requests.Memory().String())
	assert.Nil(t, got.Limits)

	// Limits filled only when want has limits; existing limits preserved for other keys.
	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	want2 := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
	}
	got2 := mergeTemplateResources(current, want2)
	assert.Equal(t, "200m", got2.Requests.Cpu().String())
	assert.Equal(t, "400m", got2.Limits.Cpu().String())
	assert.Equal(t, "256Mi", got2.Limits.Memory().String(), "unrelated existing limit kept")
}

func TestWorkloadKindName(t *testing.T) {
	assert.Equal(t, "Deployment", workloadKindName(&appsv1.Deployment{}))
	assert.Equal(t, "StatefulSet", workloadKindName(&appsv1.StatefulSet{}))
	assert.Equal(t, "DaemonSet", workloadKindName(&appsv1.DaemonSet{}))
	assert.Equal(t, "Job", workloadKindName(&batchv1.Job{}))
	assert.Equal(t, "CronJob", workloadKindName(&batchv1.CronJob{}))
	// Unknown object falls back to GVK kind (empty when unset).
	assert.Equal(t, "", workloadKindName(&corev1.Pod{}))
}

func persistAtRec64MiDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
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
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
}

func original256MiRecord() safety.ResizeRecord {
	return safety.ResizeRecord{
		PodName:      "api-abc",
		Namespace:    "default",
		Container:    "app",
		WorkloadName: "api",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
}

func TestRestoreTemplateAfterSafetyRevert_AfterSuccessfulResize(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := persistAtRec64MiDeployment()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	err := r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{deploy}, original256MiRecord())
	require.NoError(t, err)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"template memory request must restore to the pre-resize snapshot")
}

func TestRestoreTemplateAfterSafetyRevert_ClearsPersistAddedLimits(t *testing.T) {
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
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
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
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	record := safety.ResizeRecord{
		PodName:      "api-abc",
		Namespace:    "default",
		Container:    "app",
		WorkloadName: "api",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
	err := r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{deploy}, record)
	require.NoError(t, err)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	got := updated.Spec.Template.Spec.Containers[0].Resources
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"template memory request must restore to the pre-resize snapshot")
	_, hasMemLimit := got.Limits[corev1.ResourceMemory]
	assert.False(t, hasMemLimit, "restore must clear persist-added memory limit")
}

func TestRestoreTemplateAfterSafetyRevert_PreservesExtendedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	gpu := corev1.ResourceName("nvidia.com/gpu")
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
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
								gpu:                   resource.MustParse("1"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("64Mi"),
								gpu:                   resource.MustParse("1"),
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
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	err := r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{deploy}, original256MiRecord())
	require.NoError(t, err)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	got := updated.Spec.Template.Spec.Containers[0].Resources
	assertFullResources(t, got, corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
			gpu:                   resource.MustParse("1"),
		},
		Limits: corev1.ResourceList{
			gpu: resource.MustParse("1"),
		},
	}, "restore must match the full resource set, not only cpu/memory")
}

func TestRestoreTemplateAfterSafetyRevert_DisabledNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := persistAtRec64MiDeployment()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(false),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}

	err := r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{deploy}, original256MiRecord())
	require.NoError(t, err)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.True(t, updated.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("64Mi")),
		"disabled persist must leave the template at the recommended size")
}

func TestOmitRevertedOrFailedContainers(t *testing.T) {
	t.Parallel()

	two := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "app"},
			{Name: "worker"},
		},
	}}
	otherWL := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload:   "api",
			Containers: []attunev1alpha1.ContainerRecommendation{{Name: "app"}},
		},
		{
			Workload:   "web",
			Containers: []attunev1alpha1.ContainerRecommendation{{Name: "app"}},
		},
	}

	tests := []struct {
		name    string
		recs    []attunev1alpha1.WorkloadRecommendation
		history []attunev1alpha1.ResizeHistoryEntry
		want    [][]string
	}{
		{
			name: "success plus reverted drops reverted container",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultReverted},
			},
			want: [][]string{{"app"}},
		},
		{
			name: "failed container is omitted",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultFailed},
			},
			want: [][]string{{"app"}},
		},
		{
			name: "all containers reverted omits the rec",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultReverted},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultFailed},
			},
			want: nil,
		},
		{
			name:    "empty history returns recs unchanged",
			recs:    two,
			history: nil,
			want:    [][]string{{"app", "worker"}},
		},
		{
			name: "revert on other workload leaves this rec",
			recs: otherWL,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "web", Container: "app", Result: attunev1alpha1.ResizeResultReverted},
			},
			want: [][]string{{"app"}},
		},
		{
			name: "wildcard container is ignored",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "*", Result: attunev1alpha1.ResizeResultFailed},
			},
			want: [][]string{{"app", "worker"}},
		},
		{
			name: "old reverted then later success keeps container",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultReverted},
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultSuccess},
			},
			want: [][]string{{"app", "worker"}},
		},
		{
			name: "success then later reverted omits container",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultReverted},
			},
			want: [][]string{{"app"}},
		},
		{
			name: "template patched does not hide later reverted",
			recs: two,
			history: []attunev1alpha1.ResizeHistoryEntry{
				{Workload: "api", Container: "app", Result: attunev1alpha1.ResizeResultSuccess},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultSuccess},
				{
					Workload:  "api",
					Container: "worker",
					Resource:  "template",
					Method:    "TemplatePersistence",
					Result:    attunev1alpha1.ResizeResultTemplatePatched,
				},
				{Workload: "api", Container: "worker", Result: attunev1alpha1.ResizeResultReverted},
				{
					Workload:  "api",
					Container: "worker",
					Resource:  "template",
					Method:    "TemplatePersistence",
					Result:    attunev1alpha1.ResizeResultTemplatePatched,
				},
			},
			want: [][]string{{"app"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := omitRevertedOrFailedContainers(tt.recs, tt.history)
			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, len(tt.want))
			for i, names := range tt.want {
				var gotNames []string
				for _, c := range got[i].Containers {
					gotNames = append(gotNames, c.Name)
				}
				assert.Equal(t, names, gotNames)
			}
		})
	}
}

func TestApplyTemplatePersistence_AfterSuccessfulResize_OmitsRevertedContainer(t *testing.T) {
	t.Parallel()

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
							Name:  "worker",
							Image: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
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
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
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
					CPURequest: resource.MustParse("200m"),
				},
			},
			{
				Name: "worker",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("400m"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("100m"),
				},
			},
		},
	}}
	cycle := []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "api", Container: "app", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess},
		{Workload: "api", Container: "worker", Method: "InPlace", Result: attunev1alpha1.ResizeResultReverted},
	}
	filtered := omitRevertedOrFailedContainers(recs, cycle)
	only := successfulResizeWorkloads(cycle)

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, filtered,
		attunev1alpha1.TemplatePersistenceAfterSuccessfulResize, only)
	require.Len(t, history, 1)
	assert.Equal(t, attunev1alpha1.ResizeResultTemplatePatched, history[0].Result)

	var updated appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"successful container A must persist")
	assert.Equal(t, int64(400), updated.Spec.Template.Spec.Containers[1].Resources.Requests.Cpu().MilliValue(),
		"reverted container B must stay at the original template request")
}
