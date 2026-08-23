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
	"fmt"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/controller"
	"github.com/attune-io/attune/internal/metrics"
)

// API-fault envtest tests (#536).
//
// Each case uses a dedicated envtest control plane so interceptors and
// manager start/stop are not raced by TestMain's shared manager.
//
// Envtest cannot cover:
//   - kubelet /resize (no kubelet; the pods/resize subresource is not
//     exercised the way a live node does)
//   - cAdvisor / Prometheus scrape
// Those stay in test/e2e and test/e2e-go (MemoryPressure, OOMKill, scrape).
// These tests intercept AttunePolicy status Patch/Update and check
// persist/status idempotency only.

// faultMgrSeq keeps controller names unique across -count=N reruns in
// the same process (controller-runtime names are process-global).
var faultMgrSeq atomic.Int32

func startIsolatedEnvtest(t *testing.T) (*rest.Config, client.WithWatch) {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing:    true,
		ControlPlaneStartTimeout: 60 * time.Second,
		ControlPlaneStopTimeout:  20 * time.Second,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "start isolated envtest")
	t.Cleanup(func() { _ = env.Stop() })

	cl, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err, "create isolated envtest client")
	return cfg, cl
}

func newFaultReconciler(t *testing.T, cl client.Client, cfg *rest.Config) *controller.AttunePolicyReconciler {
	t.Helper()
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "create clientset")

	r := controller.NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme.Scheme
	r.Clientset = cs
	r.MinCooldown = time.Second
	r.PrometheusTimeout = 30 * time.Second
	r.MetricsFactory = func(address string, opts *metrics.CollectorOptions) (metrics.MetricsCollector, error) {
		return defaultMetricsFactory(address, opts)
	}
	return r
}

func createFaultFixture(t *testing.T, cl client.Client, nsName, deployName, policyName string) types.NamespacedName {
	t.Helper()
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	require.NoError(t, cl.Create(ctx, ns), "create namespace")

	deploy := newTestDeployment(deployName, nsName)
	require.NoError(t, cl.Create(ctx, deploy), "create deployment")
	// envtest has no ReplicaSet controller. Zero status replicas makes
	// IsRollingOut true and skips recommendations. Mark the Deployment ready
	// so the interceptor tests can assert recommendation idempotency.
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.AvailableReplicas = 1
	require.NoError(t, cl.Status().Update(ctx, deploy), "mark deployment ready")

	policy := newTestPolicy(policyName, nsName, deployName)
	require.NoError(t, cl.Create(ctx, policy), "create policy")

	return types.NamespacedName{Name: policyName, Namespace: nsName}
}

func isPolicyStatusUpdate(subResource string, obj client.Object) bool {
	if subResource != "status" {
		return false
	}
	_, ok := obj.(*attunev1alpha1.AttunePolicy)
	return ok
}

func readyReason(policy *attunev1alpha1.AttunePolicy) string {
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

func assertValidResizeHistory(t *testing.T, history []attunev1alpha1.ResizeHistoryEntry) {
	t.Helper()
	validResource := map[string]bool{
		"cpu": true, "memory": true, "cpu+memory": true, "template": true,
	}
	validMethod := map[string]bool{
		"InPlace": true, "Eviction": true, "TemplatePersistence": true,
	}
	validResult := map[string]bool{
		"Success": true, "Failed": true, "Reverted": true,
		"Evicted": true, "TemplatePatched": true,
	}
	for i, entry := range history {
		assert.Truef(t, validResource[entry.Resource],
			"history[%d].resource %q is not a CRD enum value", i, entry.Resource)
		assert.Truef(t, validMethod[entry.Method],
			"history[%d].method %q is not a CRD enum value", i, entry.Method)
		assert.Truef(t, validResult[string(entry.Result)],
			"history[%d].result %q is not a CRD enum value", i, entry.Result)
	}
}

func recWorkloadKeys(recs []attunev1alpha1.WorkloadRecommendation) []string {
	keys := make([]string, 0, len(recs))
	for _, rec := range recs {
		keys = append(keys, rec.Kind+"/"+rec.Workload)
	}
	sort.Strings(keys)
	return keys
}

func seedResizeHistory(t *testing.T, cl client.Client, key types.NamespacedName, workload string) attunev1alpha1.ResizeHistoryEntry {
	t.Helper()
	ctx := context.Background()
	var policy attunev1alpha1.AttunePolicy
	require.NoError(t, cl.Get(ctx, key, &policy))
	entry := attunev1alpha1.ResizeHistoryEntry{
		Timestamp: metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)),
		Workload:  workload,
		Container: "app",
		Resource:  "cpu",
		From:      "100m",
		To:        "80m",
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{entry}
	require.NoError(t, cl.Status().Update(ctx, &policy), "seed resize history")
	return entry
}

