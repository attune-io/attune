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
	assert.Equal(t, "attune/recommendations-ns-with-dots-pol", BranchName("ns.with.dots", "pol"))
}

func TestComputeDrift_StatefulSetAndDaemonSet(t *testing.T) {
	t.Parallel()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "pg",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("1"),
							},
						},
					}},
				},
			},
		},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent"},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "agent",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "db", Kind: "StatefulSet",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "pg",
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("500m"),
				},
			}},
		},
		{
			Workload: "agent", Kind: "DaemonSet",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "agent",
				Recommended: attunev1alpha1.ResourceValues{
					MemoryRequest: resource.MustParse("256Mi"),
				},
			}},
		},
	}
	d := ComputeDrift([]client.Object{sts, ds}, recs, 10)
	require.Len(t, d, 2)
	kinds := map[string]bool{}
	for _, x := range d {
		kinds[x.Kind] = true
	}
	assert.True(t, kinds["StatefulSet"], "expected StatefulSet drift")
	assert.True(t, kinds["DaemonSet"], "expected DaemonSet drift")
}

func TestComputeDrift_ZeroTemplateRequestIsFullDrift(t *testing.T) {
	t.Parallel()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						// No requests on template.
					}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api", Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"),
			},
		}},
	}}
	d := ComputeDrift([]client.Object{dep}, recs, 10)
	require.Len(t, d, 1)
	assert.Equal(t, float64(100), d[0].ChangePercent)
	assert.Equal(t, "0", d[0].Template)
}

func TestComputeDrift_UnknownContainerSkipped(t *testing.T) {
	t.Parallel()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api", Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "sidecar-missing",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"),
			},
		}},
	}}
	assert.Empty(t, ComputeDrift([]client.Object{dep}, recs, 10))
}

func TestComputeDrift_NameOnlyMatchWhenKindDiffers(t *testing.T) {
	// Recommendations sometimes omit or mismatch Kind; ComputeDrift falls back
	// to matching on workload name alone.
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
								corev1.ResourceCPU: resource.MustParse("1"),
							},
						},
					}},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api", Kind: "ReplicaSet", // wrong kind; name still matches
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"),
			},
		}},
	}}
	d := ComputeDrift([]client.Object{dep}, recs, 10)
	require.Len(t, d, 1)
	assert.Equal(t, "cpu", d[0].Resource)
	assert.Equal(t, "api", d[0].Workload)
}

func TestComputeDrift_SkipsWorkloadWithoutRecommendation(t *testing.T) {
	t.Parallel()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						},
					}},
				},
			},
		},
	}
	// Recommendation for a different workload only.
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "other", Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "app",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("100m")},
		}},
	}}
	assert.Empty(t, ComputeDrift([]client.Object{dep}, recs, 10))
}
