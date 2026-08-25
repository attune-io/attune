/*
Copyright 2026.
*/
package controller

import (
	"context"
	"strconv"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
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
	"github.com/attune-io/attune/internal/gitops"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestGitOpsPRUnchangedSkip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		fp          string
		annotations map[string]string
		wantSkip    bool
		wantAdopted bool
	}{
		{name: "empty fingerprint", fp: ""},
		{
			name:        "matching fingerprint",
			fp:          "abc",
			annotations: map[string]string{annotationGitOpsPRDrift: "abc"},
			wantSkip:    true,
		},
		{
			name:        "prior URL without fingerprint",
			fp:          "abc",
			annotations: map[string]string{annotationGitOpsPRURL: "https://github.com/org/repo/pull/1"},
			wantSkip:    true,
			wantAdopted: true,
		},
		{
			name:        "failed attempt has no URL",
			fp:          "abc",
			annotations: map[string]string{annotationGitOpsPRLastAttempt: "2026-08-23T00:00:00Z"},
		},
		{
			name:        "different stored fingerprint",
			fp:          "new",
			annotations: map[string]string{annotationGitOpsPRDrift: "old", annotationGitOpsPRURL: "https://github.com/org/repo/pull/1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			skip, adopted := gitOpsPRUnchangedSkip(policy, tc.fp)
			assert.Equal(t, tc.wantSkip, skip)
			assert.Equal(t, tc.wantAdopted, adopted)
		})
	}
}

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

func TestReconcileGitOpsPullRequest_NilUpdateStrategy(t *testing.T) {
	t.Parallel()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
	}
	r := NewAttunePolicyReconciler()
	// Must not panic when UpdateStrategy is nil (defensive for tests/callers).
	r.reconcileGitOpsPullRequest(context.Background(), policy, nil, nil)
	var found bool
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			found = true
			assert.Equal(t, attunev1alpha1.ReasonGitOpsPRDisabled, cond.Reason)
		}
	}
	assert.True(t, found)
}

func TestReconcileGitOpsPullRequest_NoDrift(t *testing.T) {
	t.Parallel()
	en := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Export: &attunev1alpha1.ExportConfig{
					PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
						Enabled:    &en,
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
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
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
				CPURequest: resource.MustParse("480m"), // 4% < default 10%
			},
		}},
	}}
	r := NewAttunePolicyReconciler()
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	var reason string
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			reason = cond.Reason
		}
	}
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRNoDrift, reason)
}

func TestReconcileGitOpsPullRequest_Cooldown(t *testing.T) {
	t.Parallel()
	en := true
	dry := true
	fixed := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "default",
			Annotations: map[string]string{
				// 1h before fixed clock; default cooldown 24h → blocked
				annotationGitOpsPRLastAttempt: fixed.Add(-1 * time.Hour).Format(time.RFC3339),
			},
		},
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
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api", Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"),
			},
		}},
	}}
	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return fixed })
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	var reason string
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			reason = cond.Reason
		}
	}
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRCooldown, reason)
}

func TestTouchGitOpsPRAnnotation_UsesReconcilerClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return fixed })
	policy := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	r.touchGitOpsPRAnnotation(policy, "https://example.com/pr/1")
	assert.Equal(t, fixed.Format(time.RFC3339), policy.Annotations[annotationGitOpsPRLastAttempt])
	assert.Equal(t, "https://example.com/pr/1", policy.Annotations[annotationGitOpsPRURL])
}

func TestReconcileGitOpsPullRequest_MissingSecret(t *testing.T) {
	// Live PR path (dryRun false) with drift but no token secret → Failed,
	// failed metric, no panic, no network.
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := false
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
							Name: "missing-tok", Key: "token",
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
	// Fake client has policy + dep only: no Secret named missing-tok.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c

	before := promtestutil.ToFloat64(operatormetrics.GitOpsPRTotal.WithLabelValues("default", "p", "failed"))
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

	var reason, message string
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			reason = cond.Reason
			message = cond.Message
		}
	}
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRFailed, reason)
	assert.Contains(t, message, "token secret")
	assert.NotContains(t, message, "ghp_")
	assert.Equal(t, before+1, promtestutil.ToFloat64(operatormetrics.GitOpsPRTotal.WithLabelValues("default", "p", "failed")))
}

