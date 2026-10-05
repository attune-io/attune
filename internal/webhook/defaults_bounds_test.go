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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func boundsPolicy(t *testing.T) *attunev1alpha1.AttunePolicy {
	t.Helper()
	policy := validPolicy()
	policy.Namespace = "apps"
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 168 * time.Hour}
	return policy
}

func qtyPtr(t *testing.T, raw string) *resource.Quantity {
	t.Helper()
	q := mustQty(t, raw)
	return &q
}

func TestValidate_DefaultsMinAboveMax(t *testing.T) {
	ctx := context.Background()

	t.Run("policy max and defaults min", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		_, err := surgeValidator(t, cluster).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "2Gi")
		assert.Contains(t, err.Error(), "1Gi")
		assert.Contains(t, err.Error(), `AttuneDefaults "global"`)
		assert.Contains(t, err.Error(), "on the policy")
		assert.Nil(t, policy.Spec.Memory.MinAllowed)
	})

	t.Run("policy min wins and does not list", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MinAllowed = qtyPtr(t, "100Mi")
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		base := surgeValidator(t)
		validator := &AttunePolicyValidator{Client: errListReader{Reader: base.Client}}
		_, err := validator.ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("container max under defaults min", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "*",
		}, {
			ContainerName: "app",
			Memory:        &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "1Gi")},
		}}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		_, err := surgeValidator(t, ns).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app")
		assert.Contains(t, err.Error(), "2Gi")
		assert.Contains(t, err.Error(), "1Gi")
		assert.Contains(t, err.Error(), "AttuneNamespaceDefaults apps/team")
	})

	t.Run("container min wins and does not list", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			Memory: &attunev1alpha1.ResourceConfig{
				MinAllowed: qtyPtr(t, "100Mi"),
				MaxAllowed: qtyPtr(t, "1Gi"),
			},
		}}
		base := surgeValidator(t)
		validator := &AttunePolicyValidator{Client: errListReader{Reader: base.Client}}
		_, err := validator.ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("list error when min is omitted", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		base := surgeValidator(t)
		validator := &AttunePolicyValidator{Client: errListReader{Reader: base.Client}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "listing Attune")
	})

	t.Run("list error with neither bound succeeds", func(t *testing.T) {
		policy := boundsPolicy(t)
		base := surgeValidator(t)
		validator := &AttunePolicyValidator{Client: errListReader{Reader: base.Client}}
		_, err := validator.ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("max alone with no defaults min succeeds", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		_, err := surgeValidator(t).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("nil client does not invent a floor", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "200m")
		_, err := (&AttunePolicyValidator{}).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("namespace min below the policy max wins over cluster", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "100Mi")},
			},
		}
		_, err := surgeValidator(t, cluster, ns).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("namespace without a min uses the cluster min", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec:       attunev1alpha1.AttuneDefaultsSpec{},
		}
		_, err := surgeValidator(t, cluster, ns).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `AttuneDefaults "global"`)
		assert.NotContains(t, err.Error(), "AttuneNamespaceDefaults")
	})

	t.Run("equal defaults min and policy max succeeds", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.Memory.MaxAllowed = qtyPtr(t, "1Gi")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "1Gi")},
			},
		}
		_, err := surgeValidator(t, cluster).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("container ceiling above the defaults min ignores the defaults max", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "200m")
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			Memory:        &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "10Gi")},
		}}
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "1Gi")},
			},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		_, err := surgeValidator(t, cluster, ns).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
		assert.Nil(t, policy.Spec.Memory.MinAllowed)
		assert.Nil(t, policy.Spec.Memory.MaxAllowed)
	})

	t.Run("uncapped memory ignores an inverted defaults pair", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "200m")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "1Gi")},
			},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		_, err := surgeValidator(t, cluster, ns).ValidateCreate(ctx, policy)
		assert.NoError(t, err)
	})

	t.Run("container max is named when defaults also set a lower max", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			Memory:        &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "512Mi")},
		}}
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "1Gi")},
			},
		}
		ns := &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "apps"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "2Gi")},
			},
		}
		_, err := surgeValidator(t, cluster, ns).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app")
		assert.Contains(t, err.Error(), "512Mi")
		assert.Contains(t, err.Error(), "2Gi")
		assert.NotContains(t, err.Error(), "on the policy")
	})

	t.Run("cpu defaults min above policy max", func(t *testing.T) {
		policy := boundsPolicy(t)
		policy.Spec.CPU.MaxAllowed = qtyPtr(t, "200m")
		cluster := &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "global"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU: &attunev1alpha1.ResourceConfig{MinAllowed: qtyPtr(t, "500m")},
			},
		}
		_, err := surgeValidator(t, cluster).ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500m")
		assert.Contains(t, err.Error(), "200m")
		assert.Contains(t, err.Error(), "cpu")
	})
}
