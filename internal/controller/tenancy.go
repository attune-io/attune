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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/safety"
	"github.com/attune-io/attune/internal/validation"
)

// sloPlanKey carries tenant guardrail provenance for this reconcile.
type sloPlanKey struct{}

// sloPlan is taken from the policy and namespace defaults before merge.
// Cluster AttuneDefaults guardrails are not tenant guardrails.
type sloPlan struct {
	tenantGuardrails bool
}

func (r *AttunePolicyReconciler) sigv4Allowlist() validation.SigV4Allowlist {
	if r == nil {
		return validation.SigV4Allowlist{}
	}
	return validation.SigV4Allowlist{
		RoleARNs: r.SigV4AllowedRoleARNs,
		Hosts:    r.SigV4AllowedWorkspaceHosts,
	}
}

// refuseTenantMetrics rejects namespace-authored metrics references that
// would use the operator identity outside the allowlists, or read a VPA
// in another namespace. Cluster AttuneDefaults is not passed here.
func refuseTenantMetrics(policyNamespace string, policy *attunev1alpha1.AttunePolicy, namespaceDefaults *attunev1alpha1.AttuneDefaultsSpec, allow validation.SigV4Allowlist) error {
	source := tenantMetricsSource(policy, namespaceDefaults)
	if source == nil {
		return nil
	}
	if source.Prometheus != nil && source.Prometheus.SigV4 != nil {
		if err := validation.SigV4PolicyAllowed(source.Prometheus.Address, source.Prometheus.SigV4.RoleARN, allow); err != nil {
			return err
		}
	}
	if source.CloudWatch != nil {
		if err := validation.CloudWatchRoleAllowed(source.CloudWatch.RoleARN, allow); err != nil {
			return err
		}
	}
	if source.VPA != nil {
		if err := validation.VPANamespace(policyNamespace, source.VPA.Namespace, false); err != nil {
			return err
		}
	}
	return nil
}

func tenantMetricsSource(policy *attunev1alpha1.AttunePolicy, namespaceDefaults *attunev1alpha1.AttuneDefaultsSpec) *attunev1alpha1.MetricsSource {
	if policy != nil && metricsProviderSet(&policy.Spec.MetricsSource) {
		return &policy.Spec.MetricsSource
	}
	if namespaceDefaults != nil && metricsProviderSet(namespaceDefaults.MetricsSource) {
		return namespaceDefaults.MetricsSource
	}
	return nil
}

func metricsProviderSet(ms *attunev1alpha1.MetricsSource) bool {
	return ms != nil && (ms.Prometheus != nil || ms.Datadog != nil || ms.CloudWatch != nil || ms.VPA != nil)
}

func tenantWroteGuardrails(policy *attunev1alpha1.AttunePolicy, namespaceDefaults *attunev1alpha1.AttuneDefaultsSpec) bool {
	if policy != nil && policy.Spec.UpdateStrategy != nil && len(policy.Spec.UpdateStrategy.SLOGuardrails) > 0 {
		return true
	}
	return namespaceDefaults != nil && namespaceDefaults.UpdateStrategy != nil && len(namespaceDefaults.UpdateStrategy.SLOGuardrails) > 0
}

func (r *AttunePolicyReconciler) sloAuthMode(ctx context.Context, collector rsmetrics.MetricsCollector) safety.SLOAuthMode {
	if !rsmetrics.CollectorSupportsSLO(collector) {
		return safety.SLOAuthSkip
	}
	plan, _ := ctx.Value(sloPlanKey{}).(sloPlan)
	if !plan.tenantGuardrails || !rsmetrics.CollectorUsesOperatorAuth(collector) {
		return safety.SLOAuthInherit
	}
	if r == nil || !r.SLOGuardrailEnforceNamespace {
		return safety.SLOAuthSkip
	}
	return safety.SLOAuthEnforceNamespace
}

func (r *AttunePolicyReconciler) sloQuerierActive(ctx context.Context, collector rsmetrics.MetricsCollector, namespace string, guardrails []attunev1alpha1.SLOGuardrail) bool {
	mode := r.sloAuthMode(ctx, collector)
	if len(guardrails) == 0 || mode == safety.SLOAuthSkip {
		return false
	}
	if mode != safety.SLOAuthEnforceNamespace {
		return true
	}
	for _, guardrail := range guardrails {
		if safety.PromQLQueryMayRun(guardrail.Query, namespace) {
			return true
		}
	}
	return false
}

func (r *AttunePolicyReconciler) setSLOGuardrailCondition(policy *attunev1alpha1.AttunePolicy, status metav1.ConditionStatus, reason, message string) {
	if policy == nil {
		return
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               attunev1alpha1.ConditionSLOGuardrails,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
}

func (r *AttunePolicyReconciler) noteSkippedTenantGuardrail(policy *attunev1alpha1.AttunePolicy) {
	r.setSLOGuardrailCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonSLOGuardrailNoTenantCredentials,
		"Tenant SLO guardrails were not run because they would use operator credentials. Move them to AttuneDefaults, give the policy its own Prometheus credentials, or leave --slo-guardrail-enforce-namespace at its default.")
}