func TestReconcileGitOpsPullRequest_DryRunIncrementsMetric(t *testing.T) {
	// Complements DryRun reason assertion: dry-run path increments metric
	// and never requires a Secret in the fake client.
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dry-metric", Namespace: "ns-dry"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns-dry"},
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
	before := promtestutil.ToFloat64(operatormetrics.GitOpsPRTotal.WithLabelValues("ns-dry", "dry-metric", "dry_run"))
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
	assert.Equal(t, before+1, promtestutil.ToFloat64(operatormetrics.GitOpsPRTotal.WithLabelValues("ns-dry", "dry-metric", "dry_run")))
	var reason string
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			reason = cond.Reason
		}
	}
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, reason)
}

// recordingPRClient is a fake forge. After SimulateMerge, the next
// CreateOrUpdate would open a new PR (empty list + missing head), matching
// GitHub after squash-merge deletes the recommendation branch.
type recordingPRClient struct {
	calls         []gitops.PRRequest
	merged        bool
	createsAfter  int // CreateOrUpdate calls after SimulateMerge
	createsBefore int
}

func (c *recordingPRClient) CreateOrUpdate(_ context.Context, req gitops.PRRequest) (gitops.PRResult, error) {
	c.calls = append(c.calls, req)
	if c.merged {
		c.createsAfter++
		n := c.createsBefore + c.createsAfter
		return gitops.PRResult{
			URL: "https://github.com/org/repo/pull/" + itoaPR(n), Number: n, Updated: false,
		}, nil
	}
	c.createsBefore++
	return gitops.PRResult{
		URL:    "https://github.com/org/repo/pull/" + itoaPR(c.createsBefore),
		Number: c.createsBefore, Updated: c.createsBefore > 1,
	}, nil
}

func (c *recordingPRClient) SimulateMerge() { c.merged = true }

func itoaPR(n int) string {
	if n < 0 {
		n = 0
	}
	return strconv.Itoa(n)
}

func TestReconcileGitOpsPullRequest_UnchangedDriftSkipsAfterCooldown(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := true
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "authentik", Namespace: "authentik"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "authentik"},
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
	now := start
	r.SetNowFunc(func() time.Time { return now })
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
	require.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, gitOpsPRReason(policy))
	require.NotEmpty(t, policy.Annotations[annotationGitOpsPRDrift])
	firstFP := policy.Annotations[annotationGitOpsPRDrift]

	// Cooldown expired; same recommendation vs same template must not open again.
	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Equal(t, firstFP, policy.Annotations[annotationGitOpsPRDrift])

	// New recommendation is a different fingerprint: proceed after cooldown.
	recs[0].Containers[0].Recommended.CPURequest = resource.MustParse("200m")
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, gitOpsPRReason(policy))
	assert.NotEqual(t, firstFP, policy.Annotations[annotationGitOpsPRDrift])
}

func gitOpsPRReason(policy *attunev1alpha1.AttunePolicy) string {
	for _, cond := range policy.Status.Conditions {
		if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
			return cond.Reason
		}
	}
	return ""
}

func TestReconcileGitOpsPullRequest_LivePathDoesNotRecreateAfterMerge(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := false
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "authentik", Namespace: "authentik"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "authentik"},
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
	fakeForge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = fakeForge
	now := start
	r.SetNowFunc(func() time.Time { return now })
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
	require.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	require.Equal(t, 1, fakeForge.createsBefore, "first cycle opens one PR")
	require.Equal(t, 0, fakeForge.createsAfter)

	// User merged the empty PR; GitHub deleted the head branch.
	// Next reconcile loads the policy from the API, not this pointer.
	fakeForge.SimulateMerge()
	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Equal(t, 0, fakeForge.createsAfter, "same drift after merge must not CreateOrUpdate")
	assert.Equal(t, 1, len(fakeForge.calls))

	// Real new drift still opens after cooldown.
	recs[0].Containers[0].Recommended.CPURequest = resource.MustParse("200m")
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	assert.Equal(t, 1, fakeForge.createsAfter, "changed drift may open a new PR")
	assert.Equal(t, 2, len(fakeForge.calls))
}

