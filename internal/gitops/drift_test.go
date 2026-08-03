/*
Copyright 2026.
*/
package gitops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestComputeDrift_AboveThreshold(t *testing.T) {
	t.Parallel()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("200m"),
				MemoryRequest: resource.MustParse("512Mi"),
			},
		}},
	}}
	// 60% CPU decrease, 50% mem → both above 10%
	d := ComputeDrift([]client.Object{dep}, recs, 10)
	require.Len(t, d, 2)
	body := FormatPRBody("default", "pol", d)
	assert.Contains(t, body, "api")
	assert.Contains(t, body, "Attune recommendation drift")
	assert.NotContains(t, body, "token")
}

func TestComputeDrift_BelowThreshold(t *testing.T) {
	t.Parallel()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("480m"), // 4%
			},
		}},
	}}
	d := ComputeDrift([]client.Object{dep}, recs, 10)
	assert.Empty(t, d)
}

func TestBranchName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "attune/recommendations-default-my-policy", BranchName("default", "my-policy"))
}
