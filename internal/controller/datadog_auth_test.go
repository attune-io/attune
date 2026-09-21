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

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func datadogPolicy(ns, secretName string) *attunev1alpha1.AttunePolicy {
	return &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.com",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: secretName, Key: "api-key"},
				},
			},
		},
	}
}

func ddSecret(ns, name, apiKey, appKey string) *corev1.Secret {
	data := map[string][]byte{"api-key": []byte(apiKey)}
	if appKey != "" {
		data["app-key"] = []byte(appKey)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
}

func TestResolveDatadogCollector_ClusterKeyUsesOperatorSecretNotPolicyCopy(t *testing.T) {
	policyNS := ddSecret("vpa-test", "dd-keys", "policy-copy-key", "policy-app")
	operator := ddSecret("attune-system", "datadog-keys", "operator-api-key", "operator-app")
	r := newReconcilerWithClient(policyNS, operator)
	r.OperatorNamespace = "attune-system"
	r.DatadogAPIKeySecretName = "datadog-keys"
	r.DatadogAPIKeySecretKey = "api-key"

	// Merged policy carries the inherited name. Cluster-chosen auth must ignore it.
	_, _, err := r.resolveDatadogCollector(context.Background(), datadogPolicy("vpa-test", "dd-keys"), datadogAuthContext{})
	require.NoError(t, err)

	want := "datadog:datadoghq.com|" + secretForCacheKey("operator-api-key") + "|" + secretForCacheKey("operator-app")
	decoy := "datadog:datadoghq.com|" + secretForCacheKey("policy-copy-key") + "|" + secretForCacheKey("policy-app")
	_, ok := r.collectors.Load(want)
	assert.True(t, ok, "cluster Datadog config must use the operator-namespace API key")
	_, copied := r.collectors.Load(decoy)
	assert.False(t, copied, "inherited apiKeySecretRef name in the policy namespace is not the cluster success path")
}

func TestResolveDatadogCollector_PolicyDatadogStaysInPolicyNamespace(t *testing.T) {
	policyNS := ddSecret("vpa-test", "dd-keys", "policy-api-key", "")
	operator := ddSecret("attune-system", "datadog-keys", "operator-api-key", "")
	r := newReconcilerWithClient(policyNS, operator)
	r.OperatorNamespace = "attune-system"
	r.DatadogAPIKeySecretName = "datadog-keys"

	_, _, err := r.resolveDatadogCollector(context.Background(), datadogPolicy("vpa-test", "dd-keys"), datadogAuthContext{policySetDatadog: true})
	require.NoError(t, err)

	want := "datadog:datadoghq.com|" + secretForCacheKey("policy-api-key") + "|"
	_, ok := r.collectors.Load(want)
	assert.True(t, ok, "policy Datadog block must read apiKeySecretRef in the policy namespace")
	_, operatorHit := r.collectors.Load("datadog:datadoghq.com|" + secretForCacheKey("operator-api-key") + "|")
	assert.False(t, operatorHit)
}

func TestResolveDatadogCollector_NamespaceDefaultsStayInPolicyNamespace(t *testing.T) {
	policyNS := ddSecret("prod", "team-keys", "namespace-api-key", "")
	operator := ddSecret("attune-system", "datadog-keys", "operator-api-key", "")
	r := newReconcilerWithClient(policyNS, operator)
	r.OperatorNamespace = "attune-system"
	r.DatadogAPIKeySecretName = "datadog-keys"

	_, _, err := r.resolveDatadogCollector(context.Background(), datadogPolicy("prod", "team-keys"), datadogAuthContext{namespaceSetDatadog: true})
	require.NoError(t, err)

	_, ok := r.collectors.Load("datadog:datadoghq.com|" + secretForCacheKey("namespace-api-key") + "|")
	assert.True(t, ok, "AttuneNamespaceDefaults apiKeySecretRef stays in that namespace")
}

func TestResolveDatadogCollector_InheritedNameWithoutOperatorSecret(t *testing.T) {
	policyNS := ddSecret("vpa-test", "dd-keys", "copied-api-key", "")
	r := newReconcilerWithClient(policyNS)
	r.OperatorNamespace = "attune-system"

	_, _, err := r.resolveDatadogCollector(context.Background(), datadogPolicy("vpa-test", "dd-keys"), datadogAuthContext{})
	require.NoError(t, err)
	_, ok := r.collectors.Load("datadog:datadoghq.com|" + secretForCacheKey("copied-api-key") + "|")
	assert.True(t, ok, "without --datadog-api-key-secret the inherited name is still read in the policy namespace")
}

func TestResolveDatadogCollector_ClusterKeyIgnoresPolicyCopyWhenOperatorSecretMissing(t *testing.T) {
	policyNS := ddSecret("vpa-test", "dd-keys", "policy-copy-key", "")
	r := newReconcilerWithClient(policyNS)
	r.OperatorNamespace = "attune-system"
	r.DatadogAPIKeySecretName = "datadog-keys"

	_, _, err := r.resolveDatadogCollector(context.Background(), datadogPolicy("vpa-test", "dd-keys"), datadogAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attune-system/datadog-keys")
	assert.Zero(t, collectorCount(r), "a policy-namespace copy must not satisfy a configured operator Datadog secret")
}

func TestFetchDefaultsForAuth_DatadogFlagUsesSelectedNamespaceObject(t *testing.T) {
	selectedQuiet := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-overrides", Namespace: "vpa-test"},
	}
	unselected := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "zzz-unused", Namespace: "vpa-test"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "unused", Key: "api-key"},
				},
			},
		},
	}
	r := newReconcilerWithClient(selectedQuiet, unselected)
	_, _, setDD, err := r.fetchDefaultsForAuth(context.Background(), "vpa-test")
	require.NoError(t, err)
	assert.False(t, setDD, "unselected AttuneNamespaceDefaults must not mark Datadog as namespace-chosen")

	selected := selectedQuiet.DeepCopy()
	selected.Spec.MetricsSource = &attunev1alpha1.MetricsSource{
		Datadog: &attunev1alpha1.DatadogConfig{
			APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "team-keys", Key: "api-key"},
		},
	}
	r = newReconcilerWithClient(selected, unselected)
	_, _, setDD, err = r.fetchDefaultsForAuth(context.Background(), "vpa-test")
	require.NoError(t, err)
	assert.True(t, setDD)
}

func collectorCount(r *AttunePolicyReconciler) int {
	n := 0
	r.collectors.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
