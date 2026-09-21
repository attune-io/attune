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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
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
	r.PrometheusTokenFile = filepath.Join(t.TempDir(), "missing")

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
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("sa-token\n"), 0o600))

	r := NewAttunePolicyReconciler()
	r.Scheme = testScheme()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.PrometheusUseServiceAccountToken = true
	r.PrometheusTokenFile = path

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
	r.PrometheusTokenFile = filepath.Join(t.TempDir(), "token")

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
