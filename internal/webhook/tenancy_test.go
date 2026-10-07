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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/validation"
)

func TestPolicyVPANamespace(t *testing.T) {
	t.Parallel()
	validator := &AttunePolicyValidator{}
	same := validPolicy()
	same.Namespace = "team-a"
	same.Spec.MetricsSource.Prometheus = nil
	same.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "rec", Namespace: "team-a"}
	_, err := validator.ValidateCreate(context.Background(), same)
	require.NoError(t, err)

	empty := same.DeepCopy()
	empty.Spec.MetricsSource.VPA.Namespace = ""
	_, err = validator.ValidateCreate(context.Background(), empty)
	require.NoError(t, err)

	other := same.DeepCopy()
	other.Spec.MetricsSource.VPA.Namespace = "team-b"
	_, err = validator.ValidateCreate(context.Background(), other)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team-b")
}

func TestDefaultsVPANamespaceIsUnrestricted(t *testing.T) {
	t.Parallel()
	_, err := (&AttuneDefaultsValidator{}).ValidateCreate(context.Background(), &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				VPA: &attunev1alpha1.VPAConfig{Name: "rec", Namespace: "monitoring"},
			},
		},
	})
	require.NoError(t, err)
}

func TestNamespaceDefaultsVPANamespace(t *testing.T) {
	t.Parallel()
	validator := &AttuneNamespaceDefaultsValidator{}
	obj := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "ns", Namespace: "team-a"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				VPA: &attunev1alpha1.VPAConfig{Name: "rec", Namespace: "team-b"},
			},
		},
	}
	_, err := validator.ValidateCreate(context.Background(), obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team-b")
}

func TestPolicySigV4Allowlist(t *testing.T) {
	t.Parallel()
	role := "arn:aws:iam::123456789012:role/attune-amp"
	address := "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws"
	validator := &AttunePolicyValidator{SigV4Allowlist: validation.SigV4Allowlist{
		RoleARNs: []string{"arn:aws:iam::123456789012:role/attune-*"},
		Hosts:    []string{"aps-workspaces.us-east-1.amazonaws.com"},
	}}
	policy := validPolicy()
	policy.Namespace = "team-a"
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: address,
		SigV4:   &attunev1alpha1.SigV4Config{Region: "us-east-1", RoleARN: role},
	}
	_, err := validator.ValidateCreate(context.Background(), policy)
	require.NoError(t, err)

	policy.Spec.MetricsSource.Prometheus.SigV4.RoleARN = ""
	_, err = validator.ValidateCreate(context.Background(), policy)
	require.NoError(t, err)

	denied := &AttunePolicyValidator{}
	policy.Spec.MetricsSource.Prometheus.SigV4.RoleARN = role
	_, err = denied.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sigv4-allowed-role-arns")
}

func TestClusterDefaultsSigV4AllowlistEmptyIsAccepted(t *testing.T) {
	t.Parallel()
	_, err := (&AttuneDefaultsValidator{}).ValidateCreate(context.Background(), &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws",
					SigV4: &attunev1alpha1.SigV4Config{
						Region:  "us-east-1",
						RoleARN: "arn:aws:iam::123456789012:role/prod-amp-reader",
					},
				},
			},
		},
	})
	require.NoError(t, err)
}

func TestCloudWatchRoleAllowlist(t *testing.T) {
	t.Parallel()
	role := "arn:aws:iam::123456789012:role/attune-cw"
	policy := validPolicy()
	policy.Namespace = "team-a"
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.CloudWatch = &attunev1alpha1.CloudWatchConfig{
		Region: "us-east-1", ClusterName: "prod", RoleARN: role,
	}
	_, err := (&AttunePolicyValidator{}).ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sigv4-allowed-role-arns")

	allowed := &AttunePolicyValidator{SigV4Allowlist: validation.SigV4Allowlist{RoleARNs: []string{role}}}
	_, err = allowed.ValidateCreate(context.Background(), policy)
	require.NoError(t, err)

	_, err = (&AttuneDefaultsValidator{}).ValidateCreate(context.Background(), &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region: "us-east-1", ClusterName: "prod", RoleARN: role,
				},
			},
		},
	})
	require.NoError(t, err)
}

func TestSLOGuardrailDatadogWarns(t *testing.T) {
	t.Parallel()
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		Site:            "datadoghq.com",
		APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd", Key: "api-key"},
	}
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{{
		Name: "x", Query: "avg:system.cpu.user{*}", Threshold: "1", Comparison: "above",
	}}
	warnings, err := (&AttunePolicyValidator{}).ValidateCreate(context.Background(), policy)
	require.NoError(t, err)
	assert.Contains(t, warnings, sloNonPrometheusWarning)
}
