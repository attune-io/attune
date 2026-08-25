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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/gitops"
	"github.com/attune-io/attune/internal/operatormetrics"
)

const (
	annotationGitOpsPRLastAttempt = "attune.io/gitops-pr-last-attempt"
	annotationGitOpsPRURL         = "attune.io/gitops-pr-url"
	annotationGitOpsPRDrift       = "attune.io/gitops-pr-drift"
	defaultGitOpsPRCooldown       = 24 * time.Hour
	defaultGitOpsMinChangePct     = int32(10)
)

// gitopsPREnabled reports whether pull request automation is opt-in enabled.
func gitopsPREnabled(export *attunev1alpha1.ExportConfig) bool {
	if export == nil || export.PullRequest == nil || export.PullRequest.Enabled == nil {
		return false
	}
	return *export.PullRequest.Enabled
}

// reconcileGitOpsPullRequest runs opt-in PR automation after recommendations exist.
// Never logs the token. Updates ConditionGitOpsPullRequest on the policy status.
func (r *AttunePolicyReconciler) reconcileGitOpsPullRequest(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workloads []client.Object,
	recs []attunev1alpha1.WorkloadRecommendation,
) {
	logger := log.FromContext(ctx)
	// Defensive: defaults normally set UpdateStrategy, but unit tests and
	// future callers may invoke this without applying defaults first.
	if policy.Spec.UpdateStrategy == nil || !gitopsPREnabled(policy.Spec.UpdateStrategy.Export) {
		setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRDisabled, "GitOps pull request automation is disabled")
		return
	}
	cfg := policy.Spec.UpdateStrategy.Export.PullRequest
	if cfg.Repository == "" || cfg.TokenSecretRef == nil || cfg.TokenSecretRef.Name == "" || cfg.TokenSecretRef.Key == "" {
		setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRFailed,
			"pullRequest requires repository and tokenSecretRef when enabled")
		return
	}

	minPct := float64(defaultGitOpsMinChangePct)
	if cfg.MinChangePercent != nil {
		minPct = float64(*cfg.MinChangePercent)
	}
	drifts := gitops.ComputeDrift(workloads, recs, minPct)
	if len(drifts) == 0 {
		logger.V(1).Info("GitOps PR skipped: no drift above threshold",
			"minChangePercent", minPct, "workloads", len(workloads), "recommendations", len(recs))
		setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRNoDrift,
			"No recommendation drift above minChangePercent vs templates")
		return
	}
	dryRun := cfg.DryRun != nil && *cfg.DryRun
	fp := gitops.DriftFingerprint(drifts)
	if skip, adopted := gitOpsPRUnchangedSkip(policy, fp); skip {
		// Dry-run writes the fingerprint without a PR URL. A later live
		// cycle of the same table must still open the first real PR.
		if !adopted && !dryRun && gitOpsStoredURL(policy) == "" {
			logger.V(1).Info("GitOps PR: first live cycle after dry-run, opening PR")
		} else {
			if adopted {
				logger.V(1).Info("GitOps PR skipped: adopting drift fingerprint from last PR")
				setGitOpsPRDriftAnnotation(policy, fp)
				r.persistGitOpsPRAnnotations(ctx, policy)
			} else {
				logger.V(1).Info("GitOps PR skipped: drift table unchanged since last PR")
			}
			setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRUnchanged,
				"Drift table unchanged since last pull request; not opening a new empty PR")
			return
		}
	}

	cooldown := defaultGitOpsPRCooldown
	if cfg.Cooldown != nil && cfg.Cooldown.Duration > 0 {
		cooldown = cfg.Cooldown.Duration
	}
	if last := gitOpsStoredLastAttempt(policy); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && r.now().Sub(t) < cooldown {
			until := t.Add(cooldown).UTC()
			logger.V(1).Info("GitOps PR skipped: cooldown active",
				"until", until.Format(time.RFC3339), "lastAttempt", last)
			setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRCooldown,
				fmt.Sprintf("Cooldown active until %s", until.Format(time.RFC3339)))
			return
		}
	}

	base := cfg.BaseBranch
	if base == "" {
		base = "main"
	}
	provider := cfg.Provider
	if provider == "" {
		provider = "github"
	}
	title := fmt.Sprintf("chore(attune): apply recommendations for %s/%s", policy.Namespace, policy.Name)
	body := gitops.FormatPRBody(policy.Namespace, policy.Name, drifts)
	head := gitops.BranchName(policy.Namespace, policy.Name)

	if dryRun {
		logger.Info("GitOps PR dry-run", "provider", provider, "repository", cfg.Repository,
			"head", head, "base", base, "driftCount", len(drifts))
		setGitOpsPRCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonGitOpsPRDryRun,
			fmt.Sprintf("Dry-run: would open/update PR on %s (%d drifted resources)", cfg.Repository, len(drifts)))
		// Fingerprint only. last-attempt would block the first live PR
		// for the default 24h cooldown (docs: turn off dry-run, then open).
		setGitOpsPRDriftAnnotation(policy, fp)
		r.persistGitOpsPRAnnotations(ctx, policy)
		operatormetrics.GitOpsPRTotal.WithLabelValues(policy.Namespace, policy.Name, "dry_run").Inc()
		return
	}

	var prClient gitops.PullRequestClient
	if r.gitopsPRClient != nil {
		prClient = r.gitopsPRClient
	} else {
		token, err := r.readSecretKey(ctx, policy.Namespace, cfg.TokenSecretRef.Name, cfg.TokenSecretRef.Key)
		if err != nil {
			// Do not include secret name details that might confuse with token material.
			logger.Error(err, "GitOps PR: failed to read token secret")
			setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRFailed,
				"Failed to read token secret (check name/key and RBAC)")
			operatormetrics.GitOpsPRTotal.WithLabelValues(policy.Namespace, policy.Name, "failed").Inc()
			return
		}
		switch provider {
		case "gitlab":
			prClient = &gitops.GitLabClient{
				BaseURL: cfg.APIURL,
				Token:   token,
				Project: cfg.Repository,
			}
		default:
			prClient = &gitops.GitHubClient{
				BaseURL:    cfg.APIURL,
				Token:      token,
				Repository: cfg.Repository,
			}
		}
	}

	res, err := prClient.CreateOrUpdate(ctx, gitops.PRRequest{
		Title: title, Body: body, Head: head, Base: base, Labels: cfg.Labels,
	})
	if err != nil {
		// Never include raw API bodies that might echo credentials.
		logger.Error(err, "GitOps PR create/update failed", "provider", provider, "repository", cfg.Repository)
		setGitOpsPRCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonGitOpsPRFailed,
			fmt.Sprintf("PR API error: %v", err))
		operatormetrics.GitOpsPRTotal.WithLabelValues(policy.Namespace, policy.Name, "failed").Inc()
		r.touchGitOpsPRAnnotation(policy, "")
		r.persistGitOpsPRAnnotations(ctx, policy)
		return
	}

	msg := fmt.Sprintf("Pull request %s", res.URL)
	if res.Updated {
		msg = "Updated " + msg
		operatormetrics.GitOpsPRTotal.WithLabelValues(policy.Namespace, policy.Name, "updated").Inc()
	} else {
		operatormetrics.GitOpsPRTotal.WithLabelValues(policy.Namespace, policy.Name, "created").Inc()
	}
	setGitOpsPRCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonGitOpsPROpen, msg)
	r.touchGitOpsPRAnnotation(policy, res.URL)
	setGitOpsPRDriftAnnotation(policy, fp)
	r.persistGitOpsPRAnnotations(ctx, policy)
}

