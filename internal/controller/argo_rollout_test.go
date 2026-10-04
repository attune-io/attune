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
	"os"
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/argorollout"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/safety"
)

func TestGoModDoesNotVendorArgoRollouts(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	assert.NotContains(t, string(data), "github.com/argoproj/argo-rollouts")
}

func TestRolloutGVKAndAdapter(t *testing.T) {
	t.Parallel()

	ro := &argorollout.Rollout{}
	gvks, _, err := testScheme().ObjectKinds(ro)
	require.NoError(t, err)
	require.NotEmpty(t, gvks)
	assert.Equal(t, schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Rollout",
	}, gvks[0])

	adapter := newWorkloadAdapter(ro)
	require.NotNil(t, adapter)
	assert.False(t, adapter.IsBatch())
	dep := newWorkloadAdapter(&appsv1.Deployment{})
	require.NotNil(t, dep)
	assert.Equal(t, dep.PodNameRegexSuffix(), adapter.PodNameRegexSuffix())
	assert.Nil(t, newWorkloadAdapter(&corev1.ConfigMap{}))
}

func TestRolloutAdapter_IsRollingOut(t *testing.T) {
	t.Parallel()

	one := int32(1)
	two := int32(2)
	tests := []struct {
		name    string
		rollout argorollout.Rollout
		want    bool
	}{
		{
			name: "healthy and updated matches spec",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &one},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 1, Phase: "Healthy"},
			},
		},
		{
			name: "degraded without abort and updated matches spec",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &one},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 1, Phase: "Degraded"},
			},
		},
		{
			name: "abort with degraded phase",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &one},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 1, Phase: "Degraded", Abort: true},
			},
			want: true,
		},
		{
			name: "paused blue-green preview already at full size",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &two},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 2, Phase: "Paused"},
			},
			want: true,
		},
		{
			name: "progressing analysis at full size",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &two},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 2, Phase: "Progressing"},
			},
			want: true,
		},
		{
			name: "canary holding below spec",
			rollout: argorollout.Rollout{
				Spec:   argorollout.RolloutSpec{Replicas: &two},
				Status: argorollout.RolloutStatus{UpdatedReplicas: 1, Phase: "Healthy"},
			},
			want: true,
		},
		{
			name: "nil spec replicas counts as 1 and updated 0 is rolling",
			rollout: argorollout.Rollout{
				Status: argorollout.RolloutStatus{UpdatedReplicas: 0, Phase: "Healthy"},
			},
			want: true,
		},
		{
			name: "nil spec replicas counts as 1 and updated 1 is promoted",
			rollout: argorollout.Rollout{
				Status: argorollout.RolloutStatus{UpdatedReplicas: 1, Phase: "Healthy"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter := &rolloutAdapter{Rollout: &tt.rollout}
			assert.Equal(t, tt.want, adapter.IsRollingOut())
		})
	}
}

func TestGetPodsForWorkload_RolloutSelector(t *testing.T) {
	t.Parallel()

	ro := &argorollout.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec: argorollout.RolloutSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "checkout"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "tier",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{"paid"},
				}},
			},
		},
	}
	stable := rolloutOwnedPod("checkout-stable-abcde", "checkout-stable", "checkout", "paid")
	canary := rolloutOwnedPod("checkout-canary-fghij", "checkout-canary", "checkout", "paid")
	other := rolloutOwnedPod("api-abc-1", "api-rs", "api", "paid")
	missingTier := rolloutOwnedPod("checkout-notier", "checkout-stable", "checkout", "")

	r := newReconcilerWithClient(ro, stable, canary, other, missingTier)
	pods, err := r.getPodsForWorkload(context.Background(), ro)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, pod := range pods {
		names[pod.Name] = true
	}
	assert.Equal(t, map[string]bool{
		"checkout-stable-abcde": true,
		"checkout-canary-fghij": true,
	}, names)
}

