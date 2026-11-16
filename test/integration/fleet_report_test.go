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
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/attune-io/attune/internal/fleetreport"
)

func TestFleetReportExport_WritesConfigMap(t *testing.T) {
	ctx := context.Background()
	const ns = "integration-test"

	deploy := newTestDeployment("fleet-demo", ns)
	require.NoError(t, k8sClient.Create(ctx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), deploy) })

	polName := "fleet-pol-" + time.Now().Format("150405")
	policy := newTestPolicy(polName, ns, deploy.Name)
	require.NoError(t, k8sClient.Create(ctx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), policy) })

	cmName := "attune-fleet-report-" + polName
	exp := &fleetreport.Exporter{
		Client:    k8sClient,
		Log:       logr.Discard(),
		Namespace: ns,
		Name:      cmName,
		ClusterID: "integration-test",
		Interval:  time.Hour,
		Now:       func() time.Time { return time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC) },
	}
	exp.ExportOnce(ctx)

	var got corev1.ConfigMap
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &got))
	assert.Equal(t, "true", got.Labels[fleetreport.ConfigMapLabel])
	assert.Equal(t, fleetreport.SchemaVersion, got.Data["schema-version"])
	var decoded fleetreport.Report
	require.NoError(t, json.Unmarshal([]byte(got.Data["report.json"]), &decoded))
	assert.GreaterOrEqual(t, decoded.PolicyCount, 1)
	assert.Equal(t, "integration-test", decoded.ClusterID)
	assert.Equal(t, fleetreport.SchemaVersion, decoded.SchemaVersion)

	exp.ExportOnce(ctx)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &got))
	assert.NotEmpty(t, got.Data["report.json"])
}

// TestFleetReportExport_OperatorNamespaceNotWatched covers #1137. With
// --watch-namespaces set, the manager cache only knows the listed namespaces.
// A cached Get on the fleet report ConfigMap in the operator namespace fails
// with an unknown-namespace error, not NotFound, so the export never wrote
// the ConfigMap. The exporter wiring here matches cmd/manager/main.go.
func TestFleetReportExport_OperatorNamespaceNotWatched(t *testing.T) {
	cfg, plain := startIsolatedEnvtest(t)
	ctx := context.Background()
	const watched, operatorNS = "fleet-watched", "fleet-operator"
	for _, ns := range []string{watched, operatorNS} {
		require.NoError(t, plain.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme.Scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		WebhookServer:  webhookserver.NewServer(webhookserver.Options{Port: 0}),
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{watched: {}},
		},
	})
	require.NoError(t, err)

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("manager did not stop")
		}
	})
	require.True(t, mgr.GetCache().WaitForCacheSync(mgrCtx))

	key := types.NamespacedName{Namespace: operatorNS, Name: "attune-fleet-report"}

	// The premise of the bug: the cached client cannot read outside the
	// watched namespaces, and the error is not NotFound.
	getErr := mgr.GetClient().Get(ctx, key, &corev1.ConfigMap{})
	require.Error(t, getErr)
	assert.False(t, apierrors.IsNotFound(getErr), "cached Get must not look like NotFound: %v", getErr)
	t.Logf("cached Get outside the watched namespaces: %v", getErr)

	exp := &fleetreport.Exporter{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Log:       logr.Discard(),
		Namespace: operatorNS,
		Name:      key.Name,
		ClusterID: "integration-test",
		Interval:  time.Hour,
		Now:       func() time.Time { return time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC) },
	}
	exp.ExportOnce(ctx)

	var got corev1.ConfigMap
	require.NoError(t, plain.Get(ctx, key, &got), "first export must create the ConfigMap")
	assert.Equal(t, "true", got.Labels[fleetreport.ConfigMapLabel])
	firstRV := got.ResourceVersion

	// The second export takes the update path: Get finds it, then Patch.
	exp.Now = func() time.Time { return time.Date(2026, 8, 3, 16, 0, 0, 0, time.UTC) }
	exp.ExportOnce(ctx)
	require.NoError(t, plain.Get(ctx, key, &got))
	assert.NotEqual(t, firstRV, got.ResourceVersion, "second export must patch the ConfigMap")
}
