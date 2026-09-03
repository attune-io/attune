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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// After an in-place resize the pod template (rec.Current) can stay at
// 256Mi while the live container is 1Gi. Undo annotations and quota
// headroom must snapshot the live container, not the template.

func TestExecuteResizes_OriginalAnnotationUsesLiveNotTemplate(t *testing.T) {
	// Live memory is already 1Gi; recommendation Current still has the
	// template 256Mi. CPU changes so persist runs. Original memory must
	// be 1Gi so a later revert does not roll back to the template.
	pod := newResizePodWithStatus("api-server", "500m", "1Gi", "1000m", "2Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server",
			"500m", "256Mi", "1000m", "512Mi",
			"750m", "1Gi", "1500m", "2Gi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count, "CPU change must apply so original annotations persist")
	require.NotEmpty(t, history)

	var updated corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated))

	assert.Equal(t, "1Gi", updated.Annotations[annotationOriginalMemoryPrefix+"main"],
		"original memory must be the live request, not the 256Mi template")
	assert.Equal(t, "2Gi", updated.Annotations[annotationOriginalMemoryLimitPrefix+"main"],
		"original memory limit must be the live limit, not the template")
	assert.Equal(t, "500m", updated.Annotations[annotationOriginalCPUPrefix+"main"])
}

func TestShouldSkipResize_QuotaBaselineUsesLiveNotTemplate(t *testing.T) {
	// Live 1Gi, template Current 256Mi, target 512Mi. Quota has 100Mi
	// headroom. Template delta looks like +256Mi and would skip; live
	// delta is a decrease and must pass.
	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("2Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("1948Mi"),
			},
		},
	}
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	checks := &resizePreChecks{quotas: []corev1.ResourceQuota{quota}}

	skip, reason := r.shouldSkipResize(context.Background(), policy, pod, containerRec, target, checks)
	assert.False(t, skip, "decrease from live 1Gi to 512Mi must not use template 256Mi as quota baseline, reason=%q", reason)
	assert.Empty(t, reason)
}

func TestLiveContainerCurrent_PrefersLiveOverTemplate(t *testing.T) {
	pod := newResizePod("api-server", "500m", "1Gi", "1000m", "2Gi")
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			CPULimit:      resource.MustParse("1000m"),
			MemoryRequest: resource.MustParse("256Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}

	got := liveContainerCurrent(pod, rec)
	assert.True(t, got.MemoryRequest.Equal(resource.MustParse("1Gi")),
		"live memory request must win over template Current, got %s", got.MemoryRequest.String())
	assert.True(t, got.MemoryLimit.Equal(resource.MustParse("2Gi")),
		"live memory limit must win over template Current, got %s", got.MemoryLimit.String())

	missing := rec
	missing.Name = "not-a-container"
	fallback := liveContainerCurrent(pod, missing)
	assert.True(t, fallback.MemoryRequest.Equal(resource.MustParse("256Mi")),
		"missing container must fall back to rec.Current")
	assert.Equal(t, rec.Current, liveContainerCurrent(nil, rec),
		"nil pod must fall back to rec.Current")
}

func TestLiveContainerCurrent_MissingKeysKeepCurrent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "1Gi", "1000m", "2Gi")
	delete(pod.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			CPULimit:      resource.MustParse("1000m"),
			MemoryRequest: resource.MustParse("256Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}

	got := liveContainerCurrent(pod, rec)
	assert.True(t, got.MemoryRequest.Equal(resource.MustParse("1Gi")),
		"live memory request must overlay Current, got %s", got.MemoryRequest.String())
	assert.True(t, got.MemoryLimit.Equal(resource.MustParse("512Mi")),
		"missing live limit must keep rec.Current, not invent zero; got %s", got.MemoryLimit.String())
	assert.True(t, got.CPULimit.Equal(resource.MustParse("1000m")),
		"present live CPU limit must still overlay Current")
}