func assertHistoryUnchanged(t *testing.T, want attunev1alpha1.ResizeHistoryEntry, got []attunev1alpha1.ResizeHistoryEntry) {
	t.Helper()
	require.Len(t, got, 1, "history must keep the seeded row and not grow")
	assert.Equal(t, want.Workload, got[0].Workload)
	assert.Equal(t, want.Container, got[0].Container)
	assert.Equal(t, want.Resource, got[0].Resource)
	assert.Equal(t, want.From, got[0].From)
	assert.Equal(t, want.To, got[0].To)
	assert.Equal(t, want.Method, got[0].Method)
	assert.Equal(t, want.Result, got[0].Result)
	assert.True(t, want.Timestamp.Equal(&got[0].Timestamp),
		"seeded history timestamp must be unchanged")
	assertValidResizeHistory(t, got)
}

func assertReadyConverged(t *testing.T, policy *attunev1alpha1.AttunePolicy) {
	t.Helper()
	reason := readyReason(policy)
	assert.Contains(t, []string{
		attunev1alpha1.ReasonInsufficientData,
		attunev1alpha1.ReasonMonitoring,
	}, reason, "Ready should converge to InsufficientData or Monitoring")

	resizing := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionResizing)
	if resizing != nil {
		assert.NotEqual(t, metav1.ConditionTrue, resizing.Status,
			"Recommend-mode policy must not stay Resizing=True")
	}
}

// TestAPIFault_TimeoutAfterCommittedStatusWrite lets the apiserver commit a
// status write, then returns a timeout to the reconciler. A retry with a
// fresh GET must be idempotent: no extra history row, no stuck in-flight
// resize, status still converges.
func TestAPIFault_TimeoutAfterCommittedStatusWrite(t *testing.T) {
	cfg, plain := startIsolatedEnvtest(t)
	key := createFaultFixture(t, plain, "fault-timeout", "timeout-app", "policy-timeout")
	seeded := seedResizeHistory(t, plain, key, "timeout-app")

	var statusWrites atomic.Int32
	intercepted := interceptor.NewClient(plain, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !isPolicyStatusUpdate(subResource, obj) {
				return cl.SubResource(subResource).Update(ctx, obj, opts...)
			}
			if err := cl.Status().Update(ctx, obj, opts...); err != nil {
				return err
			}
			if statusWrites.Add(1) == 1 {
				return apierrors.NewTimeoutError("injected timeout after committed status write", 0)
			}
			return nil
		},
	})

	r := newFaultReconciler(t, intercepted, cfg)
	req := ctrl.Request{NamespacedName: key}

	_, firstErr := r.Reconcile(context.Background(), req)
	require.Error(t, firstErr, "first status write must surface the injected timeout")
	assert.ErrorContains(t, firstErr, "updating status")
	assert.GreaterOrEqual(t, statusWrites.Load(), int32(1), "interceptor must commit then fail")

	var afterTimeout attunev1alpha1.AttunePolicy
	require.NoError(t, plain.Get(context.Background(), key, &afterTimeout),
		"committed write must be visible via a fresh GET")
	require.NotEmpty(t, afterTimeout.Status.Conditions,
		"status must have landed even though the client saw a timeout")
	assertHistoryUnchanged(t, seeded, afterTimeout.Status.ResizeHistory)
	recsAfterTimeout := recWorkloadKeys(afterTimeout.Status.Recommendations)
	require.NotEmpty(t, recsAfterTimeout, "first committed status must include recommendations")

	_, retryErr := r.Reconcile(context.Background(), req)
	require.NoError(t, retryErr, "retry after timeout must succeed")

	var final attunev1alpha1.AttunePolicy
	require.NoError(t, plain.Get(context.Background(), key, &final))
	assertHistoryUnchanged(t, seeded, final.Status.ResizeHistory)
	assert.Equal(t, recsAfterTimeout, recWorkloadKeys(final.Status.Recommendations),
		"retry must not append a second copy of the same recommendations")
	assertReadyConverged(t, &final)
	assert.Zero(t, final.Status.Workloads.Resized,
		"Recommend mode must not record an in-flight resize after the timeout retry")
}

// TestAPIFault_Status409ThenSuccess returns Conflict on the first two
// status writes, then passthrough. Reconcile must retry (not panic) and
// leave a schema-valid status.
func TestAPIFault_Status409ThenSuccess(t *testing.T) {
	cfg, plain := startIsolatedEnvtest(t)
	key := createFaultFixture(t, plain, "fault-409", "conflict-app", "policy-409")
	seeded := seedResizeHistory(t, plain, key, "conflict-app")

	var conflicts atomic.Int32
	intercepted := interceptor.NewClient(plain, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !isPolicyStatusUpdate(subResource, obj) {
				return cl.SubResource(subResource).Update(ctx, obj, opts...)
			}
			if n := conflicts.Add(1); n <= 2 {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "attune.io", Resource: "attunepolicies"},
					obj.GetName(),
					fmt.Errorf("injected status conflict %d", n),
				)
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})

	r := newFaultReconciler(t, intercepted, cfg)
	req := ctrl.Request{NamespacedName: key}

	var result ctrl.Result
	var err error
	require.NotPanics(t, func() {
		result, err = r.Reconcile(context.Background(), req)
	})
	require.NoError(t, err, "two status 409s then success must not fail the reconcile")
	assert.Greater(t, result.RequeueAfter, time.Duration(0),
		"successful reconcile should requeue, not drop the update")
	assert.GreaterOrEqual(t, conflicts.Load(), int32(3),
		"interceptor must see two conflicts plus the successful write")

	var final attunev1alpha1.AttunePolicy
	require.NoError(t, plain.Get(context.Background(), key, &final))
	require.NotEmpty(t, final.Status.Conditions, "status must be written after the 409 storm")
	assert.Equal(t, int32(1), final.Status.Workloads.Discovered)
	assertHistoryUnchanged(t, seeded, final.Status.ResizeHistory)
	assertReadyConverged(t, &final)
}