func setGitOpsPRCondition(policy *attunev1alpha1.AttunePolicy, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	cond := metav1.Condition{
		Type:               attunev1alpha1.ConditionGitOpsPullRequest,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: policy.Generation,
	}
	// Replace or append.
	found := false
	for i := range policy.Status.Conditions {
		if policy.Status.Conditions[i].Type == attunev1alpha1.ConditionGitOpsPullRequest {
			if policy.Status.Conditions[i].Status == status && policy.Status.Conditions[i].Reason == reason {
				cond.LastTransitionTime = policy.Status.Conditions[i].LastTransitionTime
			}
			policy.Status.Conditions[i] = cond
			found = true
			break
		}
	}
	if !found {
		policy.Status.Conditions = append(policy.Status.Conditions, cond)
	}
}

// touchGitOpsPRAnnotation records last attempt using the reconciler clock so
// cooldown checks (r.now()) stay consistent under tests with a fake clock.
func (r *AttunePolicyReconciler) touchGitOpsPRAnnotation(policy *attunev1alpha1.AttunePolicy, url string) {
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	now := r.now().UTC()
	policy.Annotations[annotationGitOpsPRLastAttempt] = now.Format(time.RFC3339)
	st := ensureGitOpsPRStatus(policy)
	t := metav1.NewTime(now)
	st.LastAttempt = &t
	if url != "" {
		policy.Annotations[annotationGitOpsPRURL] = url
		st.URL = url
	}
}

