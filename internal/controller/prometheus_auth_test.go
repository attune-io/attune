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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func TestBuildCollectorOptions_PolicySecretWinsOverOperatorAuth(t *testing.T) {
	scheme := testScheme()
	policySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "vpa-test"},
		Data:       map[string][]byte{"token": []byte("policy-token")},
	}
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"token": []byte("operator-token")},
	}
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(policySecret, opSecret).Build()
	r.PrometheusUseServiceAccountToken = true
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.OperatorNamespace = "attune-system"
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator SA token must not be read when policy secret is set")
		return "", nil
	}

	cfg := &attunev1alpha1.PrometheusConfig{
		Address: "https://thanos-querier.openshift-monitoring.svc:9091",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "prom-token",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "policy-token", opts.BearerToken)
}

func TestBuildCollectorOptions_OperatorSecretUsesOperatorNamespace(t *testing.T) {
	scheme := testScheme()
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"token": []byte("operator-token")},
	}
	wrongNS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "vpa-test"},
		Data:       map[string][]byte{"token": []byte("policy-copy")},
	}
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(opSecret, wrongNS).Build()
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.PrometheusBearerTokenSecretKey = "token"
	r.OperatorNamespace = "attune-system"

	cfg := &attunev1alpha1.PrometheusConfig{
		Address: "https://thanos-querier.openshift-monitoring.svc:9091",
	}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "operator-token", opts.BearerToken)
}

func TestBuildCollectorOptions_ServiceAccountTokenWhenNoSecret(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		return "sa-token", nil
	}

	cfg := &attunev1alpha1.PrometheusConfig{Address: "https://thanos-querier.openshift-monitoring.svc:9091"}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "sa-token", opts.BearerToken)
}

func TestBuildCollectorOptions_InheritedNameStillReadsPolicyNamespace(t *testing.T) {
	scheme := testScheme()
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		return "sa-token", nil
	}

	cfg := &attunev1alpha1.PrometheusConfig{
		Address: "https://thanos-querier.openshift-monitoring.svc:9091",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "attune-thanos-token",
			Key:  "token",
		},
	}
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vpa-test/attune-thanos-token")
}

func TestBuildCollectorOptions_OperatorSecretWinsOverSAToken(t *testing.T) {
	scheme := testScheme()
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"token": []byte("operator-token")},
	}
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(opSecret).Build()
	r.PrometheusUseServiceAccountToken = true
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.OperatorNamespace = "attune-system"
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("SA token must not be read when operator Secret is set")
		return "", nil
	}

	cfg := &attunev1alpha1.PrometheusConfig{Address: "https://thanos-querier.openshift-monitoring.svc:9091"}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "operator-token", opts.BearerToken)
}

func clusterDefaultsWithBearerToken() *attunev1alpha1.AttuneDefaults {
	return &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
					BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
						Name: "attune-thanos-token",
						Key:  "token",
					},
				},
			},
		},
	}
}

func TestResolvePrometheusConfig_AttuneDefaultsCopiesBearerTokenSecret(t *testing.T) {
	defaults := clusterDefaultsWithBearerToken()
	r := newReconcilerWithClient(defaults)
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil

	fetched, err := r.fetchDefaults(context.Background(), "vpa-test")
	require.NoError(t, err)
	cfg, err := r.resolvePrometheusConfig(context.Background(), policy, fetched)
	require.NoError(t, err)
	require.NotNil(t, cfg.BearerTokenSecret)
	assert.Equal(t, "attune-thanos-token", cfg.BearerTokenSecret.Name)
	assert.Equal(t, "token", cfg.BearerTokenSecret.Key)

	r.mergeDefaults(policy, fetched)
	require.NotNil(t, policy.Spec.MetricsSource.Prometheus)
	require.NotNil(t, policy.Spec.MetricsSource.Prometheus.BearerTokenSecret)
	assert.Equal(t, "attune-thanos-token", policy.Spec.MetricsSource.Prometheus.BearerTokenSecret.Name)
}

func TestReconcile_AttuneDefaultsBearerTokenLooksUpPolicyNamespace(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil
	defaults := clusterDefaultsWithBearerToken()
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"token": []byte("operator-token")},
	}

	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy, defaults, opSecret)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.OperatorNamespace = "attune-system"
	reconciler.readServiceAccountTokenFn = func() (string, error) {
		return "sa-token", nil
	}
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		t.Fatal("collector must not be created when the inherited Secret is missing in the policy namespace")
		return nil, nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "app-policy", Namespace: "vpa-test"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "app-policy", Namespace: "vpa-test",
	}, &updated))
	require.NotEmpty(t, updated.Status.Conditions)
	assert.Equal(t, attunev1alpha1.ReasonMetricsUnavailable, updated.Status.Conditions[0].Reason)
	assert.Contains(t, updated.Status.Conditions[0].Message, "vpa-test/attune-thanos-token")
}

func TestReconcile_OperatorSATokenWhenDefaultsHaveAddressOnly(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
				},
			},
		},
	}
	mc := &mockCollector{}
	var gotToken string
	reconciler, _ := newReconcilerForReconcile(mc, policy, defaults)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.readServiceAccountTokenFn = func() (string, error) {
		return "sa-token", nil
	}
	reconciler.MetricsFactory = func(_ string, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		if opts != nil {
			gotToken = opts.BearerToken
		}
		return mc, nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "app-policy", Namespace: "vpa-test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "sa-token", gotToken)
}