// unwindRejectedTenantMetrics reverts critical pod failures and expires a
// startup boost that already landed. It does not query the rejected
// metrics source, apply a new resize, or raise a new boost.
func (r *AttunePolicyReconciler) unwindRejectedTenantMetrics(ctx context.Context, policy *attunev1alpha1.AttunePolicy) {
	if r == nil || policy == nil || r.Clientset == nil {
		return
	}
	logger := log.FromContext(ctx)
	workloads, err := r.discoverWorkloads(ctx, policy)
	if err != nil {
		logger.Error(err, "Rejected tenant metrics; workload discovery failed, skipping in-flight unwind")
		return
	}
	if autoRevertEnabled(policy.Spec.UpdateStrategy) {
		_ = r.checkPendingSafetyObservations(contextWithSafetyKeepTracking(ctx), policy, nil, workloads)
	}
	if policy.Spec.Memory.OOMBump != nil {
		for _, w := range workloads {
			r.persistPendingAnnotationOnlyOOMBumps(ctx, policy, w)
		}
	}
	if policy.Spec.UpdateStrategy == nil || !isResizeMode(policy.Spec.UpdateStrategy.Type) ||
		policy.Spec.CPU.StartupBoost == nil || len(policy.Status.Recommendations) == 0 {
		return
	}
	podsByWorkload := r.listPodsForWorkloads(ctx, workloads)
	resizer := resize.NewPodResizer(r.Clientset, logger)
	resizer.AllowInPlaceMemoryLimitDecrease = r.AllowInPlaceMemoryLimitDecrease
	resizer.InPlacePodLevelResources = r.inPlacePodLevelResources()
	r.applyStartupBoosts(contextWithStartupBoostExpiryOnly(ctx), policy, podsByWorkload, policy.Status.Recommendations, resizer, nil)
}

// finishRejectedTenantStatus persists Ready=InvalidConfig plus any history
// or condition changes from the unwind. A later reconcile already has
// Ready set, and setFailedCondition would return without writing the
// revert mark. That left the next minute calling UpdateResize again.
func (r *AttunePolicyReconciler) finishRejectedTenantStatus(ctx context.Context, policy *attunev1alpha1.AttunePolicy, before *attunev1alpha1.AttunePolicyStatus, message string) {
	if r == nil || policy == nil {
		return
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               attunev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             attunev1alpha1.ReasonInvalidConfig,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if before != nil && equality.Semantic.DeepEqual(before, &policy.Status) {
		return
	}
	desired := policy.Status.DeepCopy()
	key := types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}
	logger := log.FromContext(ctx)
	for attempt := range 3 {
		policy.Status = *desired
		err := r.writeStatusKeepingSpec(ctx, policy)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Error(err, "Failed to persist rejected-metrics unwind status")
			return
		}
		logger.Info("rejected-metrics status conflict, retrying", "attempt", attempt+1)
		if fetchErr := r.takeStoredMetaAfterConflict(ctx, key, policy); fetchErr != nil {
			logger.Error(fetchErr, "Failed to re-fetch policy for unwind status retry")
			return
		}
	}
	logger.Error(fmt.Errorf("exhausted retries"), "Failed to persist rejected-metrics unwind status")
}

func sloSkipMessage(namespace string) string {
	return fmt.Sprintf("An SLO guardrail was not sent because it is not limited to namespace %q.", namespace)
}

// recordSLOGuardrailSkips records queries that were not sent, and scoped
// queries that returned no samples. Skip mode keeps
// SLOGuardrailNoTenantCredentials. QueryRejected and that skip reason
// clear when a later pass has nothing left unsent. SLOGuardrailNoSamples
// clears only when sampled names a guardrail that returned a finite value.
// A pass with no query (no tracked pods, an open window, or a transport
// error) leaves NoSamples in place.
func (r *AttunePolicyReconciler) recordSLOGuardrailSkips(ctx context.Context, policy *attunev1alpha1.AttunePolicy, collector rsmetrics.MetricsCollector, skipped, empty, sampled []string) {
	if policy == nil {
		return
	}
	if len(skipped) > 0 {
		r.setSLOGuardrailCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonSLOGuardrailQueryRejected, sloSkipMessage(policy.Namespace))
		return
	}
	if len(empty) > 0 {
		r.setSLOGuardrailCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonSLOGuardrailNoSamples, sloNoSamplesMessage(policy.Namespace, empty))
		return
	}
	if r.sloAuthMode(ctx, collector) == safety.SLOAuthSkip {
		return
	}
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSLOGuardrails)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return
	}
	if cond.Reason == attunev1alpha1.ReasonSLOGuardrailNoSamples && len(sampled) == 0 {
		return
	}
	if cond.Reason != attunev1alpha1.ReasonSLOGuardrailQueryRejected &&
		cond.Reason != attunev1alpha1.ReasonSLOGuardrailNoTenantCredentials &&
		cond.Reason != attunev1alpha1.ReasonSLOGuardrailNoSamples {
		return
	}
	r.setSLOGuardrailCondition(policy, metav1.ConditionFalse, attunev1alpha1.ReasonSLOGuardrailScoped, "Tenant SLO guardrails are limited to the policy namespace.")
}

func sloNoSamplesMessage(namespace string, names []string) string {
	listed := strings.Join(names, ", ")
	return fmt.Sprintf("SLO guardrail %s returned no samples after it was limited to namespace %q, so it did not revert. A series whose namespace label is not that namespace will not match. Move the guardrail to AttuneDefaults, or give the policy its own Prometheus credentials.", listed, namespace)
}
