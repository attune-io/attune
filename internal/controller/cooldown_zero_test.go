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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/resize"
)

func TestParseCooldown_ZeroAndNegativeUseDefault(t *testing.T) {
	r := NewAttunePolicyReconciler()
	tests := []struct {
		name string
		cd   time.Duration
		want time.Duration
	}{
		{name: "zero", cd: 0, want: time.Hour},
		{name: "negative", cd: -5 * time.Minute, want: time.Hour},
		{name: "one hour", cd: time.Hour, want: time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: tc.cd}
			assert.Equal(t, tc.want, r.parseCooldown(policy))
		})
	}
}

func TestParseCooldown_MergedDefaultsZeroStaysOneHour(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = nil
	defs := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 0},
			},
		},
	}
	r := NewAttunePolicyReconciler()
	r.mergeDefaults(policy, defs)
	r.applyBuiltInDefaults(policy)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, time.Duration(0), policy.Spec.UpdateStrategy.Cooldown.Duration,
		"built-in defaults fill a nil cooldown, not a stored 0s")
	assert.Equal(t, time.Hour, r.parseCooldown(policy))
}

func TestReconcile_NoWorkloads_ZeroCooldownRequeuesAtDefault(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Finalizers = []string{finalizerName}
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 0}
	reconciler, _ := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.RequeueJitter = 0

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, result.RequeueAfter)
}

func TestReconcile_Paused_ZeroCooldownDoesNotRequeue(t *testing.T) {
	paused := true
	policy := newTestPolicy("paused-policy", "default")
	policy.Finalizers = []string{finalizerName}
	policy.Spec.Paused = &paused
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 0}
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	reconciler, _ := newReconcilerForReconcile(&mockCollector{}, policy)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paused-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_InsufficientData_ZeroQueryStepRequeuesAtTenSeconds(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 2 * time.Hour}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 0}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(20, 0.1), nil
		},
	}
	reconciler, _ := newReconcilerForReconcile(mc, policy, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
}

func TestReconcile_ZeroCooldown_ObservationAndMonitoring(t *testing.T) {
	successHistory := []attunev1alpha1.ResizeHistoryEntry{{
		Workload:  "api-server",
		Method:    resize.MethodInPlace,
		Result:    attunev1alpha1.ResizeResultSuccess,
		Timestamp: metav1.Now(),
	}}

	t.Run("auto safety zero uses 5m not query step", func(t *testing.T) {
		policy := zeroCooldownAutoPolicy(30*time.Minute, &metav1.Duration{Duration: 0})
		policy.Status.ResizeHistory = successHistory
		result, updated := reconcileZeroCooldown(t, policy, 20, true)
		assert.Equal(t, 5*time.Minute, result.RequeueAfter)
		cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
		require.NotNil(t, cond)
		assert.Equal(t, attunev1alpha1.ReasonInsufficientData, cond.Reason)
		assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
	})

	t.Run("auto safety 30s floors to 1m", func(t *testing.T) {
		policy := zeroCooldownAutoPolicy(30*time.Minute, &metav1.Duration{Duration: 30 * time.Second})
		policy.Status.ResizeHistory = successHistory
		result, updated := reconcileZeroCooldown(t, policy, 20, true)
		assert.Equal(t, time.Minute, result.RequeueAfter)
		assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
	})

	t.Run("recommend with data requeues at 1h", func(t *testing.T) {
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 0}
		policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 30 * time.Minute}
		result, updated := reconcileZeroCooldown(t, policy, 200, false)
		assert.Equal(t, time.Hour, result.RequeueAfter)
		cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
		require.NotNil(t, cond)
		assert.Equal(t, attunev1alpha1.ReasonMonitoring, cond.Reason)
	})

	t.Run("auto revert off with data requeues at 1h", func(t *testing.T) {
		policy := zeroCooldownAutoPolicy(30*time.Minute, nil)
		policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)
		policy.Status.ResizeHistory = successHistory
		result, updated := reconcileZeroCooldown(t, policy, 200, false)
		assert.Equal(t, time.Hour, result.RequeueAfter)
		cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
		require.NotNil(t, cond)
		assert.NotEqual(t, attunev1alpha1.ReasonInsufficientData, cond.Reason)
	})
}

func zeroCooldownAutoPolicy(queryStep time.Duration, safety *metav1.Duration) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 0}
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = safety
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: queryStep}
	return policy
}

func reconcileZeroCooldown(t *testing.T, policy *attunev1alpha1.AttunePolicy, samples int, track bool) (ctrl.Result, attunev1alpha1.AttunePolicy) {
	t.Helper()
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	if track {
		// A retained history row is not this cycle. The short wait comes from
		// a pod still inside its observation window.
		pod.Labels[labelTracked] = "true"
		pod.Annotations = map[string]string{
			annotationResizedAt:                     time.Now().UTC().Format(time.RFC3339),
			annotationResizedWorkload:               "api-server",
			annotationResizedContainers:             "main",
			annotationOriginalCPUPrefix + "main":    "100m",
			annotationOriginalMemoryPrefix + "main": "128Mi",
			annotationPolicy:                        policy.Name,
		}
	}
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(samples, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, deploy, pod)
	if track {
		reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	}
	reconciler.RequeueJitter = 0
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace},
	})
	require.NoError(t, err)
	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: policy.Name, Namespace: policy.Namespace,
	}, &updated))
	return result, updated
}
