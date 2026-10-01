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
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/argorollout"
	"github.com/attune-io/attune/internal/controller"
	"github.com/attune-io/attune/internal/metrics"
)

// TestManagerStartsWithoutRolloutCRD proves mgr.Start stays up when the
// Rollout CRD is not installed. The fake client never calls RESTMapper, so
// a unit test cannot show this. The manager uses the production cache
// shape: DisableFor includes the local Rollout type, and ByObject does not.
func TestManagerStartsWithoutRolloutCRD(t *testing.T) {
	cfg, plain := startIsolatedEnvtest(t)
	nsName := "rollout-crd-missing"
	require.NoError(t, plain.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}))

	deploy := newTestDeployment("api", nsName)
	require.NoError(t, plain.Create(context.Background(), deploy))

	depPolicy := newTestPolicy("dep-policy", nsName, "api")
	require.NoError(t, plain.Create(context.Background(), depPolicy))

	rolloutPolicy := newTestPolicy("ro-policy", nsName, "checkout")
	rolloutPolicy.Spec.TargetRef.Kind = argorollout.Kind
	require.NoError(t, plain.Create(context.Background(), rolloutPolicy))

	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, attunev1alpha1.AddToScheme(sch))
	require.NoError(t, argorollout.AddToScheme(sch))

	byObject := map[client.Object]cache.ByObject{
		&corev1.Pod{}:        {},
		&appsv1.Deployment{}: {},
	}
	for obj := range byObject {
		_, isRollout := obj.(*argorollout.Rollout)
		require.False(t, isRollout, "ByObject must not watch Rollout")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 sch,
		LeaderElection:         false,
		HealthProbeBindAddress: fmt.Sprintf("127.0.0.1:%d", port),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		WebhookServer:          webhookserver.NewServer(webhookserver.Options{Port: 0}),
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{nsName: {}},
			ByObject:          byObject,
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.Secret{},
					&argorollout.Rollout{},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.AddHealthzCheck("healthz", healthz.Ping))
	require.NoError(t, mgr.AddReadyzCheck("readyz", healthz.Ping))

	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	reconciler := controller.NewAttunePolicyReconciler()
	reconciler.Client = mgr.GetClient()
	reconciler.APIReader = mgr.GetAPIReader()
	reconciler.Scheme = mgr.GetScheme()
	reconciler.RESTMapper = mgr.GetRESTMapper()
	reconciler.Clientset = cs
	reconciler.Recorder = mgr.GetEventRecorder("attune-rollout-crd")
	reconciler.MinCooldown = time.Second
	reconciler.PrometheusTimeout = 30 * time.Second
	reconciler.MetricsFactory = func(string, *metrics.CollectorOptions) (metrics.MetricsCollector, error) {
		return defaultMetricsFactory("", nil)
	}

	// TestMain already registered the name "attunepolicy" in this process.
	// SetupWithManager would collide on that name. The watch is still only
	// AttunePolicy, which is what SetupWithManager registers.
	name := fmt.Sprintf("attunepolicy-rollout-crd-%d", faultMgrSeq.Add(1))
	require.NoError(t, ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&attunev1alpha1.AttunePolicy{}).
		Complete(reconciler))

	mgrCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- mgr.Start(mgrCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case startErr := <-done:
			if startErr != nil && !errors.Is(startErr, context.Canceled) {
				t.Errorf("manager stop: %v", startErr)
			}
		case <-time.After(20 * time.Second):
			t.Errorf("manager did not stop")
		}
	})
	require.True(t, mgr.GetCache().WaitForCacheSync(mgrCtx))

	readyURL := fmt.Sprintf("http://127.0.0.1:%d/readyz", port)
	require.Eventually(t, func() bool {
		reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer reqCancel()
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodGet, readyURL, nil)
		if reqErr != nil {
			return false
		}
		resp, getErr := http.DefaultClient.Do(req)
		if getErr != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 20*time.Second, 100*time.Millisecond, "readyz")

	depKey := types.NamespacedName{Name: "dep-policy", Namespace: nsName}
	require.Eventually(t, func() bool {
		var got attunev1alpha1.AttunePolicy
		if getErr := plain.Get(context.Background(), depKey, &got); getErr != nil {
			return false
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, attunev1alpha1.ConditionReady)
		if cond == nil {
			return false
		}
		return cond.Reason != attunev1alpha1.ReasonWorkloadCRDMissing
	}, 30*time.Second, 200*time.Millisecond, "Deployment policy must not report a missing Rollout CRD")

	roKey := types.NamespacedName{Name: "ro-policy", Namespace: nsName}
	require.Eventually(t, func() bool {
		var got attunev1alpha1.AttunePolicy
		if getErr := plain.Get(context.Background(), roKey, &got); getErr != nil {
			return false
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, attunev1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return false
		}
		if cond.Reason != attunev1alpha1.ReasonWorkloadCRDMissing {
			return false
		}
		return cond.Message == "argoproj.io/v1alpha1 Rollout CRD is not installed"
	}, 30*time.Second, 200*time.Millisecond, "Rollout policy reports the missing CRD")

	finalCtx, finalCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finalCancel()
	finalReq, err := http.NewRequestWithContext(finalCtx, http.MethodGet, readyURL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(finalReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "manager still running after the missing-CRD reconcile")
}
