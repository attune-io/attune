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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// namespaceCacheMiss returns the controller-runtime multi-namespace cache
// error for one namespace. Other lists delegate.
type namespaceCacheMiss struct {
	client.Reader
	namespace string
}

func (r namespaceCacheMiss) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var listOpts client.ListOptions
	listOpts.ApplyOptions(opts)
	if r.namespace != "" && listOpts.Namespace == r.namespace {
		return fmt.Errorf("unable to list: %s because of unknown namespace for the cache", r.namespace)
	}
	return r.Reader.List(ctx, list, opts...)
}

func unwatchedNamespaceValidator(t *testing.T, live ...client.Object) *AttunePolicyValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	cache := fake.NewClientBuilder().WithScheme(scheme).Build()
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live...).Build()
	return &AttunePolicyValidator{
		Client:    namespaceCacheMiss{Reader: cache, namespace: "team-b"},
		APIReader: api,
	}
}

func teamBPolicy(t *testing.T) *attunev1alpha1.AttunePolicy {
	t.Helper()
	policy := validPolicy()
	policy.Namespace = "team-b"
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 168 * time.Hour}
	return policy
}

func TestValidate_UnwatchedNamespaceUsesAPIReader(t *testing.T) {
	ctx := context.Background()

	t.Run("create and update with max and no min", func(t *testing.T) {
		policy := teamBPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "2")
		validator := unwatchedNamespaceValidator(t)
		_, err := validator.ValidateCreate(ctx, policy)
		assert.NoError(t, err)
		_, err = validator.ValidateUpdate(ctx, policy.DeepCopy(), policy)
		assert.NoError(t, err)
	})

	t.Run("namespace min above max still rejects", func(t *testing.T) {
		policy := teamBPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "2")
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "team-b"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "4")},
			},
		}
		_, err := unwatchedNamespaceValidator(t, ns).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "4")
		assert.Contains(t, err.Error(), "2")
		assert.Contains(t, err.Error(), "AttuneNamespaceDefaults team-b/team")
		assert.NotContains(t, err.Error(), "unknown namespace")
	})

	t.Run("surge window reads live history", func(t *testing.T) {
		policy := validPolicy()
		policy.Namespace = "team-b"
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{
			Window: &metav1.Duration{Duration: 48 * time.Hour},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "team-b"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				MetricsSource: &attunev1alpha1.MetricsSource{
					HistoryWindow: &metav1.Duration{Duration: 24 * time.Hour},
				},
			},
		}
		_, err := unwatchedNamespaceValidator(t, ns).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "historyWindow")
		assert.NotContains(t, err.Error(), "unknown namespace")

		policy.Spec.CPU.Surge.Window = &metav1.Duration{Duration: 12 * time.Hour}
		_, err = unwatchedNamespaceValidator(t, ns).ValidateUpdate(ctx, policy.DeepCopy(), policy)
		assert.NoError(t, err)
	})

	t.Run("live list error still rejects", func(t *testing.T) {
		policy := teamBPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "2")
		base := unwatchedNamespaceValidator(t)
		base.APIReader = errListReader{Reader: base.APIReader}
		_, err := base.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "listing Attune")
	})

	t.Run("cache miss without a live reader still rejects", func(t *testing.T) {
		policy := teamBPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "2")
		validator := unwatchedNamespaceValidator(t)
		validator.APIReader = nil
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown namespace")
	})
}
