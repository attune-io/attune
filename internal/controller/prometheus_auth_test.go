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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
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
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
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
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
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
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{policySetBearer: true})
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
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
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
	cfg, discovered, err := r.resolvePrometheusConfig(context.Background(), policy, fetched)
	require.NoError(t, err)
	assert.False(t, discovered)
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

	reconciler, _ := newReconcilerForReconcile(&mockCollector{}, policy, defaults, opSecret)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.OperatorNamespace = "attune-system"
	reconciler.readServiceAccountTokenFn = func() (string, error) {
		return "sa-token", nil
	}
	var gotToken string
	reconciler.MetricsFactory = func(_ string, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		if opts != nil {
			gotToken = opts.BearerToken
		}
		return &mockCollector{}, nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "app-policy", Namespace: "vpa-test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "sa-token", gotToken, "inherited cluster bearerTokenSecret NotFound falls back to operator auth")
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

func TestBuildCollectorOptions_NamespaceAddressDoesNotGetOperatorToken(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator token must not be sent to a namespace-defaults Prometheus address")
		return "", nil
	}
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://team-prometheus.ns.svc:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{namespaceSetAddress: true})
	require.NoError(t, err)
	if opts != nil {
		assert.Empty(t, opts.BearerToken)
	}
}

func TestBuildCollectorOptions_TenantAddressDoesNotGetOperatorToken(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator SA token must not be sent to a policy-chosen address")
		return "", nil
	}
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://evil.tenant.svc:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{policySetAddress: true})
	require.NoError(t, err)
	if opts != nil {
		assert.Empty(t, opts.BearerToken)
	}
}

func TestBuildCollectorOptions_AuthorizationHeaderSkipsOperatorToken(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator token must not overwrite Authorization headers")
		return "sa-token", nil
	}
	cfg := &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		Headers: map[string]string{"authorization": "Basic dXNlcjpwYXNz"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Empty(t, opts.BearerToken)
	assert.Equal(t, "Basic dXNlcjpwYXNz", opts.Headers["authorization"])
}

func TestBuildCollectorOptions_InheritedBearerNotFoundFallsBack(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) { return "sa-token", nil }
	cfg := &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "attune-thanos-token",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "sa-token", opts.BearerToken)
}

func TestBuildCollectorOptions_OperatorSecretMissing(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.OperatorNamespace = "attune-system"
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"}
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attune-thanos-token")
}

func TestBuildCollectorOptions_OperatorSecretMissingKey(t *testing.T) {
	scheme := testScheme()
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(opSecret).Build()
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.PrometheusBearerTokenSecretKey = "token"
	r.OperatorNamespace = "attune-system"
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"}
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestBuildCollectorOptions_OperatorSecretEmptyToken(t *testing.T) {
	scheme := testScheme()
	opSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attune-thanos-token", Namespace: "attune-system"},
		Data:       map[string][]byte{"token": []byte("  \n")},
	}
	r := NewAttunePolicyReconciler()
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(opSecret).Build()
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	r.OperatorNamespace = "attune-system"
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"}
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestBuildCollectorOptions_OperatorNamespaceRequiredForSecret(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusBearerTokenSecretName = "attune-thanos-token"
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"}
	_, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POD_NAMESPACE")
}

func TestReadServiceAccountToken_FileTrimAndEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("  sa-from-file\n"), 0o600))
	prev := prometheusSATokenPath
	prometheusSATokenPath = path
	t.Cleanup(func() { prometheusSATokenPath = prev })

	r := NewAttunePolicyReconciler()
	token, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "sa-from-file", token)

	require.NoError(t, os.WriteFile(path, []byte(" \n"), 0o600))
	_, err = r.readServiceAccountToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestReadServiceAccountToken_QueryServiceAccount(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "query-sa-token"}}, nil
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	token, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "query-sa-token", token)
}

func TestReadServiceAccountToken_QueryServiceAccountCached(t *testing.T) {
	creates := 0
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		creates++
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token:               fmt.Sprintf("tok-%d", creates),
				ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour)),
			},
		}, nil
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	r.SetNowFunc(func() time.Time { return now })

	tok1, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	tok2, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, tok1, tok2)
	assert.Equal(t, 1, creates)

	now = now.Add(56 * time.Minute)
	tok3, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, creates)
	assert.NotEqual(t, tok1, tok3)
}

func TestReadServiceAccountToken_QueryServiceAccountNilClientset(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	_, err := r.readServiceAccountToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clientset")
}

func TestReadServiceAccountToken_QueryServiceAccountCreateTokenError(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden")
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	_, err := r.readServiceAccountToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attune-prometheus-query")
	assert.Contains(t, err.Error(), "forbidden")
}

func TestReadServiceAccountToken_QueryServiceAccountEmptyToken(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: "  \n"},
		}, nil
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	_, err := r.readServiceAccountToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty token")
}

