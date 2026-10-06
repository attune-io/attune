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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestCheckPendingSafetyObservations_HeldOOMThenNotReadyKeepsFloor(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-notready", resizeTime, corev1.ConditionFalse)
	policy := policyWithHeldOOM(5 * time.Minute)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(10 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.False(t, pending, "notready revert finishes observation")
	assert.False(t, sloTrackingKept(t, fakeClient, "oom-notready"))
	assert.Equal(t, "notready", heldOOMHistoryReason(policy))
	cpu, mem, ok := lastResizeRequests(reconciler)
	require.True(t, ok, "notready revert calls UpdateResize")
	assert.True(t, cpu.Equal(resource.MustParse("500m")), "CPU reverts to 500m, got %s", cpu.String())
	assert.True(t, mem.Equal(resource.MustParse("768Mi")), "memory stays at the OOM floor 768Mi, got %s", mem.String())
}

func TestCheckPendingSafetyObservations_HeldOOMThenSLOBreachKeepsFloor(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-slo", resizeTime, corev1.ConditionTrue)
	window := 15 * time.Minute
	policy := policyWithSLOWindow(5*time.Minute, &window)
	policy.Spec.Memory.OOMBump = &attunev1alpha1.OOMBump{}
	policy.Status.ResizeHistory = heldOOMSuccessHistory()
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockCollector{queryFunc: func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0.99, nil
	}}
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(16 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending, "elapsed breach reverts and clears observation")
	assert.Equal(t, 1, calls)
	assert.False(t, sloTrackingKept(t, fakeClient, "oom-slo"))
	assert.Equal(t, "slo:latency", heldOOMHistoryReason(policy))
	_, mem, ok := lastResizeRequests(reconciler)
	require.True(t, ok)
	assert.True(t, mem.Equal(resource.MustParse("768Mi")), "memory stays at the OOM floor, got %s", mem.String())
}

func TestCheckPendingSafetyObservations_HeldOOMOpenSLOWindowStaysPending(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-slo-open", resizeTime, corev1.ConditionTrue)
	window := 15 * time.Minute
	policy := policyWithSLOWindow(5*time.Minute, &window)
	policy.Spec.Memory.OOMBump = &attunev1alpha1.OOMBump{}
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockCollector{queryFunc: func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0.99, nil
	}}
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(6 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.True(t, pending, "open SLO window keeps observation after a held OOM")
	assert.Equal(t, 0, calls, "the open window is not queried")
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.True(t, sloTrackingKept(t, fakeClient, "oom-slo-open"))
}

func TestCheckPendingSafetyObservations_HeldOOMThrottleGraceStaysPending(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-throttle", resizeTime, corev1.ConditionTrue)
	policy := policyWithHeldOOM(time.Minute)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(2 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockThrottleCollector{}, safetyWorkloads())
	assert.True(t, pending, "throttle grace keeps observation after the period elapsed")
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.True(t, sloTrackingKept(t, fakeClient, "oom-throttle"))
	assert.Empty(t, heldOOMHistoryReason(policy))
}

func TestCheckPendingSafetyObservations_HeldOOMReadyClears(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-clear", resizeTime, corev1.ConditionTrue)
	policy := policyWithHeldOOM(5 * time.Minute)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(10 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.False(t, pending, "a ready pod with nothing pending ends observation")
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.False(t, sloTrackingKept(t, fakeClient, "oom-clear"))
	assert.Empty(t, heldOOMHistoryReason(policy))
}

func TestCheckPendingSafetyObservations_OOMWithoutBumpStillReverts(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-plain", resizeTime, corev1.ConditionTrue)
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 5 * time.Minute}
	policy.Status.ResizeHistory = heldOOMSuccessHistory()
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(10 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.False(t, pending)
	assert.False(t, sloTrackingKept(t, fakeClient, "oom-plain"))
	assert.Equal(t, "oomkill", heldOOMHistoryReason(policy))
	_, mem, ok := lastResizeRequests(reconciler)
	require.True(t, ok)
	assert.True(t, mem.Equal(resource.MustParse("512Mi")), "without oomBump the original memory is restored, got %s", mem.String())
}

func TestCheckPendingSafetyObservations_HeldOOMDuringPeriodDoesNotCheckNotReady(t *testing.T) {
	resizeTime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pod := heldOOMObservationPod(t, "oom-early", resizeTime, corev1.ConditionFalse)
	policy := policyWithHeldOOM(5 * time.Minute)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(30 * time.Second) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, nil, safetyWorkloads())
	assert.True(t, pending, "the period has not elapsed")
	assert.Equal(t, 0, sloResizeUpdates(reconciler), "a held OOM during the period does not revert for NotReady")
	assert.True(t, sloTrackingKept(t, fakeClient, "oom-early"))
	assert.Empty(t, heldOOMHistoryReason(policy))
}

func heldOOMObservationPod(t *testing.T, name string, resizedAt time.Time, ready corev1.ConditionStatus) *corev1.Pod {
	t.Helper()
	oomAt := resizedAt.Add(time.Minute)
	pod := sloObservationPod(name, resizedAt, ready, []corev1.Container{
		sloResizedContainer("main", "250m", "768Mi"),
	}, nil)
	origin := resource.MustParse("512Mi")
	floor := resource.MustParse("768Mi")
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count:     1,
		Origin:    origin.Value(),
		Floor:     floor.Value(),
		OOMAt:     oomAt,
		Restart:   1,
		HoldUntil: resizedAt.Add(24 * time.Hour),
	})
	require.NoError(t, err)
	pod.Annotations["attune.io/oom-bump.main"] = raw
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "main",
		RestartCount: 1,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				FinishedAt: metav1.NewTime(oomAt),
			},
		},
	}}
	return pod
}

func policyWithHeldOOM(period time.Duration) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: period}
	policy.Spec.Memory.OOMBump = &attunev1alpha1.OOMBump{}
	policy.Status.ResizeHistory = heldOOMSuccessHistory()
	return policy
}

func heldOOMSuccessHistory() []attunev1alpha1.ResizeHistoryEntry {
	return []attunev1alpha1.ResizeHistoryEntry{{
		Workload:  "api-server",
		Container: "main",
		Resource:  "cpu",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}}
}

func heldOOMHistoryReason(policy *attunev1alpha1.AttunePolicy) string {
	if policy == nil || len(policy.Status.ResizeHistory) == 0 {
		return ""
	}
	return policy.Status.ResizeHistory[0].Reason
}

func lastResizeRequests(reconciler *AttunePolicyReconciler) (resource.Quantity, resource.Quantity, bool) {
	cs, ok := reconciler.Clientset.(*kubefake.Clientset)
	if !ok {
		return resource.Quantity{}, resource.Quantity{}, false
	}
	var cpu, mem resource.Quantity
	found := false
	for _, action := range cs.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		req := updated.Spec.Containers[0].Resources.Requests
		cpu = req[corev1.ResourceCPU]
		mem = req[corev1.ResourceMemory]
		found = true
	}
	return cpu, mem, found
}