func TestFilterStandaloneReplicaSets_RolloutOwner(t *testing.T) {
	t.Parallel()

	yes := true
	no := false
	deployOwned := replicaSetWithOwner("dep-rs", metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api", Controller: &yes,
	})
	deployOwnedNoController := replicaSetWithOwner("dep-ref", metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	rolloutOwned := replicaSetWithOwner("ro-rs", metav1.OwnerReference{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "checkout", Controller: &yes,
	})
	rolloutNotController := replicaSetWithOwner("ro-ref", metav1.OwnerReference{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "checkout", Controller: &no,
	})
	wrongVersion := replicaSetWithOwner("old-ro", metav1.OwnerReference{
		APIVersion: "argoproj.io/v1beta1", Kind: "Rollout", Name: "checkout", Controller: &yes,
	})
	standalone := replicaSetWithOwner("alone", metav1.OwnerReference{})
	standalone.OwnerReferences = nil

	got := filterStandaloneReplicaSets([]client.Object{
		deployOwned, deployOwnedNoController, rolloutOwned, rolloutNotController, wrongVersion, standalone,
	})
	names := make([]string, 0, len(got))
	for _, obj := range got {
		names = append(names, obj.GetName())
	}
	assert.Equal(t, []string{"ro-ref", "old-ro", "alone"}, names)
}

func TestGetWorkloadByName_RolloutOwnedReplicaSet(t *testing.T) {
	t.Parallel()

	yes := true
	roRS := replicaSetWithOwner("checkout-abc", metav1.OwnerReference{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "checkout", Controller: &yes,
	})
	depRS := replicaSetWithOwner("api-abc", metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api", Controller: &yes,
	})
	r := newReconcilerWithClient(roRS, depRS)

	_, err := r.getWorkloadByName(context.Background(), "default", "ReplicaSet", "checkout-abc")
	require.EqualError(t, err, "ReplicaSet default/checkout-abc is owned by a Rollout; target the Rollout instead")

	_, err = r.getWorkloadByName(context.Background(), "default", "ReplicaSet", "api-abc")
	require.EqualError(t, err, "ReplicaSet default/api-abc is owned by a Deployment; target the Deployment instead")
}

func TestDiscoverWorkloads_RolloutCRDMissingDoesNotGet(t *testing.T) {
	t.Parallel()

	var gets int
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets++
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	r.RESTMapper = &mapErrMapper{err: &meta.NoKindMatchError{}}

	policy := newTestPolicy("p", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	workloads, err := r.discoverWorkloads(context.Background(), policy)
	require.ErrorIs(t, err, errRolloutCRDMissing)
	assert.Nil(t, workloads)
	assert.Equal(t, 0, gets)
	assert.Equal(t, "argoproj.io/v1alpha1 Rollout CRD is not installed", err.Error())
}

func TestDiscoverWorkloads_NilMapperSkipsMissingCheck(t *testing.T) {
	t.Parallel()

	r := newReconcilerWithClient()
	policy := newTestPolicy("p", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	workloads, err := r.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Empty(t, workloads)
}

func TestDiscoverWorkloads_DeploymentDoesNotConsultRolloutMapper(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"}}
	r := newReconcilerWithClient(dep)
	mapper := &mapErrMapper{err: fmt.Errorf("mapper down")}
	r.RESTMapper = mapper
	policy := newTestPolicy("p", "default")
	workloads, err := r.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, "api-server", workloads[0].GetName())
	assert.Equal(t, 0, mapper.calls)
}

func TestReconcile_RolloutCRDMissingBeforeMetrics(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	factoryCalls := 0
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		factoryCalls++
		return nil, fmt.Errorf("prometheus down")
	}
	reconciler.RESTMapper = &mapErrMapper{err: &meta.NoKindMatchError{}}

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads"))
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads"))

	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, result.RequeueAfter)
	assert.Equal(t, 0, factoryCalls)
	assert.Equal(t, before, after)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonWorkloadCRDMissing, cond.Reason)
	assert.Equal(t, "argoproj.io/v1alpha1 Rollout CRD is not installed", cond.Message)
}

