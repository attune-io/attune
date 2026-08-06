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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestApplyStatusBudget_CapsAndStripsExplanations(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.MaxStatusRecommendations = 2
	include := false
	r.IncludeExplanationsInStatus = &include

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
	}

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "small",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "c",
				Current: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("100m"),
					MemoryRequest: resource.MustParse("100Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("110m"),
					MemoryRequest: resource.MustParse("110Mi"),
				},
				Explanation: &attunev1alpha1.ContainerRecommendationExplanation{},
			}},
		},
		{
			Workload: "large",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "c",
				Current: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("100m"),
					MemoryRequest: resource.MustParse("100Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("500m"),
					MemoryRequest: resource.MustParse("1Gi"),
				},
				Explanation: &attunev1alpha1.ContainerRecommendationExplanation{},
			}},
		},
		{
			Workload: "medium",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "c",
				Current: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("100m"),
					MemoryRequest: resource.MustParse("100Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("200m"),
					MemoryRequest: resource.MustParse("200Mi"),
				},
				Explanation: &attunev1alpha1.ContainerRecommendationExplanation{},
			}},
		},
	}

	out := r.applyStatusBudget(recs, policy)
	require.Len(t, out, 2)
	// Full set untouched.
	assert.Len(t, recs, 3)
	assert.NotNil(t, recs[0].Containers[0].Explanation)
	// Status copy has no explanations and prefers large change.
	names := map[string]bool{}
	for _, rec := range out {
		names[rec.Workload] = true
		assert.Nil(t, rec.Containers[0].Explanation)
	}
	assert.True(t, names["large"])
	assert.False(t, names["small"])
}

func TestAddRequeueJitter_Deterministic(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.RequeueJitter = 5 * time.Minute
	p1 := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{UID: types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")}}
	p2 := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{UID: types.UID("ffffffff-1111-2222-3333-444444444444")}}
	base := time.Hour
	j1 := r.addRequeueJitter(base, p1)
	j1b := r.addRequeueJitter(base, p1)
	j2 := r.addRequeueJitter(base, p2)
	assert.Equal(t, j1, j1b)
	assert.GreaterOrEqual(t, j1, base)
	assert.Less(t, j1, base+r.RequeueJitter)
	// Different UIDs usually differ (collision theoretically possible but rare).
	assert.NotEqual(t, j1, j2)
	// Disabled
	r.RequeueJitter = 0
	assert.Equal(t, base, r.addRequeueJitter(base, p1))
}

func TestPromQLBuilder_FromPolicy(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				PodAggregation:        "Avg",
				CPURecordingMetric:    "rule:cpu",
				MemoryRecordingMetric: "rule:mem",
			},
		},
	}
	qb := r.promQLBuilder(policy)
	q := qb.BuildQuery("ns", "pod-.*", "", "cpu", 5*time.Minute)
	assert.Contains(t, q, "avg by (container)")
	assert.Contains(t, q, "rule:cpu")
}
