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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/safety"
)

func TestReconcile_CrossNamespaceVPAIsInvalidConfig(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "other-vpa", Namespace: "team-b"}

	scheme := testScheme()
	reads := 0
	c := fakeClientWithCrossNSGuard(t, scheme, &reads, policy)
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	built := false
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		built = true
		return &mockCollector{}, nil
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, time.Minute, result.RequeueAfter)
	assert.False(t, built)
	assert.Equal(t, 0, reads)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "team-b")
}

func TestReconcile_NamespaceDefaultsCrossNamespaceVPAIsInvalidConfig(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus = nil
	ns := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "ns", Namespace: "team-a"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				VPA: &attunev1alpha1.VPAConfig{Name: "other-vpa", Namespace: "team-b"},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "team-b")
}

func TestReconcile_ClusterDefaultsVPANamespaceIsAllowed(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus = nil
	cluster := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				VPA: &attunev1alpha1.VPAConfig{Name: "shared", Namespace: "monitoring"},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, cluster).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	if cond != nil {
		assert.NotContains(t, cond.Message, "must be")
	}
}

func TestReconcile_PolicySigV4NotAllowlistedDoesNotBuildCollector(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus.SigV4 = &attunev1alpha1.SigV4Config{
		Region:  "us-east-1",
		RoleARN: "arn:aws:iam::123456789012:role/prod-amp-reader",
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	built := false
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		built = true
		return &mockCollector{}, nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	assert.False(t, built)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "--sigv4-allowed-role-arns")
}

func TestReconcile_SigV4WithoutRoleRequiresHostAllowlist(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus.Address = "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws"
	policy.Spec.MetricsSource.Prometheus.SigV4 = &attunev1alpha1.SigV4Config{Region: "us-east-1"}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Contains(t, cond.Message, "--sigv4-allowed-workspace-hosts")
}

func TestReconcile_ClusterDefaultsSigV4IsAllowed(t *testing.T) {
	policy := newTestPolicy("probe", "team-a")
	policy.Spec.MetricsSource.Prometheus = nil
	cluster := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://127.0.0.1:9",
					SigV4: &attunev1alpha1.SigV4Config{
						Region:  "us-east-1",
						RoleARN: "arn:aws:iam::123456789012:role/prod-amp-reader",
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, cluster).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "probe", Namespace: "team-a"},
	})
	require.NoError(t, err)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "probe", Namespace: "team-a"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	if cond != nil {
		assert.NotContains(t, cond.Message, "--sigv4-allowed-role-arns")
	}
}

func TestBuildCollectorOptions_NamespacedSigV4SetsExternalID(t *testing.T) {
	r := NewAttunePolicyReconciler()
	opts, err := r.buildCollectorOptions(context.Background(), "team-a", &attunev1alpha1.PrometheusConfig{
		Address: "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws",
		SigV4: &attunev1alpha1.SigV4Config{
			Region:  "us-east-1",
			RoleARN: "arn:aws:iam::123456789012:role/attune-amp",
		},
	}, prometheusAuthContext{policySetAddress: true})
	require.NoError(t, err)
	require.NotNil(t, opts.SigV4)
	assert.Equal(t, "attune:team-a", opts.SigV4.ExternalID)
}

type operatorCollector struct {
	mockCollector
	queries []string
}

func (o *operatorCollector) UsesOperatorAuth() bool { return true }

func (o *operatorCollector) Query(_ context.Context, query string, _ time.Time) (float64, error) {
	o.queries = append(o.queries, query)
	return 1234.5, nil
}

func TestSLOTenancy_DatadogAndCloudWatchDoNotHold(t *testing.T) {
	r := NewAttunePolicyReconciler()
	guardrails := []attunev1alpha1.SLOGuardrail{{
		Name: "x", Query: "sum(foo)", Threshold: "1", Comparison: "above",
	}}
	dd, err := rsmetrics.NewDatadogCollector("datadoghq.com", "key", "", logr.Discard())
	require.NoError(t, err)
	ddWrapped := rsmetrics.NewRateLimitedCollector(dd, 10, 1)
	assert.False(t, rsmetrics.CollectorSupportsSLO(ddWrapped))
	assert.False(t, r.sloQuerierActive(context.Background(), ddWrapped, "team-a", guardrails))
	monitor := r.newSafetyMonitorIn(context.Background(), logr.Discard(), nil, ddWrapped, guardrails)
	assert.False(t, monitor.HasSLOChecker())

	cw := rsmetrics.NewCloudWatchCollectorWithClient(nil, "prod", logr.Discard())
	cwWrapped := rsmetrics.NewRateLimitedCollector(cw, 10, 1)
	assert.False(t, rsmetrics.CollectorSupportsSLO(cwWrapped))
	assert.False(t, r.sloQuerierActive(context.Background(), cwWrapped, "team-a", guardrails))
	assert.Equal(t, 5*time.Minute, canaryPromotionWait(policyWithGuardrails("team-a"), 5*time.Minute, false))
}