func TestReconcile_RolloutMapperErrorUsesDiscoveryFailed(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy)
	reconciler.RESTMapper = &mapErrMapper{err: fmt.Errorf("mapper down")}

	before := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads"))
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	after := promtestutil.ToFloat64(operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads"))

	require.NoError(t, err)
	assert.Equal(t, time.Minute, result.RequeueAfter)
	assert.Equal(t, before+1, after)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonWorkloadDiscoveryFailed, cond.Reason)
	assert.Contains(t, cond.Message, "mapper down")
}

func TestReconcile_DeploymentIgnoresRolloutMapper(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(&mockCollector{}, policy, dep)
	mapper := &mapErrMapper{err: &meta.NoKindMatchError{}}
	reconciler.RESTMapper = mapper

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.NotEqual(t, 2*time.Minute, result.RequeueAfter)
	assert.Equal(t, 0, mapper.calls)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.NotEqual(t, attunev1alpha1.ReasonWorkloadCRDMissing, cond.Reason)
}

func TestRolloutResizeGate(t *testing.T) {
	tests := []struct {
		name       string
		replicas   *int32
		updated    int32
		phase      string
		abort      bool
		wantResize bool
		event      string
	}{
		{name: "abort", replicas: int32Ptr(1), updated: 1, phase: "Degraded", abort: true, event: "abort true"},
		{name: "paused at full size", replicas: int32Ptr(2), updated: 2, phase: "Paused", event: "phase Paused"},
		{name: "progressing at full size", replicas: int32Ptr(2), updated: 2, phase: "Progressing", event: "phase Progressing"},
		{name: "below desired", replicas: int32Ptr(2), updated: 1, phase: "Healthy", event: "phase Healthy"},
		{name: "nil replicas updated 0", updated: 0, phase: "Healthy", event: "phase Healthy"},
		{name: "healthy", replicas: int32Ptr(1), updated: 1, phase: "Healthy", wantResize: true},
		{name: "nil replicas updated 1", updated: 1, phase: "Healthy", wantResize: true},
	}
	cpuReq, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memReq, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			cpu := cpuReq.DeepCopy()
			mem := memReq.DeepCopy()
			ro := &argorollout.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
				Spec: argorollout.RolloutSpec{
					Replicas: tt.replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "main",
								Image: "nginx:latest",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    cpu,
										corev1.ResourceMemory: mem,
									},
								},
							}},
						},
					},
				},
				Status: argorollout.RolloutStatus{
					UpdatedReplicas: tt.updated,
					Phase:           tt.phase,
					Abort:           tt.abort,
				},
			}
			pod := burstableResizePod("api-abc-1", "api")
			policy := newTestPolicy("test-policy", "default")
			policy.Spec.TargetRef.Kind = argorollout.Kind
			policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
			r, rec := newRolloutReconciler([]client.Object{ro}, []*corev1.Pod{pod})
			result := runRolloutProcess(r, policy, ro, now)
			requireNonStaleRec(t, result)

			count, _ := r.executeResizes(context.Background(), policy, []client.Object{ro},
				[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("api")},
				podMap("api", pod), nil, nil)
			evs := recordedEvents(rec)
			if tt.wantResize {
				require.Greater(t, count, 0)
				assert.NotEmpty(t, resizedPodNames(r.Clientset.(*kubefake.Clientset)))
				assert.Equal(t, 0, countSubstr(evs, "RolloutInProgress"))
				return
			}
			assert.Equal(t, 0, count)
			assert.Empty(t, resizedPodNames(r.Clientset.(*kubefake.Clientset)))
			assert.Equal(t, 1, countSubstr(evs, "RolloutInProgress"))
			assert.Equal(t, 1, countSubstr(evs, tt.event))
		})
	}
}