func TestReadServiceAccountToken_QueryServiceAccountZeroExpiryCaches(t *testing.T) {
	creates := 0
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		creates++
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token: fmt.Sprintf("tok-%d", creates),
			},
		}, nil
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	r.SetNowFunc(func() time.Time { return now })

	tok1, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	tok2, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, tok1, tok2)
	assert.Equal(t, 1, creates)

	now = now.Add(56 * time.Minute)
	tok3, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, creates)
	assert.NotEqual(t, tok1, tok3)
}

func TestReadServiceAccountToken_QueryServiceAccountRefreshErrorKeepsCachedToken(t *testing.T) {
	creates := 0
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates > 1 {
			return true, nil, fmt.Errorf("apiserver unavailable")
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token:               "tok-1",
				ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour)),
			},
		}, nil
	})
	r := NewAttunePolicyReconciler()
	r.Clientset = cs
	r.OperatorNamespace = "attune-system"
	r.PrometheusQueryServiceAccount = "attune-prometheus-query"
	r.SetNowFunc(func() time.Time { return now })

	tok1, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok-1", tok1)

	now = now.Add(56 * time.Minute)
	tok2, err := r.readServiceAccountToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, tok1, tok2)
	assert.Equal(t, 2, creates)

	now = now.Add(10 * time.Minute)
	_, err = r.readServiceAccountToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver unavailable")
	assert.Equal(t, 3, creates)
}

func TestBuildCollectorOptions_DiscoveredAddressDoesNotGetOperatorToken(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator token must not be sent to an auto-discovered Prometheus address")
		return "", nil
	}
	cfg := &attunev1alpha1.PrometheusConfig{Address: "http://prometheus-k8s.openshift-monitoring:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "vpa-test", cfg, prometheusAuthContext{addressDiscovered: true})
	require.NoError(t, err)
	if opts != nil {
		assert.Empty(t, opts.BearerToken)
	}
}

func TestFetchDefaultsForAuth_UsesSelectedNamespaceObject(t *testing.T) {
	selectedQuiet := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-overrides", Namespace: "vpa-test"},
	}
	unselected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "zzz-unused", Namespace: "vpa-test"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://unused-prom:9090"},
			},
		},
	}
	r := newReconcilerWithClient(selectedQuiet, unselected)
	_, set, _, err := r.fetchDefaultsForAuth(context.Background(), "vpa-test")
	require.NoError(t, err)
	assert.False(t, set)
	_, set, _, err = r.fetchDefaultsForAuth(context.Background(), "other")
	require.NoError(t, err)
	assert.False(t, set)

	selectedAddr := selectedQuiet.DeepCopy()
	selectedAddr.Spec.MetricsSource = &attunev1alpha1.MetricsSource{
		Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://team-prom:9090"},
	}
	r = newReconcilerWithClient(selectedAddr, unselected)
	_, set, _, err = r.fetchDefaultsForAuth(context.Background(), "vpa-test")
	require.NoError(t, err)
	assert.True(t, set)
}

func TestReconcile_UnselectedNamespaceDefaultsDoesNotBlockOperatorToken(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil
	cluster := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"},
			},
		},
	}
	selected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-overrides", Namespace: "vpa-test"},
	}
	unselected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "zzz-unused", Namespace: "vpa-test"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://unused-prom:9090"},
			},
		},
	}
	mc := &mockCollector{}
	var gotToken string
	reconciler, _ := newReconcilerForReconcile(mc, policy, cluster, selected, unselected)
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

func TestReconcile_SelectedNamespaceDefaultsAddressDoesNotGetOperatorToken(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil
	cluster := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://prometheus:9090"},
			},
		},
	}
	selected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-overrides", Namespace: "vpa-test"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://team-prom:9090"},
			},
		},
	}
	unselected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "zzz-unused", Namespace: "vpa-test"},
	}
	mc := &mockCollector{}
	var gotToken string
	reconciler, _ := newReconcilerForReconcile(mc, policy, cluster, selected, unselected)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator token must not be sent to the selected namespace-defaults address")
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
	assert.Empty(t, gotToken)
}

func TestReconcile_NamespaceDefaultsAddressDoesNotGetOperatorToken(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = nil
	nsDef := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "vpa-test"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{Address: "http://team-prom:9090"},
			},
		},
	}
	mc := &mockCollector{}
	var gotToken string
	reconciler, _ := newReconcilerForReconcile(mc, policy, nsDef)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.readServiceAccountTokenFn = func() (string, error) {
		t.Fatal("operator token must not be sent to a namespace-defaults Prometheus address")
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
	assert.Empty(t, gotToken)
}

func TestReconcile_PolicyBearerMissingDoesNotFallBack(t *testing.T) {
	policy := newTestPolicy("app-policy", "vpa-test")
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "prom-token",
			Key:  "token",
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.PrometheusUseServiceAccountToken = true
	reconciler.readServiceAccountTokenFn = func() (string, error) { return "sa-token", nil }
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
	assert.Contains(t, updated.Status.Conditions[0].Message, "vpa-test/prom-token")
}