// TestAPIFault_ManagerRestartMidReconcile stops the manager after the first
// status write and starts a new manager against the same apiserver.
// The second process must not duplicate recommendation generation, and
// deleting the policy must still clear the cleanup finalizer.
func TestAPIFault_ManagerRestartMidReconcile(t *testing.T) {
	cfg, plain := startIsolatedEnvtest(t)
	key := createFaultFixture(t, plain, "fault-restart", "restart-app", "policy-restart")
	seeded := seedResizeHistory(t, plain, key, "restart-app")

	startMgr := func(t *testing.T) (context.CancelFunc, <-chan error) {
		t.Helper()
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			LeaderElection:         false,
			HealthProbeBindAddress: "0",
			Metrics:                metricsserver.Options{BindAddress: "0"},
		})
		require.NoError(t, err, "create manager")

		r := newFaultReconciler(t, mgr.GetClient(), cfg)
		r.Recorder = mgr.GetEventRecorder("attune-api-fault")
		// Unique controller name: controller-runtime registers names in a
		// process-wide map (Prometheus metric collision). TestMain already
		// owns "attunepolicy", and -count=N plus the restarted manager
		// each need a fresh name.
		name := fmt.Sprintf("attunepolicy-fault-%d", faultMgrSeq.Add(1))
		require.NoError(t, ctrl.NewControllerManagedBy(mgr).
			Named(name).
			For(&attunev1alpha1.AttunePolicy{}).
			Complete(r), "setup controller")

		mgrCtx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- mgr.Start(mgrCtx)
		}()
		t.Cleanup(func() { cancel() })
		require.True(t, mgr.GetCache().WaitForCacheSync(mgrCtx), "cache sync")
		return cancel, done
	}

	stopMgr := func(t *testing.T, cancel context.CancelFunc, done <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "manager stop")
		case <-time.After(20 * time.Second):
			t.Fatal("manager did not stop")
		}
	}

	cancel1, done1 := startMgr(t)

	var first attunev1alpha1.AttunePolicy
	require.Eventually(t, func() bool {
		if err := plain.Get(context.Background(), key, &first); err != nil {
			return false
		}
		return len(first.Status.Conditions) > 0 && len(first.Status.Recommendations) > 0
	}, 30*time.Second, 200*time.Millisecond, "first manager must write status and recommendations")

	firstGen := first.Generation
	firstRecs := recWorkloadKeys(first.Status.Recommendations)
	require.NotEmpty(t, firstRecs, "first manager must produce recommendations before restart")
	assert.Contains(t, first.Finalizers, "attune.io/cleanup")
	assertHistoryUnchanged(t, seeded, first.Status.ResizeHistory)

	stopMgr(t, cancel1, done1)

	cancel2, done2 := startMgr(t)
	t.Cleanup(func() { stopMgr(t, cancel2, done2) })

	var second attunev1alpha1.AttunePolicy
	require.Eventually(t, func() bool {
		if err := plain.Get(context.Background(), key, &second); err != nil {
			return false
		}
		if len(second.Status.Conditions) == 0 {
			return false
		}
		reason := readyReason(&second)
		return reason == attunev1alpha1.ReasonInsufficientData ||
			reason == attunev1alpha1.ReasonMonitoring
	}, 30*time.Second, 200*time.Millisecond, "second manager must reconverge Ready")

	assert.Equal(t, firstGen, second.Generation,
		"restart must not bump spec generation")
	assertHistoryUnchanged(t, seeded, second.Status.ResizeHistory)
	assert.Equal(t, firstRecs, recWorkloadKeys(second.Status.Recommendations),
		"restart must not apply a second copy of the same recommendations")
	assertReadyConverged(t, &second)

	require.NoError(t, plain.Delete(context.Background(), &second))
	require.Eventually(t, func() bool {
		var gone attunev1alpha1.AttunePolicy
		return apierrors.IsNotFound(plain.Get(context.Background(), key, &gone))
	}, 30*time.Second, 200*time.Millisecond,
		"cleanup finalizer must be removed so the policy does not stay Terminating")
}
