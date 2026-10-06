//go:build integration

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

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestReconcile_NoWorkloads_InheritedCooldownEnvtest(t *testing.T) {
	cfg, cl := startIsolatedEnvtest(t)
	reconciler := newFaultReconciler(t, cl, cfg)
	reconciler.RequeueJitter = 0

	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inherited-cooldown"}}
	require.NoError(t, cl.Create(ctx, ns))

	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "inherited-cooldown"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}
	require.NoError(t, cl.Create(ctx, defaults))

	policy := newTestPolicy("inherited-cooldown", ns.Name, "missing-deploy")
	policy.Spec.UpdateStrategy.Cooldown = nil
	require.NoError(t, cl.Create(ctx, policy))

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: policy.Name, Namespace: ns.Name},
	})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, result.RequeueAfter)

	var stored attunev1alpha1.AttunePolicy
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: ns.Name}, &stored))
	require.NotNil(t, stored.Spec.UpdateStrategy)
	assert.Nil(t, stored.Spec.UpdateStrategy.Cooldown)
}