func TestApplyTemplatePersistence_Rollout(t *testing.T) {
	cpuCur, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memCur, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	cpuRec, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)

	ro := rolloutWithResources("checkout", cpuCur, memCur)
	child := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-abc", Namespace: "default"},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout"}},
			Template: ro.Spec.Template,
		},
	}
	r := newReconcilerWithClient(ro, child)
	policy := rolloutPersistPolicy()
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    attunev1alpha1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  attunev1alpha1.ReasonMonitoring,
		Message: "watching",
	})

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{ro},
		[]attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	require.Len(t, history, 1)

	var updated argorollout.Rollout
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())

	var rs appsv1.ReplicaSet
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(child), &rs))
	assert.Equal(t, int64(500), rs.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.Nil(t, meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionTemplatePersistence))
	ready := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, attunev1alpha1.ReasonMonitoring, ready.Reason)
}

func TestApplyTemplatePersistence_RolloutMidStepDoesNotPatch(t *testing.T) {
	cpuCur, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memCur, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	cpuRec, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)

	cases := []struct {
		name    string
		phase   string
		abort   bool
		updated int32
		patch   bool
	}{
		{name: "paused", phase: "Paused", updated: 1, patch: false},
		{name: "progressing", phase: "Progressing", updated: 1, patch: false},
		{name: "abort", phase: "Healthy", abort: true, updated: 1, patch: false},
		{name: "healthy", phase: "Healthy", updated: 1, patch: true},
		{name: "healthy behind", phase: "Healthy", updated: 0, patch: false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ro := rolloutWithResources("checkout", cpuCur, memCur)
			ro.Status.Phase = tt.phase
			ro.Status.Abort = tt.abort
			ro.Status.UpdatedReplicas = tt.updated
			r := newReconcilerWithClient(ro)
			policy := rolloutPersistPolicy()
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    attunev1alpha1.ConditionReady,
				Status:  metav1.ConditionTrue,
				Reason:  attunev1alpha1.ReasonMonitoring,
				Message: "watching",
			})
			_ = r.applyTemplatePersistence(context.Background(), policy, []client.Object{ro},
				[]attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
				attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
			var updated argorollout.Rollout
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &updated))
			got := updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
			if tt.patch {
				assert.Equal(t, int64(200), got)
				return
			}
			assert.Equal(t, int64(500), got)
		})
	}
}

func TestComputeRecommendations_RolloutWorkloadRef(t *testing.T) {
	cpu, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	mem, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	policy := newTestPolicy("p", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu_usage_seconds_total") {
				return generateSamples(200, 0.1), nil
			}
			return generateSamples(200, 128*1024*1024), nil
		},
	}

	t.Run("uses referenced deployment containers", func(t *testing.T) {
		dep := newTestDeployment("checkout", "default", nil)
		ro := rolloutWithResources("checkout-rollout", cpu, mem)
		ro.Spec.Template.Spec.Containers = nil
		ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "checkout", Kind: "Deployment", APIVersion: "apps/v1"}
		r := newReconcilerWithClient(ro, dep)
		rec, _, _, _, _, recErr := r.computeRecommendations(context.Background(), policy, ro, mc, nil, nil, nil, nil, nil)
		require.NoError(t, recErr)
		require.NotNil(t, rec)
		require.NotEmpty(t, rec.Containers)
		assert.Equal(t, "main", rec.Containers[0].Name)
		var stored argorollout.Rollout
		require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &stored))
		assert.Empty(t, stored.Spec.Template.Spec.Containers)
	})

	t.Run("missing deployment sets an error", func(t *testing.T) {
		ro := rolloutWithResources("checkout-rollout", cpu, mem)
		ro.Spec.Template.Spec.Containers = nil
		ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "missing", Kind: "Deployment", APIVersion: "apps/v1"}
		r := newReconcilerWithClient(ro)
		rec, _, _, _, _, recErr := r.computeRecommendations(context.Background(), policy, ro, mc, nil, nil, nil, nil, nil)
		require.Error(t, recErr)
		assert.Nil(t, rec)
		assert.Contains(t, recErr.Error(), "workloadRef ")
		policy.Status.WorkloadErrors = []attunev1alpha1.WorkloadError{{Workload: ro.Name, Error: recErr.Error()}}
		noteWorkloadRefReadErrors(policy, policy.Status.WorkloadErrors)
		cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionTemplatePersistence)
		require.NotNil(t, cond)
		assert.Equal(t, attunev1alpha1.ReasonWorkloadRefUnread, cond.Reason)
	})

	t.Run("own template is unchanged", func(t *testing.T) {
		ro := rolloutWithResources("checkout", cpu, mem)
		r := newReconcilerWithClient(ro)
		rec, _, _, _, _, recErr := r.computeRecommendations(context.Background(), policy, ro, mc, nil, nil, nil, nil, nil)
		require.NoError(t, recErr)
		require.NotNil(t, rec)
		assert.Equal(t, "app", rec.Containers[0].Name)
	})
}

