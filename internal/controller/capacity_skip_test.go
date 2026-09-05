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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestRecordCapacitySkip(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cap-test", Namespace: "ns-cap"},
	}
	beforeAlloc := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "allocatable"))
	beforePress := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "pressure"))
	beforeUnavail := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "unavailable"))
	beforeNeighbors := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "neighbors"))

	// Exact producer strings from shouldSkipResize / nodePressureBlocksIncrease.
	recordCapacitySkip(policy, "total pod requests would exceed node allocatable")
	recordCapacitySkip(policy, "node has MemoryPressure; skipping memory request increase")
	recordCapacitySkip(policy, "node has DiskPressure; skipping resource request increase")
	recordCapacitySkip(policy, "node has PIDPressure; skipping resource request increase")
	recordCapacitySkip(policy, "node status unavailable; skipping request increase")
	recordCapacitySkip(policy, "node free request budget exceeded by neighbors")
	recordCapacitySkip(policy, "node neighbor list unavailable; skipping request increase")
	recordCapacitySkip(policy, "quota/limitrange violation: too large")                  // no metric
	recordCapacitySkip(policy, "")                                                       // no metric
	recordCapacitySkip(nil, "node has MemoryPressure; skipping memory request increase") // no metric

	assert.Equal(t, beforeAlloc+1, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "allocatable")))
	assert.Equal(t, beforePress+3, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "pressure")))
	assert.Equal(t, beforeUnavail+1, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "unavailable")))
	assert.Equal(t, beforeNeighbors+2, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("ns-cap", "cap-test", "neighbors")))
}

// TestGetNodeForResize_PrefersClientsetThenFallsBack covers live vs cache
// fetch order when Clientset errors or is unset.
func TestGetNodeForResize_PrefersClientsetThenFallsBack(t *testing.T) {
	scheme := testScheme()
	cachedOnly := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n-fallback"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue,
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cachedOnly).Build()

	// Clientset miss → fall back to controller-runtime client.
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Clientset = kubefake.NewSimpleClientset() // no node
	got := r.getNodeForResize(context.Background(), "n-fallback")
	require.NotNil(t, got)
	assert.Equal(t, "n-fallback", got.Name)

	// No Clientset, client hit.
	r2 := NewAttunePolicyReconciler()
	r2.Client = fakeClient
	got2 := r2.getNodeForResize(context.Background(), "n-fallback")
	require.NotNil(t, got2)

	// Neither source has the node.
	r3 := NewAttunePolicyReconciler()
	r3.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	r3.Clientset = kubefake.NewSimpleClientset()
	assert.Nil(t, r3.getNodeForResize(context.Background(), "missing"))
}

// TestExecuteResizes_MemoryPressure_LiveRecheckAfterStaleCache ensures a
// per-cycle nodeCache without pressure cannot authorize a memory increase
// when the live Clientset reports MemoryPressure=True at apply time.
func TestExecuteResizes_MemoryPressure_LiveRecheckAfterStaleCache(t *testing.T) {
	const (
		policyNS   = "default"
		policyName = "pressure-recheck-policy"
		nodeName   = "stale-cache-node"
		appName    = "recheck-app"
	)

	pod := newResizePod(appName, "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = nodeName
	deploy := newTestDeployment(appName, policyNS, map[string]string{"app": appName})

	staleNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeMemoryPressure,
				Status: corev1.ConditionFalse,
			}},
		},
	}
	liveNode := staleNode.DeepCopy()
	liveNode.Status.Conditions = []corev1.NodeCondition{{
		Type:   corev1.NodeMemoryPressure,
		Status: corev1.ConditionTrue,
	}}

	// Client holds stale (no pressure); Clientset holds live (pressure).
	// shouldSkipResize may use checks.nodeCache seeded from the first Get;
	// the last-moment re-check must still see Clientset pressure.
	reconciler, _ := newResizeReconciler(pod, deploy, staleNode)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), liveNode)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy(policyName, policyNS)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation(appName,
			"500m", "512Mi", "1000m", "1Gi",
			"500m", "1Gi", "1000m", "2Gi"),
	}

	// Seed stale cache as a real reconcile cycle would after a False read.
	checks := &resizePreChecks{}
	checks.nodeCache.Store(nodeName, staleNode)

	beforePress := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure"))
	count, _ := reconciler.executeResizes(
		context.Background(),
		policy,
		[]client.Object{deploy},
		recommendations,
		podMap(appName, pod),
		nil,
		checks,
	)
	assert.Equal(t, 0, count, "live re-check must block despite stale nodeCache")
	assert.Equal(t, beforePress+1,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure")))

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "MemoryPressure") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped from live pressure re-check")
			return
		}
	}
}