// 0.1.22 wrote last-attempt + URL but not attune.io/gitops-pr-drift.
// After upgrade, cooldown expiry used to open another empty PR for the
// same table (#537 comment after 0.1.24).
func TestReconcileGitOpsPullRequest_PreFingerprintUpgradeDoesNotRecreate(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := false
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "authentik",
			Namespace: "authentik",
			Annotations: map[string]string{
				annotationGitOpsPRLastAttempt: start.Add(-25 * time.Hour).Format(time.RFC3339),
				annotationGitOpsPRURL:         "https://github.com/org/repo/pull/41",
			},
		},
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
	dep := gitOpsDriftDeployment("api", "authentik", "1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).Build()
	fakeForge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = fakeForge
	r.SetNowFunc(func() time.Time { return start })
	recs := gitOpsCPURec("api", "100m")

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Empty(t, fakeForge.calls, "upgrade with prior PR URL must not open another empty PR")
	require.NotEmpty(t, policy.Annotations[annotationGitOpsPRDrift])
	stored := &attunev1alpha1.AttunePolicy{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(policy), stored))
	require.Equal(t, policy.Annotations[annotationGitOpsPRDrift], stored.Annotations[annotationGitOpsPRDrift],
		"adopt must persist attune.io/gitops-pr-drift on the API object")

	// Fingerprint now persists; later cycles stay quiet.
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Empty(t, fakeForge.calls)
}

func TestReconcileGitOpsPullRequest_PreFingerprintBackfillDuringCooldown(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := false
	start := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "authentik",
			Namespace: "authentik",
			Annotations: map[string]string{
				annotationGitOpsPRLastAttempt: start.Add(-1 * time.Hour).Format(time.RFC3339),
				annotationGitOpsPRURL:         "https://github.com/org/repo/pull/41",
			},
		},
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
	dep := gitOpsDriftDeployment("api", "authentik", "1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).Build()
	fakeForge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = fakeForge
	now := start
	r.SetNowFunc(func() time.Time { return now })
	recs := gitOpsCPURec("api", "100m")

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy),
		"missing fingerprint with a prior PR URL should adopt, not wait out cooldown")
	assert.Empty(t, fakeForge.calls)
	require.NotEmpty(t, policy.Annotations[annotationGitOpsPRDrift])
	stored := &attunev1alpha1.AttunePolicy{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(policy), stored))
	require.Equal(t, policy.Annotations[annotationGitOpsPRDrift], stored.Annotations[annotationGitOpsPRDrift])

	now = start.Add(24 * time.Hour)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Empty(t, fakeForge.calls, "backfill during cooldown must prevent the next-morning empty PR")
}

func TestReconcileGitOpsPullRequest_FailedAttemptWithoutURLStillRetries(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	dry := false
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "authentik",
			Namespace: "authentik",
			Annotations: map[string]string{
				// Failed API writes last-attempt but not URL.
				annotationGitOpsPRLastAttempt: start.Add(-25 * time.Hour).Format(time.RFC3339),
			},
		},
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
	dep := gitOpsDriftDeployment("api", "authentik", "1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).Build()
	fakeForge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = fakeForge
	r.SetNowFunc(func() time.Time { return start })

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	assert.Equal(t, 1, fakeForge.createsBefore, "failed prior attempt with no URL must retry")
}

func gitOpsEnabledPolicy(name, ns string, dry bool, anns map[string]string) *attunev1alpha1.AttunePolicy {
	en := true
	d := dry
	return &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: anns},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Export: &attunev1alpha1.ExportConfig{
					PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
						Enabled:    &en,
						DryRun:     &d,
						Repository: "org/repo",
						TokenSecretRef: &attunev1alpha1.SecretKeyRef{
							Name: "tok", Key: "token",
						},
					},
				},
			},
		},
	}
}

func gitOpsReload(t *testing.T, c client.Client, policy *attunev1alpha1.AttunePolicy) {
	t.Helper()
	fresh := &attunev1alpha1.AttunePolicy{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(policy), fresh))
	*policy = *fresh
}

func gitOpsLiveReconciler(t *testing.T, policy *attunev1alpha1.AttunePolicy, objs ...client.Object) (*AttunePolicyReconciler, client.Client, *recordingPRClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	all := append([]client.Object{policy}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	forge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = forge
	return r, c, forge
}

func gitOpsDriftDeployment(name, ns, cpu string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse(cpu),
							},
						},
					}},
				},
			},
		},
	}
}

