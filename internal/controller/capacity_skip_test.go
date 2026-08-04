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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// TestExecuteResizes_MemoryPressure_EmitsResizeSkippedAndCapacitySkipMetric
// pins the full resize path: shouldSkipResize (live node MemoryPressure) →
// ResizeSkipped event + CapacitySkipTotal{reason=pressure}. The helper-only
// TestRecordCapacitySkip does not cover this call site.
func TestExecuteResizes_MemoryPressure_EmitsResizeSkippedAndCapacitySkipMetric(t *testing.T) {
	const (
		policyNS   = "default"
		policyName = "pressure-path-policy"
		nodeName   = "pressure-node"
		appName    = "pressure-app"
	)

	pod := newResizePod(appName, "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = nodeName
	deploy := newTestDeployment(appName, policyNS, map[string]string{"app": appName})
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeMemoryPressure,
				Status: corev1.ConditionTrue,
			}},
		},
	}

	reconciler, _ := newResizeReconciler(pod, deploy, node)
	// Clientset is preferred for live node status; include the pressure node.
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), node.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy(policyName, policyNS)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	// Memory request increase (CPU flat) under MemoryPressure must skip.
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation(appName,
			"500m", "512Mi", "1000m", "1Gi",
			"500m", "1Gi", "1000m", "2Gi"),
	}

	beforePress := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure"))
	beforeAlloc := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "allocatable"))

	count, history := reconciler.executeResizes(
		context.Background(),
		policy,
		[]client.Object{deploy},
		recommendations,
		podMap(appName, pod),
		nil,
		nil,
	)
	assert.Equal(t, 0, count, "memory increase under MemoryPressure must not resize")
	assert.Empty(t, history, "skipped resize produces no history entries")

	assert.Equal(t, beforePress+1,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure")),
		"resize path must increment CapacitySkipTotal reason=pressure")
	assert.Equal(t, beforeAlloc,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "allocatable")),
		"pressure skip must not increment allocatable reason")

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "MemoryPressure") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped event mentioning MemoryPressure")
			return
		}
	}
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
