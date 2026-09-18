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
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// SecretAccessChecker reports whether the admission user may get a Secret.
type SecretAccessChecker interface {
	CanGetSecret(ctx context.Context, req admission.Request, namespace, name string) (bool, error)
}

type sarSecretChecker struct {
	client kubernetes.Interface
}

// NewSARSecretChecker reviews get access on Secrets as the admission user.
func NewSARSecretChecker(cs kubernetes.Interface) SecretAccessChecker {
	return &sarSecretChecker{client: cs}
}

func (c *sarSecretChecker) CanGetSecret(ctx context.Context, req admission.Request, namespace, name string) (bool, error) {
	var extra map[string]authorizationv1.ExtraValue
	if len(req.UserInfo.Extra) > 0 {
		extra = make(map[string]authorizationv1.ExtraValue, len(req.UserInfo.Extra))
		for k, v := range req.UserInfo.Extra {
			extra[k] = authorizationv1.ExtraValue(v)
		}
	}
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   req.UserInfo.Username,
			Groups: req.UserInfo.Groups,
			UID:    req.UserInfo.UID,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "get",
				Resource:  "secrets",
				Name:      name,
			},
		},
	}
	out, err := c.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

func policySecretNames(policy *attunev1alpha1.AttunePolicy) []string {
	seen := make(map[string]struct{})
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	ms := policy.Spec.MetricsSource
	if ms.Prometheus != nil && ms.Prometheus.BearerTokenSecret != nil {
		add(ms.Prometheus.BearerTokenSecret.Name)
	}
	if ms.Datadog != nil {
		add(ms.Datadog.APIKeySecretRef.Name)
	}
	if us := policy.Spec.UpdateStrategy; us != nil && us.Export != nil &&
		us.Export.PullRequest != nil && us.Export.PullRequest.TokenSecretRef != nil {
		add(us.Export.PullRequest.TokenSecretRef.Name)
	}
	return names
}

func admissionRequest(ctx context.Context) (admission.Request, bool) {
	req, err := admission.RequestFromContext(ctx)
	return req, err == nil
}

func (v *AttunePolicyValidator) checkReferencedSecretAccess(ctx context.Context, policy *attunev1alpha1.AttunePolicy) error {
	if v.SecretAccess == nil {
		return nil
	}
	req, ok := admissionRequest(ctx)
	if !ok {
		return nil
	}
	ns := policy.Namespace
	for _, name := range policySecretNames(policy) {
		allowed, err := v.SecretAccess.CanGetSecret(ctx, req, ns, name)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("the admission user must have get on Secret %q in namespace %q", name, ns)
		}
	}
	return nil
}