func gitOpsCPURec(workload, cpu string) []attunev1alpha1.WorkloadRecommendation {
	return []attunev1alpha1.WorkloadRecommendation{gitOpsCPUWorkloadRec(workload, cpu)}
}

func gitOpsCPUWorkloadRec(workload, cpu string) attunev1alpha1.WorkloadRecommendation {
	return gitOpsWorkloadRec(workload, cpu, "")
}

func gitOpsWorkloadRec(workload, cpu, mem string) attunev1alpha1.WorkloadRecommendation {
	rec := attunev1alpha1.ResourceValues{}
	if cpu != "" {
		rec.CPURequest = resource.MustParse(cpu)
	}
	if mem != "" {
		rec.MemoryRequest = resource.MustParse(mem)
	}
	return attunev1alpha1.WorkloadRecommendation{
		Workload: workload, Kind: "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app", Recommended: rec,
		}},
	}
}

func gitOpsDeploymentResources(name, ns, cpu, mem string) *appsv1.Deployment {
	req := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu),
	}
	if mem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      "app",
						Resources: corev1.ResourceRequirements{Requests: req},
					}},
				},
			},
		},
	}
}

func TestReconcileGitOpsPullRequest_IncompleteConfig(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	en := true
	cases := []struct {
		name string
		pr   *attunev1alpha1.GitOpsPullRequestConfig
	}{
		{
			name: "empty repository",
			pr: &attunev1alpha1.GitOpsPullRequestConfig{
				Enabled: &en, Repository: "",
				TokenSecretRef: &attunev1alpha1.SecretKeyRef{Name: "tok", Key: "token"},
			},
		},
		{
			name: "nil tokenSecretRef",
			pr: &attunev1alpha1.GitOpsPullRequestConfig{
				Enabled: &en, Repository: "org/repo",
			},
		},
		{
			name: "empty secret key",
			pr: &attunev1alpha1.GitOpsPullRequestConfig{
				Enabled: &en, Repository: "org/repo",
				TokenSecretRef: &attunev1alpha1.SecretKeyRef{Name: "tok", Key: ""},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := &attunev1alpha1.AttunePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
				Spec: attunev1alpha1.AttunePolicySpec{
					UpdateStrategy: &attunev1alpha1.UpdateStrategy{
						Export: &attunev1alpha1.ExportConfig{PullRequest: tc.pr},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
			r := NewAttunePolicyReconciler()
			r.Client = c
			r.reconcileGitOpsPullRequest(context.Background(), policy, nil, nil)
			var reason, message string
			for _, cond := range policy.Status.Conditions {
				if cond.Type == attunev1alpha1.ConditionGitOpsPullRequest {
					reason = cond.Reason
					message = cond.Message
				}
			}
			assert.Equal(t, attunev1alpha1.ReasonGitOpsPRFailed, reason)
			assert.Contains(t, message, "repository")
			assert.Contains(t, message, "tokenSecretRef")
		})
	}
}

func TestSetGitOpsPRCondition_ReplaceAndPreserveTransition(t *testing.T) {
	t.Parallel()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Generation: 3},
	}
	// First set
	setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRNoDrift, "none")
	require.Len(t, policy.Status.Conditions, 1)
	first := policy.Status.Conditions[0].LastTransitionTime

	// Same status+reason: preserve LastTransitionTime, update message/generation
	setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRNoDrift, "still none")
	require.Len(t, policy.Status.Conditions, 1)
	assert.Equal(t, first, policy.Status.Conditions[0].LastTransitionTime)
	assert.Equal(t, "still none", policy.Status.Conditions[0].Message)
	assert.Equal(t, int64(3), policy.Status.Conditions[0].ObservedGeneration)

	// Different reason: replace condition (new transition time allowed)
	setGitOpsPRCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonGitOpsPRDryRun, "dry")
	require.Len(t, policy.Status.Conditions, 1)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, policy.Status.Conditions[0].Reason)
	assert.Equal(t, metav1.ConditionTrue, policy.Status.Conditions[0].Status)
	assert.Equal(t, "dry", policy.Status.Conditions[0].Message)
}
