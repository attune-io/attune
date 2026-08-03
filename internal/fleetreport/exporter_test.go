/*
Copyright 2026.
*/
package fleetreport

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestExporter_exportOnce_WritesConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "attune-system"}}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Workloads: attunev1alpha1.WorkloadStatus{Discovered: 2},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, policy).Build()
	e := &Exporter{
		Client:    c,
		Log:       logr.Discard(),
		Namespace: "attune-system",
		Name:      "attune-fleet-report",
		ClusterID: "test-cluster",
		Interval:  time.Hour,
		Now:       func() time.Time { return time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC) },
	}
	e.ExportOnce(context.Background())

	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "attune-system", Name: "attune-fleet-report"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "v1", cm.Data["schema-version"])
	assert.Equal(t, "test-cluster", cm.Data["cluster-id"])
	assert.Contains(t, cm.Data["report.json"], `"policyCount": 1`)

	// Second export updates
	e.ExportOnce(context.Background())
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "attune-system", Name: "attune-fleet-report"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "true", cm.Labels[ConfigMapLabel])
}

func TestExporter_NeedLeaderElection(t *testing.T) {
	t.Parallel()
	assert.True(t, (&Exporter{}).NeedLeaderElection())
}

func TestExporter_Start_StopsOnCancel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "attune-system"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	e := &Exporter{
		Client:    c,
		Log:       logr.Discard(),
		Namespace: "attune-system",
		// Empty name uses default attune-fleet-report
		ClusterID: "c",
		Interval:  50 * time.Millisecond,
		Now:       func() time.Time { return time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Start(ctx) }()
	// Allow initial ExportOnce + at least one tick, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "attune-system", Name: "attune-fleet-report"}, &cm)
	require.NoError(t, err, "Start should default Name and export ConfigMap")
	assert.Equal(t, "v1", cm.Data["schema-version"])
}