// TestExecuteResizes_NodeAllocatable_EmitsResizeSkippedAndCapacitySkipMetric
// pins the allocatable skip path (sibling of the pressure path test).
func TestExecuteResizes_NodeAllocatable_EmitsResizeSkippedAndCapacitySkipMetric(t *testing.T) {
	const (
		policyNS   = "default"
		policyName = "alloc-path-policy"
		nodeName   = "tiny-node"
		appName    = "alloc-app"
	)

	pod := newResizePod(appName, "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = nodeName
	deploy := newTestDeployment(appName, policyNS, map[string]string{"app": appName})
	// Node too small for recommended CPU (would be 2 + sidecar-less 2).
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	reconciler, _ := newResizeReconciler(pod, deploy, node)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), node.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy(policyName, policyNS)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	// Target CPU 2000m alone exceeds node allocatable 1000m.
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation(appName,
			"500m", "512Mi", "1000m", "1Gi",
			"2000m", "512Mi", "2000m", "1Gi"),
	}

	beforeAlloc := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "allocatable"))
	beforePress := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure"))

	count, history := reconciler.executeResizes(
		context.Background(),
		policy,
		[]client.Object{deploy},
		recommendations,
		podMap(appName, pod),
		nil,
		nil,
	)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	assert.Equal(t, beforeAlloc+1,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "allocatable")))
	assert.Equal(t, beforePress,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "pressure")))

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "allocatable") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped event mentioning allocatable")
			return
		}
	}
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

// TestShouldSkipResize_NilNodeBlocksIncrease allows decreases when the node
// cannot be loaded (#483 fail-closed for increases only).
func TestShouldSkipResize_NilNodeBlocksIncrease(t *testing.T) {
	scheme := testScheme()
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	r.Clientset = kubefake.NewSimpleClientset() // no nodes
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "missing-node",
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	higherMem := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	lowerMem := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), &attunev1alpha1.AttunePolicy{}, pod, rec, higherMem, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "node status unavailable")

	skip, reason = r.shouldSkipResize(context.Background(), &attunev1alpha1.AttunePolicy{}, pod, rec, lowerMem, nil)
	assert.False(t, skip, "decreases must not be blocked when node is unavailable: %s", reason)
}

// TestExecuteResizes_NilNode_BlocksIncreaseEmitsEventAndMetric covers the
// full resize path when node Get fails and the target is a memory increase.
func TestExecuteResizes_NilNode_BlocksIncreaseEmitsEventAndMetric(t *testing.T) {
	const (
		policyNS   = "default"
		policyName = "nil-node-policy"
		appName    = "nil-node-app"
	)

	pod := newResizePod(appName, "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = "does-not-exist"
	deploy := newTestDeployment(appName, policyNS, map[string]string{"app": appName})

	reconciler, _ := newResizeReconciler(pod, deploy)
	// Clientset has only the pod; no node object.
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy(policyName, policyNS)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation(appName,
			"500m", "512Mi", "1000m", "1Gi",
			"500m", "1Gi", "1000m", "2Gi"),
	}

	beforeUnavail := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "unavailable"))
	count, history := reconciler.executeResizes(
		context.Background(),
		policy,
		[]client.Object{deploy},
		recommendations,
		podMap(appName, pod),
		nil,
		nil,
	)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	assert.Equal(t, beforeUnavail+1,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "unavailable")))

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "node status unavailable") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped for unavailable node")
			return
		}
	}
}

