/*
Copyright 2026.
*/
package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestReconcileGitOpsPullRequest_DryRunThenLiveOpensPR(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	policy := gitOpsEnabledPolicy("authentik", "ns-dry-live", true, nil)
	dep := gitOpsDriftDeployment("api", "ns-dry-live", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return start })
	recs := gitOpsCPURec("api", "100m")

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	require.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, gitOpsPRReason(policy))
	assert.Empty(t, forge.calls, "dry-run must not call the forge")

	gitOpsReload(t, c, policy)
	*policy.Spec.UpdateStrategy.Export.PullRequest.DryRun = false
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy),
		"first live cycle after dry-run must open a PR for the same table")
	assert.Equal(t, 1, forge.createsBefore)
}

func TestReconcileGitOpsPullRequest_LiveAfterDryRunWithLastAttemptStillCools(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	dep := gitOpsDriftDeployment("api", "ns-stale-dry", "1")
	recs := gitOpsCPURec("api", "100m")
	policy := gitOpsEnabledPolicy("p", "ns-stale-dry", true, nil)
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return start })
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	require.Equal(t, attunev1alpha1.ReasonGitOpsPRDryRun, gitOpsPRReason(policy))
	fp := policy.Annotations[annotationGitOpsPRDrift]
	require.NotEmpty(t, fp)

	// 0.1.24 dry-run wrote last-attempt. Live must still honor cooldown
	// so a failed API retry window is not skipped.
	gitOpsReload(t, c, policy)
	*policy.Spec.UpdateStrategy.Export.PullRequest.DryRun = false
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[annotationGitOpsPRLastAttempt] = start.Add(-time.Hour).Format(time.RFC3339)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRCooldown, gitOpsPRReason(policy))
	assert.Empty(t, forge.calls)
}

func TestReconcileGitOpsPullRequest_AnnotationWipeStatusStillSkips(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-wipe", false, nil)
	dep := gitOpsDriftDeployment("api", "ns-wipe", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return now })
	recs := gitOpsCPURec("api", "100m")

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	require.Equal(t, 1, forge.createsBefore)
	require.NotNil(t, policy.Status.GitOpsPR)
	require.NotEmpty(t, policy.Status.GitOpsPR.DriftFingerprint)

	// Flux/Argo replaced metadata.annotations from git.
	gitOpsReload(t, c, policy)
	policy.Annotations = map[string]string{}
	require.NoError(t, c.Update(context.Background(), policy))
	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	require.Empty(t, policy.Annotations[annotationGitOpsPRDrift])
	require.NotNil(t, policy.Status.GitOpsPR)
	require.NotEmpty(t, policy.Status.GitOpsPR.DriftFingerprint)

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Equal(t, 1, len(forge.calls), "status fingerprint must skip after annotation wipe")
}

func TestReconcileGitOpsPullRequest_PersistRetriesFirstPatchFail(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	var patches atomic.Int32
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	policy := gitOpsEnabledPolicy("persist-retry", "ns-persist", false, nil)
	dep := gitOpsDriftDeployment("api", "ns-persist", "1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*attunev1alpha1.AttunePolicy); ok && patches.Add(1) == 1 {
					return fmt.Errorf("simulated conflict")
				}
				return cw.Patch(ctx, obj, p, opts...)
			},
		}).Build()
	forge := &recordingPRClient{}
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.gitopsPRClient = forge
	r.SetNowFunc(func() time.Time { return start })

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	require.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	require.Equal(t, 1, forge.createsBefore)

	gitOpsReload(t, c, policy)
	require.NotEmpty(t, policy.Annotations[annotationGitOpsPRDrift],
		"first Patch conflict must be retried so the fingerprint lands")
	require.NotEmpty(t, policy.Annotations[annotationGitOpsPRURL])

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Equal(t, 1, len(forge.calls))
}