// gitOpsPRUnchangedSkip reports whether the current drift table should
// not open a new PR. adopted is true when 0.1.22 (or a persist miss)
// left last-attempt+URL but no fingerprint: treat the live table as the
// last notification and persist the hash instead of opening another
// empty PR. Do not adopt when URL is empty; that last-attempt may be a
// failed API call that should retry after cooldown.
func gitOpsPRUnchangedSkip(policy *attunev1alpha1.AttunePolicy, fp string) (skip, adopted bool) {
	if fp == "" {
		return false, false
	}
	if gitOpsStoredDrift(policy) == fp {
		return true, false
	}
	if gitOpsStoredDrift(policy) == "" && gitOpsStoredURL(policy) != "" {
		return true, true
	}
	return false, false
}

func gitOpsStoredDrift(policy *attunev1alpha1.AttunePolicy) string {
	if policy.Status.GitOpsPR != nil && policy.Status.GitOpsPR.DriftFingerprint != "" {
		return policy.Status.GitOpsPR.DriftFingerprint
	}
	return policy.Annotations[annotationGitOpsPRDrift]
}

func gitOpsStoredURL(policy *attunev1alpha1.AttunePolicy) string {
	if policy.Status.GitOpsPR != nil && policy.Status.GitOpsPR.URL != "" {
		return policy.Status.GitOpsPR.URL
	}
	return policy.Annotations[annotationGitOpsPRURL]
}

func gitOpsStoredLastAttempt(policy *attunev1alpha1.AttunePolicy) string {
	if policy.Status.GitOpsPR != nil && policy.Status.GitOpsPR.LastAttempt != nil &&
		!policy.Status.GitOpsPR.LastAttempt.Time.IsZero() {
		return policy.Status.GitOpsPR.LastAttempt.UTC().Format(time.RFC3339)
	}
	return policy.Annotations[annotationGitOpsPRLastAttempt]
}

func ensureGitOpsPRStatus(policy *attunev1alpha1.AttunePolicy) *attunev1alpha1.GitOpsPRStatus {
	if policy.Status.GitOpsPR == nil {
		policy.Status.GitOpsPR = &attunev1alpha1.GitOpsPRStatus{}
	}
	return policy.Status.GitOpsPR
}

func setGitOpsPRDriftAnnotation(policy *attunev1alpha1.AttunePolicy, fingerprint string) {
	if fingerprint == "" {
		return
	}
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[annotationGitOpsPRDrift] = fingerprint
	ensureGitOpsPRStatus(policy).DriftFingerprint = fingerprint
}

// persistGitOpsPRAnnotations patches policy annotations so cooldown survives restarts.
// Best-effort; failures do not fail reconcile but are logged at V(1).
func (r *AttunePolicyReconciler) persistGitOpsPRAnnotations(ctx context.Context, policy *attunev1alpha1.AttunePolicy) {
	if policy.Annotations == nil {
		return
	}
	logger := log.FromContext(ctx)
	const attempts = 3
	var lastErr error
	for range attempts {
		latest := &attunev1alpha1.AttunePolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(policy), latest); err != nil {
			lastErr = err
			continue
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		if v, ok := policy.Annotations[annotationGitOpsPRLastAttempt]; ok {
			latest.Annotations[annotationGitOpsPRLastAttempt] = v
		}
		if v, ok := policy.Annotations[annotationGitOpsPRURL]; ok {
			latest.Annotations[annotationGitOpsPRURL] = v
		}
		if v, ok := policy.Annotations[annotationGitOpsPRDrift]; ok {
			latest.Annotations[annotationGitOpsPRDrift] = v
		}
		if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			lastErr = err
			continue
		}
		if policy.Status.GitOpsPR != nil {
			stBase := latest.DeepCopy()
			latest.Status.GitOpsPR = policy.Status.GitOpsPR.DeepCopy()
			if err := r.Status().Patch(ctx, latest, client.MergeFrom(stBase)); err != nil {
				lastErr = err
				continue
			}
		}
		return
	}
	if lastErr != nil {
		logger.V(1).Info("GitOps PR: failed to persist cooldown annotations",
			"error", lastErr.Error(), "policy", policy.Name, "namespace", policy.Namespace)
	}
}