// TestExecuteResizes_NilNode_LiveRecheckBlocksIncrease after shouldSkip
// saw a cached node, live Get nil still fail-closes increases (#483).
func TestExecuteResizes_NilNode_LiveRecheckBlocksIncrease(t *testing.T) {
	const (
		policyNS   = "default"
		policyName = "nil-live-recheck-policy"
		nodeName   = "cached-only-node"
		appName    = "live-nil-app"
	)

	pod := newResizePod(appName, "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = nodeName
	deploy := newTestDeployment(appName, policyNS, map[string]string{"app": appName})
	cachedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse,
			}},
		},
	}

	// Client has the node for shouldSkip; Clientset has only the pod so live
	// re-check getNodeForResize fails after Clientset miss and client... wait,
	// getNodeForResize tries Clientset first then Client. If Client has the
	// node, live re-check succeeds. To force live nil we need Client without
	// node OR Clientset empty and Client empty.
	// Seed checks.nodeCache so shouldSkip does not call Get; live re-check
	// always calls getNodeForResize which uses Clientset then Client.
	// Empty Clientset + empty Client → nil live.
	reconciler, _ := newResizeReconciler(pod, deploy)
	reconciler.Client = fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(deploy, pod).Build()
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy(policyName, policyNS)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation(appName,
			"500m", "512Mi", "1000m", "1Gi",
			"500m", "1Gi", "1000m", "2Gi"),
	}
	checks := &resizePreChecks{}
	checks.nodeCache.Store(nodeName, cachedNode)

	beforeUnavail := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "unavailable"))
	count, _ := reconciler.executeResizes(
		context.Background(),
		policy,
		[]client.Object{deploy},
		recommendations,
		podMap(appName, pod),
		nil,
		checks,
	)
	assert.Equal(t, 0, count, "live re-check with nil node must block increase")
	assert.Equal(t, beforeUnavail+1,
		testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues(policyNS, policyName, "unavailable")))

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "node status unavailable") {
				found = true
			}
		default:
			require.True(t, found)
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

func neighborTestNode(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func neighborPod(name, nodeName, cpu, mem string) *corev1.Pod {
	p := newResizePod(name, cpu, mem, cpu, mem)
	p.Name = name
	p.Spec.NodeName = nodeName
	return p
}

func TestExecuteResizes_NeighborBudget_SkipsIncrease(t *testing.T) {
	const nodeName = "shared-node"
	pod := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	pod.Spec.NodeName = nodeName
	neighbor := neighborPod("other", nodeName, "1000m", "1Gi")
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(pod, deploy, node, neighbor)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), neighbor.DeepCopy(), node.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("nb-skip", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	// This pod 1500m + neighbor 1000m = 2500m > 2000m allocatable.
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	before := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("default", "nb-skip", "neighbors"))
	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	assert.Equal(t, before+1, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("default", "nb-skip", "neighbors")))

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "neighbors") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped mentioning neighbors")
			return
		}
	}
}

func TestExecuteResizes_NeighborBudget_ZeroRequestNeighborAllowsIncrease(t *testing.T) {
	const nodeName = "shared-node-zero"
	pod := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	pod.Spec.NodeName = nodeName
	neighbor := neighborPod("silent", nodeName, "0", "0")
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(pod, deploy, node, neighbor)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), neighbor.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-zero", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", pod), nil, nil)
	assert.Equal(t, 1, count, "neighbor with no requests must not block")
}

func TestExecuteResizes_NeighborBudget_DecreaseStillApplies(t *testing.T) {
	const nodeName = "shared-node-dec"
	pod := newResizePod("app", "1500m", "512Mi", "2000m", "2Gi")
	pod.Spec.NodeName = nodeName
	neighbor := neighborPod("hog", nodeName, "1800m", "1Gi")
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(pod, deploy, node, neighbor)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy(), neighbor.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-dec", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "1500m", "512Mi", "2000m", "2Gi", "200m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", pod), nil, nil)
	assert.Equal(t, 1, count, "decreases stay allowed when neighbors fill the node")
}

func TestExecuteResizes_NeighborListError_SkipsIncrease(t *testing.T) {
	const nodeName = "shared-node-err"
	pod := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	pod.Spec.NodeName = nodeName
	node := neighborTestNode(nodeName, "8000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(pod, deploy, node)
	cs := kubefake.NewSimpleClientset(pod.DeepCopy(), node.DeepCopy())
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("list pods forbidden")
	})
	reconciler.Clientset = cs

	policy := newTestPolicy("nb-err", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	before := testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("default", "nb-err", "neighbors"))
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", pod), nil, nil)
	assert.Equal(t, 0, count, "increase must not apply when neighbor list fails")
	assert.Equal(t, before+1, testutil.ToFloat64(operatormetrics.CapacitySkipTotal.WithLabelValues("default", "nb-err", "neighbors")))
}

