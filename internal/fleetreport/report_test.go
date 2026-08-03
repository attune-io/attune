/*
Copyright 2026.
*/
package fleetreport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestBuild_AggregatesPolicies(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	policies := []attunev1alpha1.AttunePolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"},
			Spec: attunev1alpha1.AttunePolicySpec{
				UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
			},
			Status: attunev1alpha1.AttunePolicyStatus{
				Conditions: []metav1.Condition{{
					Type: attunev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: attunev1alpha1.ReasonMonitoring,
				}},
				Workloads: attunev1alpha1.WorkloadStatus{Discovered: 3, WithRecommendations: 2, Resized: 1},
				Savings: attunev1alpha1.SavingsStatus{
					EstimatedMonthlySavings: "$10.50",
					ReclaimedCPURequest:     "500m",
					ReclaimedMemoryRequest:  "256Mi",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns2"},
			Spec: attunev1alpha1.AttunePolicySpec{
				UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeRecommend},
			},
			Status: attunev1alpha1.AttunePolicyStatus{
				Conditions: []metav1.Condition{{
					Type: attunev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: attunev1alpha1.ReasonInsufficientData,
				}},
				Workloads: attunev1alpha1.WorkloadStatus{Discovered: 1},
			},
		},
	}
	r := Build(policies, "prod-us", now)
	assert.Equal(t, SchemaVersion, r.SchemaVersion)
	assert.Equal(t, "prod-us", r.ClusterID)
	assert.Equal(t, 2, r.PolicyCount)
	assert.Equal(t, 1, r.PoliciesByMode[string(attunev1alpha1.UpdateTypeAuto)])
	assert.Equal(t, 1, r.PoliciesByMode[string(attunev1alpha1.UpdateTypeRecommend)])
	assert.Equal(t, 1, r.ReadyTrue)
	assert.Equal(t, 1, r.ReadyFalse)
	assert.Equal(t, 1, r.InsufficientData)
	assert.Equal(t, 4, r.WorkloadsDiscovered)
	assert.Equal(t, 2, r.WorkloadsWithRecommendations)
	assert.Equal(t, 1, r.WorkloadsResized)
	assert.InDelta(t, 10.50, r.EstimatedMonthlySavingsUSD, 0.001)
	assert.Equal(t, int64(500), r.ReclaimedCPURequestMilli)
	assert.Equal(t, int64(256*1024*1024), r.ReclaimedMemoryRequestBytes)

	raw, err := MarshalJSONDocument(r)
	require.NoError(t, err)
	var decoded Report
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, r.PolicyCount, decoded.PolicyCount)

	cm, err := ConfigMapFromReport("attune-system", "attune-fleet-report", r)
	require.NoError(t, err)
	assert.Equal(t, "true", cm.Labels[ConfigMapLabel])
	assert.Contains(t, cm.Data["report.json"], `"schemaVersion": "v1"`)
}

func TestParseHelpers(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 12.5, parseUSD("$12.50"))
	m, ok := parseCPUMilli("250m")
	assert.True(t, ok)
	assert.Equal(t, int64(250), m)
	b, ok := parseMemoryBytes("1Gi")
	assert.True(t, ok)
	assert.Equal(t, int64(1024*1024*1024), b)
}
