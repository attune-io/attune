/*
Copyright 2026.
*/

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
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