func TestApplyTemplatePersistence_RolloutWorkloadRef(t *testing.T) {
	cpuCur, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memCur, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	cpuRec, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	memRec, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)

	ro := rolloutWithResources("checkout", cpuCur, memCur)
	ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "checkout", Kind: "Deployment", APIVersion: "apps/v1"}
	child := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-abc", Namespace: "default"},
		Spec:       appsv1.ReplicaSetSpec{Template: ro.Spec.Template},
	}
	r := newReconcilerWithClient(ro, child)
	policy := rolloutPersistPolicy()
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    attunev1alpha1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  attunev1alpha1.ReasonMonitoring,
		Message: "watching",
	})

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{ro},
		[]attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	assert.Empty(t, history)

	var updated argorollout.Rollout
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionTemplatePersistence)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonTemplateWorkloadRef, cond.Reason)
	assert.Equal(t, templateWorkloadRefMessage, cond.Message)
	ready := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, attunev1alpha1.ReasonMonitoring, ready.Reason)
}

func TestApplyTemplatePersistence_WorkloadRefUnreadSurvives(t *testing.T) {
	cpuCur := resource.MustParse("500m")
	memCur := resource.MustParse("512Mi")
	cpuRec := resource.MustParse("200m")
	memRec := resource.MustParse("256Mi")
	readErr := `workloadRef Deployment default/missing: deployments.apps "missing" not found`

	unreadRollout := func() *argorollout.Rollout {
		ro := rolloutWithResources("orders", cpuCur, memCur)
		ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "missing", Kind: "Deployment", APIVersion: "apps/v1"}
		return ro
	}
	refRollout := func() *argorollout.Rollout {
		ro := rolloutWithResources("orders", cpuCur, memCur)
		ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "orders", Kind: "Deployment", APIVersion: "apps/v1"}
		return ro
	}
	plainRollout := func() *argorollout.Rollout { return rolloutWithResources("checkout", cpuCur, memCur) }
	deploymentRec := func(name string) attunev1alpha1.WorkloadRecommendation {
		rec := rolloutRecommendation(name, cpuCur, memCur, cpuRec, memRec)
		rec.Kind = "Deployment"
		rec.Containers[0].Name = "main"
		return rec
	}

	cases := []struct {
		name      string
		errs      []attunev1alpha1.WorkloadError
		stale     string // reason seeded directly when errs is empty
		mode      attunev1alpha1.UpdateType
		when      attunev1alpha1.TemplatePersistenceWhen
		only      map[string]bool
		workloads func() []client.Object
		recs      []attunev1alpha1.WorkloadRecommendation
		want      string // "" means the condition is absent
		wantMsg   string
		patched   string // healthy Rollout whose template must be patched
	}{
		{
			name: "unread with workloadRef rollout, Recommend OnRecommendation",
			errs: []attunev1alpha1.WorkloadError{{Workload: "orders", Error: readErr}},
			mode: attunev1alpha1.UpdateTypeRecommend, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{unreadRollout(), plainRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
			want:      attunev1alpha1.ReasonWorkloadRefUnread, wantMsg: readErr,
			patched: "checkout",
		},
		{
			name: "unread with workloadRef rollout, Auto OnRecommendation",
			errs: []attunev1alpha1.WorkloadError{{Workload: "orders", Error: readErr}},
			mode: attunev1alpha1.UpdateTypeAuto, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{unreadRollout(), plainRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
			want:      attunev1alpha1.ReasonWorkloadRefUnread, wantMsg: readErr,
			patched: "checkout",
		},
		{
			name: "unread with workloadRef rollout, Auto AfterSuccessfulResize",
			errs: []attunev1alpha1.WorkloadError{{Workload: "orders", Error: readErr}},
			mode: attunev1alpha1.UpdateTypeAuto, when: attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
			only:      map[string]bool{"checkout": true},
			workloads: func() []client.Object { return []client.Object{unreadRollout(), plainRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
			want:      attunev1alpha1.ReasonWorkloadRefUnread, wantMsg: readErr,
			patched: "checkout",
		},
		{
			// Unreachable in a real reconcile (the unread Rollout is in the
			// workload list); kept as a unit guard on the errors-keyed rule.
			name: "unread without workloadRef rollout in the list",
			errs: []attunev1alpha1.WorkloadError{{Workload: "orders", Error: readErr}},
			mode: attunev1alpha1.UpdateTypeRecommend, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{newTestDeployment("api", "default", nil)} },
			recs:      []attunev1alpha1.WorkloadRecommendation{deploymentRec("api")},
			want:      attunev1alpha1.ReasonWorkloadRefUnread, wantMsg: readErr,
		},
		{
			name:  "stale unread, reference readable now",
			stale: attunev1alpha1.ReasonWorkloadRefUnread,
			mode:  attunev1alpha1.UpdateTypeRecommend, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{refRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("orders", cpuCur, memCur, cpuRec, memRec)},
			want:      attunev1alpha1.ReasonTemplateWorkloadRef, wantMsg: templateWorkloadRefMessage,
		},
		{
			name:  "stale unread, no workloadRef rollout",
			stale: attunev1alpha1.ReasonWorkloadRefUnread,
			mode:  attunev1alpha1.UpdateTypeRecommend, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{plainRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
		},
		{
			name:  "stale TemplateWorkloadRef, no workloadRef rollout",
			stale: attunev1alpha1.ReasonTemplateWorkloadRef,
			mode:  attunev1alpha1.UpdateTypeRecommend, when: attunev1alpha1.TemplatePersistenceOnRecommendation,
			workloads: func() []client.Object { return []client.Object{plainRollout()} },
			recs:      []attunev1alpha1.WorkloadRecommendation{rolloutRecommendation("checkout", cpuCur, memCur, cpuRec, memRec)},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			workloads := tt.workloads()
			r := newReconcilerWithClient(workloads...)
			policy := rolloutPersistPolicy()
			policy.Spec.UpdateStrategy.Type = tt.mode
			policy.Spec.UpdateStrategy.TemplatePersistence.When = tt.when
			if len(tt.errs) > 0 {
				// Same order as the reconcile: workloadErrors, then the note.
				policy.Status.WorkloadErrors = tt.errs
				noteWorkloadRefReadErrors(policy, policy.Status.WorkloadErrors)
			} else {
				meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
					Type:    attunev1alpha1.ConditionTemplatePersistence,
					Status:  metav1.ConditionFalse,
					Reason:  tt.stale,
					Message: "left over from an earlier reconcile",
				})
			}

			_ = r.applyTemplatePersistence(context.Background(), policy, workloads, tt.recs, tt.when, tt.only)

			cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionTemplatePersistence)
			if tt.want == "" {
				assert.Nil(t, cond)
			} else {
				require.NotNil(t, cond)
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, tt.want, cond.Reason)
				assert.Equal(t, tt.wantMsg, cond.Message)
			}
			for _, w := range workloads {
				ro, ok := w.(*argorollout.Rollout)
				if !ok || ro.Spec.WorkloadRef == nil {
					continue
				}
				var stored argorollout.Rollout
				require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &stored))
				assert.Equal(t, int64(500), stored.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
			}
			if tt.patched != "" {
				var healthy argorollout.Rollout
				require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: policy.Namespace, Name: tt.patched}, &healthy))
				assert.Equal(t, int64(200), healthy.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
			}
		})
	}
}

