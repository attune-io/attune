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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func newVPAUnstructured(name, namespace string, containerRecs []map[string]interface{}) *unstructured.Unstructured {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "autoscaling.k8s.io",
		Version: "v1",
		Kind:    "VerticalPodAutoscaler",
	})
	vpa.SetName(name)
	vpa.SetNamespace(namespace)
	if containerRecs != nil {
		recsSlice := make([]interface{}, len(containerRecs))
		for i, r := range containerRecs {
			recsSlice[i] = r
		}
		_ = unstructured.SetNestedSlice(vpa.Object, recsSlice, "status", "recommendation", "containerRecommendations")
	}
	return vpa
}

func TestComputeVPARecommendationsForWorkload_Basic(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "my-vpa"}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	vpaRecs := []rsmetrics.VPAContainerRecommendation{
		{
			ContainerName: "main",
			CPUTarget:     resource.MustParse("250m"),
			MemoryTarget:  resource.MustParse("512Mi"),
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(deploy).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: testScheme(),
	}

	rec, maxDP, err := reconciler.computeVPARecommendationsForWorkload(
		context.Background(), policy, deploy, vpaRecs, nil, nil, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 1, maxDP)
	require.Len(t, rec.Containers, 1)

	cRec := rec.Containers[0]
	assert.Equal(t, "main", cRec.Name)
	assert.Equal(t, float64(1.0), cRec.Confidence)
	assert.Equal(t, int32(2), cRec.DataPoints) // 1 CPU + 1 memory
	assert.NotNil(t, cRec.Explanation)
	assert.Contains(t, cRec.Explanation.CPU.FinalAdjustment, "source: VPA")
	assert.Contains(t, cRec.Explanation.Memory.FinalAdjustment, "source: VPA")
}

func TestComputeVPARecommendationsForWorkload_ExcludesContainer(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "my-vpa"}
	policy.Spec.ExcludedContainers = []string{"main"}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	vpaRecs := []rsmetrics.VPAContainerRecommendation{
		{
			ContainerName: "main",
			CPUTarget:     resource.MustParse("250m"),
			MemoryTarget:  resource.MustParse("512Mi"),
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(deploy).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: testScheme(),
	}

	rec, _, err := reconciler.computeVPARecommendationsForWorkload(
		context.Background(), policy, deploy, vpaRecs, nil, nil, nil,
	)
	require.NoError(t, err)
	assert.Nil(t, rec, "excluded container should result in no recommendation")
}

func TestComputeVPARecommendationsForWorkload_NoMatchingContainer(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "my-vpa"}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	// VPA has recommendation for "web", but deployment has container "main".
	vpaRecs := []rsmetrics.VPAContainerRecommendation{
		{
			ContainerName: "web",
			CPUTarget:     resource.MustParse("250m"),
			MemoryTarget:  resource.MustParse("512Mi"),
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(deploy).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: testScheme(),
	}

	rec, _, err := reconciler.computeVPARecommendationsForWorkload(
		context.Background(), policy, deploy, vpaRecs, nil, nil, nil,
	)
	require.NoError(t, err)
	assert.Nil(t, rec, "no matching container should result in no recommendation")
}

func newVPAPolicy(name, namespace, vpaName string) *attunev1alpha1.AttunePolicy {
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
				VPA:               &attunev1alpha1.VPAConfig{Name: vpaName},
				MinimumDataPoints: int32Ptr(1),
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
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeRecommend,
			},
		},
	}
}

func TestReconcile_VPASource_Recommendations(t *testing.T) {
	policy := newVPAPolicy("vpa-policy", "default", "my-vpa")
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server",
			Namespace: "default",
			Labels:    map[string]string{"app": "api-server"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:latest",
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
		Status: appsv1.DeploymentStatus{
			Replicas:          1,
			UpdatedReplicas:   1,
			AvailableReplicas: 1,
		},
	}

	vpa := newVPAUnstructured("my-vpa", "default", []map[string]interface{}{
		{
			"containerName": "main",
			"target": map[string]interface{}{
				"cpu":    "250m",
				"memory": "512Mi",
			},
		},
	})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, vpa).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "vpa-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Greater(t, result.RequeueAfter, time.Duration(0))

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "vpa-policy", Namespace: "default"}, &updated))

	assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	require.Len(t, updated.Status.Recommendations, 1)
	assert.Equal(t, "api-server", updated.Status.Recommendations[0].Workload)
	require.Len(t, updated.Status.Recommendations[0].Containers, 1)

	cRec := updated.Status.Recommendations[0].Containers[0]
	assert.Equal(t, "main", cRec.Name)
	assert.Equal(t, float64(1.0), cRec.Confidence)
	// VPA target (250m) with 20% overhead = 300m.
	assert.True(t, cRec.Recommended.CPURequest.Cmp(resource.MustParse("100m")) > 0,
		"CPU recommendation should be higher than current 100m")
}

func TestReconcile_VPASource_VPANotFound(t *testing.T) {
	policy := newVPAPolicy("vpa-policy", "default", "missing-vpa")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "vpa-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Greater(t, result.RequeueAfter, time.Duration(0))

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "vpa-policy", Namespace: "default"}, &updated))

	// VPA not found means no recommendations can be generated.
	assert.Empty(t, updated.Status.Recommendations)
	assert.NotEmpty(t, updated.Status.WorkloadErrors, "should report VPA read error")
}

func TestReconcile_VPASource_DefaultsNamespace(t *testing.T) {
	// When VPA namespace is empty, it should default to the policy's namespace.
	policy := newVPAPolicy("vpa-policy", "prod", "my-vpa")
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server",
			Namespace: "prod",
			Labels:    map[string]string{"app": "api-server"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:latest",
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
		Status: appsv1.DeploymentStatus{
			Replicas:          1,
			UpdatedReplicas:   1,
			AvailableReplicas: 1,
		},
	}

	// VPA in same namespace as policy
	vpa := newVPAUnstructured("my-vpa", "prod", []map[string]interface{}{
		{
			"containerName": "main",
			"target": map[string]interface{}{
				"cpu":    "300m",
				"memory": "1Gi",
			},
		},
	})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, vpa).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "vpa-policy", Namespace: "prod"},
	})
	require.NoError(t, err)
	assert.Greater(t, result.RequeueAfter, time.Duration(0))

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "vpa-policy", Namespace: "prod"}, &updated))

	assert.Equal(t, int32(1), updated.Status.Workloads.WithRecommendations)
	require.Len(t, updated.Status.Recommendations, 1)
}

func TestResolveMetricsCollector_VPA(t *testing.T) {
	policy := newVPAPolicy("vpa-policy", "default", "my-vpa")

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	collector, qb, err := reconciler.resolveMetricsCollector(
		context.Background(), policy, nil,
	)
	assert.NoError(t, err)
	assert.Nil(t, collector, "VPA source should return nil collector")
	assert.Nil(t, qb, "VPA source should return nil query builder")
}

func TestReconcile_VPASource_EmptyRecommendations(t *testing.T) {
	policy := newVPAPolicy("vpa-policy", "default", "empty-vpa")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	// VPA exists but has no recommendations yet.
	vpa := newVPAUnstructured("empty-vpa", "default", nil)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, vpa).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	reconciler := &AttunePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "vpa-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Greater(t, result.RequeueAfter, time.Duration(0))

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "vpa-policy", Namespace: "default"}, &updated))

	// Empty VPA recommendations = no workload recommendations.
	assert.Empty(t, updated.Status.Recommendations)
	assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
}
