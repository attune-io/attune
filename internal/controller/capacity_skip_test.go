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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestRecordCapacitySkip(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cap-test", Namespace: "ns-cap"},
	}
	beforeAlloc := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "allocatable"))
	beforePress := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "pressure"))

	recordCapacitySkip(policy, "total pod requests would exceed node allocatable")
	recordCapacitySkip(policy, "node has MemoryPressure; skipping memory request increase")
	// Producer strings from nodePressureBlocksIncrease must map to the same pressure label.
	recordCapacitySkip(policy, "node has DiskPressure; skipping memory request increase")
	recordCapacitySkip(policy, "node has PIDPressure; skipping CPU request increase")
	recordCapacitySkip(policy, "quota/limitrange violation: too large") // no metric

	assert.Equal(t, beforeAlloc+1, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "allocatable")))
	assert.Equal(t, beforePress+3, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "pressure")))
}

func TestComputeSavings_ReclaimedAliases(t *testing.T) {
	r := newSavingsReconciler()
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "app",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("200m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}
	savings, acc := r.computeSavings(recs, nil)
	assert.Equal(t, savings.CPURequestReduction, savings.ReclaimedCPURequest)
	assert.Equal(t, savings.MemoryRequestReduction, savings.ReclaimedMemoryRequest)
	assert.NotEmpty(t, savings.ReclaimedCPURequest)
	assert.NotEmpty(t, savings.ReclaimedMemoryRequest)
	assert.Greater(t, acc.totalCPUSaved, int64(0))
}
