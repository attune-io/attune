/*
Copyright 2026.
*/

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func TestParseHistoryWindow_OperatorMaxClamp(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.MaxHistoryWindow = 48 * time.Hour
	hw := metav1.Duration{Duration: 168 * time.Hour}
	p := &attunev1alpha1.AttunePolicy{Spec: attunev1alpha1.AttunePolicySpec{
		MetricsSource: attunev1alpha1.MetricsSource{HistoryWindow: &hw},
	}}
	assert.Equal(t, 48*time.Hour, r.parseHistoryWindow(p))
}

func TestGetQueryStep_OperatorMinClamp(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.MinQueryStep = 10 * time.Minute
	qs := metav1.Duration{Duration: 5 * time.Minute}
	p := &attunev1alpha1.AttunePolicy{Spec: attunev1alpha1.AttunePolicySpec{
		MetricsSource: attunev1alpha1.MetricsSource{QueryStep: &qs},
	}}
	assert.Equal(t, 10*time.Minute, r.getQueryStep(p))
}

func TestShouldRefreshBlockers(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.BlockerRefreshInterval = 5 * time.Minute
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return fixed })
	p := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}

	assert.True(t, r.shouldRefreshBlockers(p, true))  // always when resizing
	assert.True(t, r.shouldRefreshBlockers(p, false)) // first time
	r.markBlockersRefreshed(p)
	assert.False(t, r.shouldRefreshBlockers(p, false))
	// Throttle hold: still force refresh when needPods (resize path).
	assert.True(t, r.shouldRefreshBlockers(p, true))
	r.SetNowFunc(func() time.Time { return fixed.Add(6 * time.Minute) })
	assert.True(t, r.shouldRefreshBlockers(p, false))
}

func TestMaxProfileSamples(t *testing.T) {
	r := NewAttunePolicyReconciler()
	assert.Equal(t, rsmetrics.DefaultMaxProfileSamples, r.maxProfileSamples())
	r.MaxProfileSamples = 500
	assert.Equal(t, 500, r.maxProfileSamples())
	r.MaxProfileSamples = -1
	assert.Equal(t, 0, r.maxProfileSamples(), "negative disables (unlimited)")
	r.MaxProfileSamples = 0
	assert.Equal(t, rsmetrics.DefaultMaxProfileSamples, r.maxProfileSamples())
}

func TestLiveReader_FallsBackToClient(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	assert.Same(t, c, r.liveReader())

	api := fake.NewClientBuilder().WithScheme(scheme).Build()
	r.APIReader = api
	assert.Same(t, api, r.liveReader())
}

func TestJoinSampleNames(t *testing.T) {
	assert.Equal(t, "none", joinSampleNames(nil))
	assert.Equal(t, "none", joinSampleNames([]string{}))
	assert.Equal(t, "a, b", joinSampleNames([]string{"a", "b"}))
}