func TestRestoreTemplate_RolloutWorkloadRefSkips(t *testing.T) {
	cpuCur, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	memCur, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	originalCPU, err := resource.ParseQuantity("500m")
	require.NoError(t, err)

	ro := rolloutWithResources("checkout", cpuCur, memCur)
	ro.Spec.WorkloadRef = &argorollout.WorkloadRef{Name: "checkout", Kind: "Deployment", APIVersion: "apps/v1"}
	r := newReconcilerWithClient(ro)
	policy := rolloutPersistPolicy()
	policy.Spec.UpdateStrategy.TemplatePersistence.When = attunev1alpha1.TemplatePersistenceAfterSuccessfulResize

	err = r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{ro}, safety.ResizeRecord{
		WorkloadName: "checkout",
		Container:    "app",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: originalCPU, corev1.ResourceMemory: memCur},
		},
	})
	require.NoError(t, err)

	var updated argorollout.Rollout
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &updated))
	assert.Equal(t, int64(200), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionTemplatePersistence)
	require.NotNil(t, cond)
	assert.Equal(t, templateWorkloadRefMessage, cond.Message)
}

func TestRestoreTemplate_RolloutPatchesRollout(t *testing.T) {
	cpuCur, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	memCur, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	originalCPU, err := resource.ParseQuantity("500m")
	require.NoError(t, err)

	ro := rolloutWithResources("checkout", cpuCur, memCur)
	r := newReconcilerWithClient(ro)
	policy := rolloutPersistPolicy()
	policy.Spec.UpdateStrategy.TemplatePersistence.When = attunev1alpha1.TemplatePersistenceAfterSuccessfulResize

	err = r.restoreTemplateAfterSafetyRevert(context.Background(), policy, []client.Object{ro}, safety.ResizeRecord{
		WorkloadName: "checkout",
		Container:    "app",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: originalCPU, corev1.ResourceMemory: memCur},
		},
	})
	require.NoError(t, err)

	var updated argorollout.Rollout
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(ro), &updated))
	assert.Equal(t, int64(500), updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
}