func TestNeighborRequestTotals_ExcludesTerminalPhase(t *testing.T) {
	const nodeName = "node-term"
	self := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	self.Spec.NodeName = nodeName
	self.UID = "self-uid"
	done := neighborPod("finished-job", nodeName, "1500m", "1Gi")
	done.UID = "job-uid"
	done.Status.Phase = corev1.PodSucceeded
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(self, deploy, node, done)
	reconciler.Clientset = kubefake.NewSimpleClientset(self.DeepCopy(), done.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-term", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", self), nil, nil)
	assert.Equal(t, 1, count, "Succeeded neighbor must not consume allocatable")
}

func TestNeighborRequestTotals_IncludesTerminating(t *testing.T) {
	const nodeName = "node-terming"
	self := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	self.Spec.NodeName = nodeName
	self.UID = "self-uid"
	leaving := neighborPod("leaving", nodeName, "1500m", "1Gi")
	leaving.UID = "leave-uid"
	now := metav1.Now()
	leaving.DeletionTimestamp = &now
	leaving.Finalizers = []string{"attune.io/test"}
	leaving.Status.Phase = corev1.PodRunning
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(self, deploy, node, leaving)
	reconciler.Clientset = kubefake.NewSimpleClientset(self.DeepCopy(), leaving.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-terming", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", self), nil, nil)
	assert.Equal(t, 0, count, "terminating neighbor still holds allocation")
}

func TestNeighborRequestTotals_IgnoresInformerOnlyNeighbor(t *testing.T) {
	const nodeName = "node-cache"
	self := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	self.Spec.NodeName = nodeName
	self.UID = "self-uid"
	cachedOnly := neighborPod("other-ns-pod", nodeName, "1500m", "1Gi")
	cachedOnly.Namespace = "other"
	cachedOnly.UID = "other-uid"
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	// Fat neighbor exists only in the informer client, not Clientset.
	reconciler, _ := newResizeReconciler(self, deploy, node, cachedOnly)
	reconciler.Clientset = kubefake.NewSimpleClientset(self.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-cache", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", self), nil, nil)
	assert.Equal(t, 1, count, "informer-only neighbor must not be counted (watch-namespaces fail-open guard)")
}

func TestNeighborRequestTotals_CountsOtherNamespaceOnClientset(t *testing.T) {
	const nodeName = "node-xns"
	self := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	self.Spec.NodeName = nodeName
	self.UID = "self-uid"
	otherNS := neighborPod("other-ns-pod", nodeName, "1500m", "1Gi")
	otherNS.Namespace = "other"
	otherNS.UID = "other-uid"
	node := neighborTestNode(nodeName, "2000m", "8Gi")
	deploy := newTestDeployment("app", "default", map[string]string{"app": "app"})

	reconciler, _ := newResizeReconciler(self, deploy, node)
	reconciler.Clientset = kubefake.NewSimpleClientset(self.DeepCopy(), otherNS.DeepCopy(), node.DeepCopy())

	policy := newTestPolicy("nb-xns", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app", "500m", "512Mi", "2000m", "2Gi", "1500m", "512Mi", "2000m", "2Gi"),
	}
	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recs, podMap("app", self), nil, nil)
	assert.Equal(t, 0, count, "Clientset neighbor in another namespace must still count")
}

func TestNeighborRequestTotals_SingleFlightOneList(t *testing.T) {
	const nodeName = "node-sf"
	self := newResizePod("app", "500m", "512Mi", "2000m", "2Gi")
	self.Spec.NodeName = nodeName
	self.UID = "self-uid"
	other := neighborPod("other", nodeName, "100m", "64Mi")
	other.UID = "other-uid"

	reconciler := NewAttunePolicyReconciler()
	cs := kubefake.NewSimpleClientset(self.DeepCopy(), other.DeepCopy())
	var lists atomic.Int32
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		time.Sleep(40 * time.Millisecond)
		return false, nil, nil
	})
	reconciler.Clientset = cs
	checks := &resizePreChecks{}

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, _, err := reconciler.neighborRequestTotals(context.Background(), self, checks)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), lists.Load(), "two workers on one node must share one List")
}
