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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/safety"
)

func TestResizeContainer_LiveAppliedClampNoopEmitsResizeUnchanged(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	r, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	r.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	target := pod.Spec.Containers[0].Resources.DeepCopy()
	pre := target.DeepCopy()
	pre.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	pre.Limits[corev1.ResourceMemory] = resource.MustParse("64Mi")

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: attunev1alpha1.ContainerRecommendation{Name: "main"},
		Target:       *target,
		LiveApplied:  true,
		ApplyMeta:    liveResizeApplyMeta{PreClamped: *pre},
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeNone, outcome)
	assert.Empty(t, entries)

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeUnchanged") {
				found = true
			}
		default:
			require.True(t, found, "LiveApplied clamp-to-current must emit ResizeUnchanged")
			return
		}
	}
}

func TestResizeContainer_InfeasiblePodEvictedDirectly(t *testing.T) {
	// A pod marked Infeasible with InPlaceOrRecreate should go directly to
	// eviction without attempting another in-place resize.
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Status.Conditions = append(pod1.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	// Second pod so eviction is not blocked by last-replica protection.
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod1,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeEvicted, outcome, "infeasible pod should be evicted")
	require.Len(t, entries, 1)
	assert.Equal(t, "Eviction", entries[0].Method)
	assert.Equal(t, attunev1alpha1.ResizeResultEvicted, entries[0].Result)

	// Verify an eviction was actually issued, not a resize attempt.
	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
	}
	assert.Equal(t, 1, evictions, "should have issued exactly one eviction")

	// Verify NO resize was attempted (the pod was Infeasible, so we skip UpdateResize).
	var resizes int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "pods" && a.GetSubresource() == "resize" {
			resizes++
		}
	}
	assert.Equal(t, 0, resizes, "should NOT have attempted in-place resize on Infeasible pod")
}

func TestResizeContainer_InfeasibleLiveRecheckAfterStaleCache(t *testing.T) {
	// Listed/informer pod has no Infeasible; Clientset does. Evict.
	listed := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	listed.Name = "api-server-abc-1"
	live := listed.DeepCopy()
	live.Status.Conditions = append(live.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	peer := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	peer.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, listed, peer).Build()
	clientset := kubefake.NewSimpleClientset(live, peer.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          listed,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeEvicted, outcome, "live Infeasible must evict even if listed pod is clear")
	require.Len(t, entries, 1)
	assert.Equal(t, "Eviction", entries[0].Method)
	assert.Equal(t, attunev1alpha1.ResizeResultEvicted, entries[0].Result)

	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
	}
	assert.Equal(t, 1, evictions)
}

func TestResizeContainer_StaleInfeasibleClearedOnLiveGet(t *testing.T) {
	// Listed/informer pod is still Infeasible; live Get has cleared it.
	// Inverse of InfeasibleLiveRecheckAfterStaleCache: stay in-place.
	listed := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	listed.Name = "api-server-abc-1"
	listed.Status.Conditions = append(listed.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	live := listed.DeepCopy()
	live.Status.Conditions = nil
	peer := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	peer.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, listed, peer).Build()
	clientset := kubefake.NewSimpleClientset(live, peer.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target, _ := buildResizeTarget(containerRec)

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          listed,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Target:       target,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeInPlace, outcome, "cleared live Infeasible must stay in-place")
	require.NotEmpty(t, entries)
	assert.NotEqual(t, resizeOutcomeEvicted, outcome)
	for _, e := range entries {
		assert.NotEqual(t, "Eviction", e.Method)
		assert.NotEqual(t, attunev1alpha1.ResizeResultEvicted, e.Result)
	}

	var evictions, resizes int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizes++
		}
	}
	assert.Equal(t, 0, evictions, "cleared live Infeasible must not evict")
	assert.Greater(t, resizes, 0, "cleared live Infeasible must attempt in-place resize")
}

func TestResizeContainer_InfeasiblePodSkippedWithInPlaceOnly(t *testing.T) {
	// An Infeasible pod with InPlaceOnly should be skipped entirely
	// (no resize attempt, no eviction).
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod.Name = "api-server-abc-1"
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	// InPlaceOnly (default): no eviction allowed.

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeNone, outcome, "infeasible pod with InPlaceOnly should not be resized")
	require.Len(t, entries, 1, "should record a Failed history entry with reason infeasible")
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, entries[0].Result)
	assert.Equal(t, "infeasible", entries[0].Reason)
	assert.Equal(t, "api-server", entries[0].Workload)

	// Verify InfeasibleBlocked event was emitted.
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "InfeasibleBlocked")
		assert.Contains(t, event, "api-server-abc-1")
		assert.Contains(t, event, "InPlaceOrRecreate")
	default:
		t.Error("expected InfeasibleBlocked event but none was emitted")
	}

	// Verify NO resize and NO eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Error("should NOT have attempted in-place resize on Infeasible pod")
		}
		if a.GetVerb() == "create" && a.GetSubresource() == "eviction" {
			t.Error("should NOT have attempted eviction with InPlaceOnly")
		}
	}
}

func TestResizeContainer_InfeasibleLastReplicaRecordsReason(t *testing.T) {
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod.Name = "api-server-abc-1"
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("512Mi"),
		},
	}

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Resizer:      resizer,
		Monitor:      nil,
		Now:          metav1.Now(),
	})
	assert.Equal(t, resizeOutcomeEvictionBlocked, outcome, "last live Running replica must not be evicted")
	require.Len(t, entries, 1, "should record a Failed history entry")
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, entries[0].Result)
	assert.Equal(t, reasonEvictionLastReplica, entries[0].Reason)
	assert.NotEqual(t, "infeasible", entries[0].Reason)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EvictionBlocked")
		assert.Contains(t, event, "live Running")
	default:
		t.Error("expected EvictionBlocked event but none was emitted")
	}

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetSubresource() == "eviction" {
			t.Error("should NOT have attempted eviction of the last live Running replica")
		}
	}
}

func TestResizeContainer_FailedRevertStaysInPlace(t *testing.T) {
	// Annotation persist fails after UpdateResize, then RevertPod fails.
	// The higher requests stay on the pod, so the outcome must be in-place.
	// resizeOutcomeNone would refund the cycle budget.
	pod := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	resizeCalls := 0
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(k8stesting.UpdateAction)
		if !ok || updateAction.GetSubresource() != "resize" {
			return false, nil, nil
		}
		resizeCalls++
		if resizeCalls >= 2 {
			return true, nil, fmt.Errorf("simulated revert failure")
		}
		return false, nil, nil
	})

	r := NewAttunePolicyReconciler()
	r.Client = &failOnPodUpdateClient{Client: fakeClient}
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	target, _ := buildResizeTarget(containerRec)

	entries, outcome := r.resizeContainer(context.Background(), resizeParams{
		Policy:       policy,
		Pod:          pod,
		Workload:     deploy,
		WorkloadName: "api-server",
		ContainerRec: containerRec,
		Target:       target,
		Resizer:      resize.NewPodResizer(clientset, ctrl.Log),
		Monitor:      safety.NewMonitor(clientset, ctrl.Log),
		Now:          metav1.Now(),
		LiveApplied:  true,
	})
	assert.Equal(t, resizeOutcomeInPlace, outcome, "failed revert must keep the increase and not refund budget")
	assert.GreaterOrEqual(t, resizeCalls, 2, "UpdateResize must apply, then RevertPod must fail")
	require.NotEmpty(t, entries)
	for _, e := range entries {
		assert.Equal(t, attunev1alpha1.ResizeResultFailed, e.Result)
	}
}