type mapErrMapper struct {
	err   error
	calls int
}

func (m *mapErrMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return &meta.RESTMapping{}, nil
}

func rolloutOwnedPod(name, rsName, app, tier string) *corev1.Pod {
	labels := map[string]string{"app": app}
	if tier != "" {
		labels["tier"] = tier
	}
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rsName,
				UID:        types.UID(rsName),
				Controller: &yes,
			}},
		},
	}
}

func replicaSetWithOwner(name string, owner metav1.OwnerReference) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	if owner.Kind != "" {
		if owner.UID == "" {
			owner.UID = types.UID(name)
		}
		rs.OwnerReferences = []metav1.OwnerReference{owner}
	}
	return rs
}

func rolloutWithResources(name string, cpu, mem resource.Quantity) *argorollout.Rollout {
	one := int32(1)
	return &argorollout.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: argorollout.RolloutSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "nginx",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    cpu,
								corev1.ResourceMemory: mem,
							},
						},
					}},
				},
			},
		},
		Status: argorollout.RolloutStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, Phase: "Healthy"},
	}
}

func rolloutPersistPolicy() *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("p", "default")
	policy.Spec.TargetRef.Kind = argorollout.Kind
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	return policy
}

func rolloutRecommendation(name string, cpuCur, memCur, cpuRec, memRec resource.Quantity) attunev1alpha1.WorkloadRecommendation {
	return attunev1alpha1.WorkloadRecommendation{
		Workload: name,
		Kind:     argorollout.Kind,
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    cpuCur,
				MemoryRequest: memCur,
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    cpuRec,
				MemoryRequest: memRec,
			},
		}},
	}
}
