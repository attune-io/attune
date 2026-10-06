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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func clusterDefaults(spec attunev1alpha1.AttuneDefaultsSpec) *attunev1alpha1.AttuneDefaults {
	return &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       spec,
	}
}

func policyOmittingCooldown() *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = nil
	return policy
}

func steadySamples() *mockCollector {
	return &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
}

func fewSamples() *mockCollector {
	return &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil
		},
	}
}

func reconcilePolicy(t *testing.T, mc rsmetrics.MetricsCollector, objects ...client.Object) (ctrl.Result, client.Client) {
	t.Helper()
	reconciler, c := newReconcilerForReconcile(mc, objects...)
	reconciler.RequeueJitter = 0
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	return result, c
}

func storedPolicy(t *testing.T, c client.Client) attunev1alpha1.AttunePolicy {
	t.Helper()
	var stored attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &stored))
	return stored
}

func TestReconcile_NoWorkloads_InheritedCooldown(t *testing.T) {
	policy := policyOmittingCooldown()
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
		},
	})

	result, c := reconcilePolicy(t, &mockCollector{}, policy, defs)
	assert.Equal(t, 10*time.Minute, result.RequeueAfter)

	stored := storedPolicy(t, c)
	require.NotNil(t, stored.Spec.UpdateStrategy)
	assert.Nil(t, stored.Spec.UpdateStrategy.Cooldown, "status write must not store inherited cooldown")
}

func TestReconcile_Recommendations_InheritedCooldownBothPasses(t *testing.T) {
	policy := policyOmittingCooldown()
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
		},
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	mc := steadySamples()
	reconciler, c := newReconcilerForReconcile(mc, policy, defs, deploy, pod)
	reconciler.RequeueJitter = 0
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}

	first, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, first.RequeueAfter)

	second, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, second.RequeueAfter)

	stored := storedPolicy(t, c)
	assert.Nil(t, stored.Spec.UpdateStrategy.Cooldown)
	assert.NotEmpty(t, stored.Status.Recommendations)
}

func TestReconcile_InsufficientData_InheritedQueryStep(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		MetricsSource: &attunev1alpha1.MetricsSource{
			QueryStep: &metav1.Duration{Duration: 2 * time.Minute},
		},
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})

	result, _ := reconcilePolicy(t, fewSamples(), policy, defs, deploy, pod)
	assert.Equal(t, 2*time.Minute, result.RequeueAfter)
}

func TestReconcile_AutoResize_InheritedObservationPeriod(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			SafetyObservationPeriod: &metav1.Duration{Duration: 2 * time.Minute},
		},
	})
	result, c := reconcileAutoResize(t, policy, defs)
	assert.Equal(t, 2*time.Minute, result.RequeueAfter)
	assert.Equal(t, int32(1), storedPolicy(t, c).Status.Workloads.Resized)
}

func TestReconcile_AutoResize_InheritedAutoRevertOffUsesCooldown(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			AutoRevert: boolPtr(false),
		},
	})
	result, c := reconcileAutoResize(t, policy, defs)
	assert.Equal(t, time.Hour, result.RequeueAfter)
	assert.Equal(t, int32(1), storedPolicy(t, c).Status.Workloads.Resized)
}

func TestReconcile_AutoResize_NilStrategyDoesNotPanic(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy = nil
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			Type: attunev1alpha1.UpdateTypeAuto,
		},
	})
	result, c := reconcileAutoResize(t, policy, defs)
	assert.Equal(t, defaultObservationPeriod, result.RequeueAfter)
	stored := storedPolicy(t, c)
	assert.Nil(t, stored.Spec.UpdateStrategy)
	assert.Equal(t, int32(1), stored.Status.Workloads.Resized)
}

func TestReconcile_AllCooling_InheritedCooldownRemaining(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := policyOmittingCooldown()
	policy.Annotations = map[string]string{
		lastResizeAnnotationPrefix + "api-server": now.Add(-4 * time.Minute).UTC().Format(time.RFC3339),
	}
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
		},
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newReconcilerForReconcile(steadySamples(), policy, defs, deploy, pod)
	reconciler.RequeueJitter = 0
	reconciler.SetNowFunc(func() time.Time { return now })

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, 6*time.Minute, result.RequeueAfter)
}

func TestReconcile_StatusConflictKeepsInheritedCooldown(t *testing.T) {
	policy := policyOmittingCooldown()
	defs := clusterDefaults(attunev1alpha1.AttuneDefaultsSpec{
		UpdateStrategy: &attunev1alpha1.UpdateStrategy{
			Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
		},
	})
	scheme := testScheme()
	conflicts := 0
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResource == "status" {
					if _, ok := obj.(*attunev1alpha1.AttunePolicy); ok && conflicts == 0 {
						conflicts++
						return apierrors.NewConflict(
							schema.GroupResource{Group: attunev1alpha1.GroupVersion.Group, Resource: "attunepolicies"},
							obj.GetName(),
							fmt.Errorf("resourceVersion changed"),
						)
					}
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		WithObjects(ensureTestNamespaces([]client.Object{policy, defs})...).
		Build()
	reconciler := newReconcilerForReconcileWithClient(&mockCollector{}, base, scheme)
	reconciler.RequeueJitter = 0

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, 10*time.Minute, result.RequeueAfter)
}

func TestGetObservationPeriod_NilStrategyUsesDefault(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy = nil
	assert.Equal(t, defaultObservationPeriod, getObservationPeriod(policy))
	assert.Equal(t, defaultObservationPeriod, getObservationPeriod(nil))
}

func reconcileAutoResize(t *testing.T, policy *attunev1alpha1.AttunePolicy, defs *attunev1alpha1.AttuneDefaults) (ctrl.Result, client.Client) {
	t.Helper()
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	reconciler, c := newReconcilerForReconcile(steadySamples(), policy, defs, deploy, pod)
	reconciler.RequeueJitter = 0
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	return result, c
}
