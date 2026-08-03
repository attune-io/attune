/*
Copyright 2026.
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

func TestGitopsPREnabled(t *testing.T) {
	t.Parallel()
	assert.False(t, gitopsPREnabled(nil))
	assert.False(t, gitopsPREnabled(&attunev1alpha1.ExportConfig{}))
	en := true
	assert.True(t, gitopsPREnabled(&attunev1alpha1.ExportConfig{
		PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{Enabled: &en},
	}))
}

func TestReconcileGitOpsPullRequest_DryRun(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Export: &attunev1alpha1.ExportConfig{
					PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
						Enabled:    &en,
						DryRun:     &dry,
						Repository: "org/repo",
						TokenSecretRef: &attunev1alpha1.SecretKeyRef{
							Name: "tok", Key: "token",
						},
					},
				},
			},
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api", Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"),
			},
		}},
	}}
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	var found bool
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			found = true
			assert.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, cond.Reason)
		}
	}
	assert.True(t, found)
}