func TestSLOTenancy_EnforceRewritesAndSkipDoesNotQuery(t *testing.T) {
	guardrails := []attunev1alpha1.SLOGuardrail{{
		Name: "x", Query: "sum(foo)", Threshold: "1", Comparison: "above",
	}}
	policy := policyWithGuardrails("team-a")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "team-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	ctx := context.WithValue(context.Background(), sloPlanKey{}, sloPlan{tenantGuardrails: true})
	col := &operatorCollector{}
	r := NewAttunePolicyReconciler()
	r.Clientset = kubefake.NewSimpleClientset(pod)
	r.SLOGuardrailEnforceNamespace = true
	monitor := r.newSafetyMonitorIn(ctx, logr.Discard(), policy, col, guardrails)
	require.True(t, monitor.HasSLOChecker())
	verdict, err := monitor.CheckPod(context.Background(), safety.ResizeRecord{
		PodName: "web-0", Namespace: "team-a", Container: "app",
		ResizedAt: time.Now().Add(-6 * time.Minute), WorkloadName: "web",
	}, time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	require.NotEmpty(t, col.queries)
	assert.Contains(t, col.queries[0], `namespace="team-a"`)
	assert.NotContains(t, verdict.Message, "1234")

	skipped := &operatorCollector{}
	r.SLOGuardrailEnforceNamespace = false
	monitor = r.newSafetyMonitorIn(ctx, logr.Discard(), policy, skipped, guardrails)
	assert.False(t, monitor.HasSLOChecker())
	assert.False(t, r.sloQuerierActive(ctx, skipped, policy.Namespace, guardrails))
	verdict, err = monitor.CheckPod(context.Background(), safety.ResizeRecord{
		PodName: "web-0", Namespace: "team-a", Container: "app",
		ResizedAt: time.Now().Add(-30 * time.Second), WorkloadName: "web",
	}, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
	assert.False(t, verdict.SLODeferred)
	assert.Empty(t, skipped.queries)
}

func TestSLOTenancy_DefaultsGuardrailIsNotRewritten(t *testing.T) {
	guardrails := []attunev1alpha1.SLOGuardrail{{
		Name: "x", Query: "sum(foo)", Threshold: "1", Comparison: "above",
	}}
	policy := policyWithGuardrails("team-a")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "team-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app"},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	col := &operatorCollector{}
	r := NewAttunePolicyReconciler()
	r.Clientset = kubefake.NewSimpleClientset(pod)
	monitor := r.newSafetyMonitorIn(context.Background(), logr.Discard(), policy, col, guardrails)
	_, err := monitor.CheckPod(context.Background(), safety.ResizeRecord{
		PodName: "web-0", Namespace: "team-a", Container: "app",
		ResizedAt: time.Now().Add(-6 * time.Minute), WorkloadName: "web",
	}, time.Now())
	require.NoError(t, err)
	require.NotEmpty(t, col.queries)
	assert.NotContains(t, col.queries[0], "namespace=")
}

func TestRecordSLOGuardrailSkipsClearsStaleRejection(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := policyWithGuardrails("team-a")
	r.setSLOGuardrailCondition(policy, metav1.ConditionTrue, attunev1alpha1.ReasonSLOGuardrailQueryRejected, "old")
	r.recordSLOGuardrailSkips(context.Background(), policy, &mockCollector{}, nil)
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSLOGuardrails)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Scoped", cond.Reason)

	r.SLOGuardrailEnforceNamespace = false
	ctx := context.WithValue(context.Background(), sloPlanKey{}, sloPlan{tenantGuardrails: true})
	r.noteSkippedTenantGuardrail(policy)
	r.recordSLOGuardrailSkips(ctx, policy, &operatorCollector{}, nil)
	cond = meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSLOGuardrails)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonSLOGuardrailNoTenantCredentials, cond.Reason)

	r.SLOGuardrailEnforceNamespace = true
	r.recordSLOGuardrailSkips(ctx, policy, &operatorCollector{}, nil)
	cond = meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionSLOGuardrails)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Scoped", cond.Reason)
}

func TestSLOTenancy_RejectedQueryDoesNotExtendCanary(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.SLOGuardrailEnforceNamespace = true
	ctx := context.WithValue(context.Background(), sloPlanKey{}, sloPlan{tenantGuardrails: true})
	policy := policyWithGuardrails("team-a")
	policy.Spec.UpdateStrategy.SLOGuardrails[0].Query = `foo{namespace="other"}`
	policy.Spec.UpdateStrategy.SLOGuardrails[0].EvaluationWindow = &metav1.Duration{Duration: 30 * time.Minute}
	assert.False(t, r.sloQuerierActive(ctx, &operatorCollector{}, policy.Namespace, policy.Spec.UpdateStrategy.SLOGuardrails))
	assert.Equal(t, 5*time.Minute, canaryPromotionWait(policy, 5*time.Minute, false))

	policy.Spec.UpdateStrategy.SLOGuardrails[0].Query = "sum(foo)"
	assert.True(t, r.sloQuerierActive(ctx, &operatorCollector{}, policy.Namespace, policy.Spec.UpdateStrategy.SLOGuardrails))
	assert.Equal(t, 30*time.Minute, canaryPromotionWait(policy, 5*time.Minute, true))

	policy.Spec.UpdateStrategy.SLOGuardrails[0].Query = `sum(foo{pod="{{ .PodName }}"})`
	assert.True(t, r.sloQuerierActive(ctx, &operatorCollector{}, policy.Namespace, policy.Spec.UpdateStrategy.SLOGuardrails))
}

func policyWithGuardrails(namespace string) *attunev1alpha1.AttunePolicy {
	return &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: namespace},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				SLOGuardrails: []attunev1alpha1.SLOGuardrail{{
					Name: "x", Query: "sum(foo)", Threshold: "1", Comparison: "above",
				}},
			},
		},
	}
}

func fakeClientWithCrossNSGuard(t *testing.T, scheme *runtime.Scheme, reads *int, policy *attunev1alpha1.AttunePolicy) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Namespace == "team-b" {
					*reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}