func TestReconcileGitOpsPullRequest_AdoptThenRecsChangeOpens(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	policy := gitOpsEnabledPolicy("authentik", "ns-adopt-chg", false, map[string]string{
		annotationGitOpsPRLastAttempt: start.Add(-25 * time.Hour).Format(time.RFC3339),
		annotationGitOpsPRURL:         "https://github.com/org/repo/pull/41",
	})
	dep := gitOpsDriftDeployment("api", "ns-adopt-chg", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return start })

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	require.Equal(t, attunev1alpha1.ReasonGitOpsPRUnchanged, gitOpsPRReason(policy))
	assert.Empty(t, forge.calls)

	gitOpsReload(t, c, policy)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "200m"))
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	assert.Equal(t, 1, forge.createsBefore)
}

func TestReconcileGitOpsPullRequest_OpenPRRecsChangeDuringCooldown(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-cool-upd", false, nil)
	dep := gitOpsDriftDeployment("api", "ns-cool-upd", "1")
	r, _, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return now })

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	require.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	require.Equal(t, 1, forge.createsBefore)

	now = start.Add(time.Hour)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "200m"))
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRCooldown, gitOpsPRReason(policy))
	assert.Equal(t, 1, len(forge.calls), "open PR body stays stale during cooldown")
}

func TestReconcileGitOpsPullRequest_OpenPRRecsChangeAfterCooldownUpdates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-upd", false, nil)
	dep := gitOpsDriftDeployment("api", "ns-upd", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return now })

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "100m"))
	require.Equal(t, 1, forge.createsBefore)

	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, gitOpsCPURec("api", "200m"))
	require.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	require.Len(t, forge.calls, 2)
	assert.Equal(t, 2, forge.createsBefore, "head still exists: CreateOrUpdate updates the open PR")
	assert.Equal(t, 0, forge.createsAfter)
}

func TestReconcileGitOpsPullRequest_TemplatesPatchedNoDrift(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-applied", false, nil)
	dep := gitOpsDriftDeployment("api", "ns-applied", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return now })
	recs := gitOpsCPURec("api", "100m")

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	require.Equal(t, 1, forge.createsBefore)

	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	applied := gitOpsDriftDeployment("api", "ns-applied", "100m")
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{applied}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPRNoDrift, gitOpsPRReason(policy))
	assert.Equal(t, 1, len(forge.calls))
}

func TestReconcileGitOpsPullRequest_TwoWorkloadsOneApplied(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-two", false, nil)
	server := gitOpsDriftDeployment("server", "ns-two", "1")
	worker := gitOpsDriftDeployment("worker", "ns-two", "1")
	r, c, forge := gitOpsLiveReconciler(t, policy, server, worker)
	r.SetNowFunc(func() time.Time { return now })
	recs := []attunev1alpha1.WorkloadRecommendation{
		gitOpsCPUWorkloadRec("server", "100m"),
		gitOpsCPUWorkloadRec("worker", "100m"),
	}

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{server, worker}, recs)
	require.Equal(t, 1, forge.createsBefore)

	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	serverApplied := gitOpsDriftDeployment("server", "ns-two", "100m")
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{serverApplied, worker}, recs)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy),
		"one remaining drifted workload is a new table")
	assert.Equal(t, 2, len(forge.calls))
}

func TestReconcileGitOpsPullRequest_OneRowDropsBelowMinChange(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	now := start
	policy := gitOpsEnabledPolicy("authentik", "ns-row", false, nil)
	dep := gitOpsDeploymentResources("api", "ns-row", "1", "1Gi")
	r, c, forge := gitOpsLiveReconciler(t, policy, dep)
	r.SetNowFunc(func() time.Time { return now })
	recs := []attunev1alpha1.WorkloadRecommendation{gitOpsWorkloadRec("api", "100m", "512Mi")}

	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, recs)
	require.Equal(t, 1, forge.createsBefore)

	now = start.Add(25 * time.Hour)
	gitOpsReload(t, c, policy)
	// Memory now matches template; only CPU remains above 10%.
	cpuOnly := []attunev1alpha1.WorkloadRecommendation{gitOpsWorkloadRec("api", "100m", "1Gi")}
	r.reconcileGitOpsPullRequest(context.Background(), policy, []client.Object{dep}, cpuOnly)
	assert.Equal(t, attunev1alpha1.ReasonGitOpsPROpen, gitOpsPRReason(policy))
	assert.Equal(t, 2, len(forge.calls))
}
