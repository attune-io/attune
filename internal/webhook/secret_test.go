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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

type secretAccessCall struct {
	namespace string
	name      string
	user      string
}

type fakeSecretAccess struct {
	allowed bool
	err     error
	calls   []secretAccessCall
}

func (f *fakeSecretAccess) CanGetSecret(ctx context.Context, req admission.Request, namespace, name string) (bool, error) {
	f.calls = append(f.calls, secretAccessCall{
		namespace: namespace,
		name:      name,
		user:      req.UserInfo.Username,
	})
	return f.allowed, f.err
}

func admissionUserCtx(user string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: user},
		},
	})
}

func policyWithBearerSecret(ns, secretName string) *attunev1alpha1.AttunePolicy {
	p := validPolicy()
	p.Namespace = ns
	p.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: secretName,
			Key:  "token",
		},
	}
	return p
}

func policyWithGitOpsToken(ns, secretName string) *attunev1alpha1.AttunePolicy {
	p := validPolicy()
	p.Namespace = ns
	p.Spec.UpdateStrategy.Export = &attunev1alpha1.ExportConfig{
		PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
			TokenSecretRef: &attunev1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  "token",
			},
		},
	}
	return p
}

func TestValidateCreate_SecretAccessDenied(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `Secret "prom-token"`)
	assert.Contains(t, err.Error(), `namespace "apps"`)
	require.Len(t, checker.calls, 1)
	assert.Equal(t, "apps", checker.calls[0].namespace)
	assert.Equal(t, "prom-token", checker.calls[0].name)
	assert.Equal(t, "alice", checker.calls[0].user)
}

func TestValidateUpdate_SecretAccessDenied(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateUpdate(admissionUserCtx("alice"), policy, policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `Secret "prom-token"`)
}

func TestValidateCreate_SecretAccessAllowed(t *testing.T) {
	checker := &fakeSecretAccess{allowed: true}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	assert.NoError(t, err)
	require.Len(t, checker.calls, 1)
	assert.Equal(t, "prom-token", checker.calls[0].name)
}

func TestValidateCreate_GitOpsTokenSecretAccessDenied(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithGitOpsToken("apps", "gitops-token")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `Secret "gitops-token"`)
	assert.Contains(t, err.Error(), `namespace "apps"`)
	require.Len(t, checker.calls, 1)
	assert.Equal(t, "gitops-token", checker.calls[0].name)
}

func TestValidateCreate_NilSecretAccessSkipsSAR(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	assert.NoError(t, err)
}

func TestValidateCreate_MissingAdmissionRequestSkipsSAR(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, checker.calls)
}

func TestValidateCreate_SecretAccessErrorFailsClosed(t *testing.T) {
	checker := &fakeSecretAccess{err: errors.New("authorization review failed")}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorization review failed")
}

func TestValidateCreate_EmptySecretNameSkipsSAR(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "")

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	assert.NoError(t, err)
	assert.Empty(t, checker.calls)
}

func TestValidateCreate_DedupsSecretNames(t *testing.T) {
	checker := &fakeSecretAccess{allowed: true}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "shared-token")
	policy.Spec.UpdateStrategy.Export = &attunev1alpha1.ExportConfig{
		PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
			TokenSecretRef: &attunev1alpha1.SecretKeyRef{Name: "shared-token", Key: "token"},
		},
	}

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	assert.NoError(t, err)
	require.Len(t, checker.calls, 1)
	assert.Equal(t, "shared-token", checker.calls[0].name)
}

func TestValidateCreate_InvalidPolicySkipsSAR(t *testing.T) {
	checker := &fakeSecretAccess{allowed: false}
	validator := &AttunePolicyValidator{SecretAccess: checker}
	policy := policyWithBearerSecret("apps", "prom-token")
	policy.Spec.MetricsSource.Prometheus.BearerTokenSecret.Name = "other-ns/token"

	_, err := validator.ValidateCreate(admissionUserCtx("alice"), policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain '/'")
	assert.Empty(t, checker.calls)
}

func TestSARSecretChecker_CanGetSecret(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		require.True(t, ok)
		sar, ok := create.GetObject().(*authorizationv1.SubjectAccessReview)
		require.True(t, ok)
		assert.Equal(t, "alice", sar.Spec.User)
		assert.Equal(t, []string{"devs"}, sar.Spec.Groups)
		assert.Equal(t, "uid-1", sar.Spec.UID)
		assert.Equal(t, authorizationv1.ExtraValue{"impersonated"}, sar.Spec.Extra["scopes.authorization.k8s.io"])
		require.NotNil(t, sar.Spec.ResourceAttributes)
		assert.Equal(t, "apps", sar.Spec.ResourceAttributes.Namespace)
		assert.Equal(t, "get", sar.Spec.ResourceAttributes.Verb)
		assert.Equal(t, "secrets", sar.Spec.ResourceAttributes.Resource)
		assert.Equal(t, "prom-token", sar.Spec.ResourceAttributes.Name)
		sar.Status.Allowed = true
		return true, sar, nil
	})

	checker := NewSARSecretChecker(cs)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{
				Username: "alice",
				UID:      "uid-1",
				Groups:   []string{"devs"},
				Extra: map[string]authenticationv1.ExtraValue{
					"scopes.authorization.k8s.io": {"impersonated"},
				},
			},
		},
	}

	allowed, err := checker.CanGetSecret(context.Background(), req, "apps", "prom-token")

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestSARSecretChecker_DeniedAndError(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		sar := create.GetObject().(*authorizationv1.SubjectAccessReview)
		if sar.Spec.ResourceAttributes.Name == "boom" {
			return true, nil, errors.New("apiserver unavailable")
		}
		sar.Status.Allowed = false
		return true, sar, nil
	})
	checker := NewSARSecretChecker(cs)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: "bob"},
		},
	}

	allowed, err := checker.CanGetSecret(context.Background(), req, "apps", "hidden")
	require.NoError(t, err)
	assert.False(t, allowed)

	_, err = checker.CanGetSecret(context.Background(), req, "apps", "boom")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver unavailable")
}
