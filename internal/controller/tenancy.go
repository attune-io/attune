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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
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

func sloSkipMessage(namespace string) string {
	return fmt.Sprintf("An SLO guardrail was not sent because it is not limited to namespace %q.", namespace)
}

// recordSLOGuardrailSkips records queries that were not sent. Skip mode
// keeps SLOGuardrailNoTenantCredentials. Once a later pass sends the
// queries, that reason and a stale QueryRejected are cleared.
func (r *AttunePolicyReconciler) recordSLOGuardrailSkips(ctx context.Context, policy *attunev1alpha1.AttunePolicy, collector rsmetrics.MetricsCollector, names []string) {
	if policy == nil {
		return
	}
	if len(names) > 0 {
		r.setSLOGuardrailCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonSLOGuardrailQueryRejected, sloSkipMessage(policy.Namespace))
		return
	}
	if r.sloAuthMode(ctx, collector) == safety.SLOAuthSkip {
		return
	}
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSLOGuardrails)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return
	}
	if cond.Reason != attunev1alpha1.ReasonSLOGuardrailQueryRejected &&
		cond.Reason != attunev1alpha1.ReasonSLOGuardrailNoTenantCredentials {
		return
	}
	r.setSLOGuardrailCondition(policy, metav1.ConditionFalse, "Scoped", "Tenant SLO guardrails are limited to the policy namespace.")
}
