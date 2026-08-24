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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// makeCanaryPod creates a pod for canary selection tests. When running is true
// the pod phase is set to Running; when deleting is true a DeletionTimestamp
// is set so that resize.IsEligibleForResize returns false.
func makeCanaryPod(name string, running bool, deleting bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	if !running {
		pod.Status.Phase = corev1.PodPending
	}
	if deleting {
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
	}
	return pod
}

// makeRunningPods creates the requested number of running, non-deleting pods.
func makeRunningPods(count int) []corev1.Pod {
	pods := make([]corev1.Pod, count)
	for i := range pods {
		pods[i] = makeCanaryPod(fmt.Sprintf("pod-%d", i), true, false)
	}
	return pods
}

func TestSelectPodsForResize_OneShot_SelectsExactlyOne(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeOneShot, 0)
	assert.Len(t, selected, 1)
}

func TestSelectPodsForResize_Canary_10PercentOf20(t *testing.T) {
	pods := makeRunningPods(20)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeCanary, 10)
	assert.Len(t, selected, 2) // 10% of 20 = 2
}

func TestSelectPodsForResize_Canary_10PercentOf3_RoundsUp(t *testing.T) {
	pods := makeRunningPods(3)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeCanary, 10)
	assert.Len(t, selected, 1) // 10% of 3 = 0.3, rounds up to 1
}

func TestSelectPodsForResize_Canary_100Percent_SelectsAll(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeCanary, 100)
	assert.Len(t, selected, 5)
}

func TestSelectPodsForResize_Auto_SelectsAllEligible(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeAuto, 0)
	assert.Len(t, selected, 5)
}

func TestSelectPodsForResize_Observe_SelectsNone(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeObserve, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_Recommend_SelectsNone(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeRecommend, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_AllIneligible_ReturnsNil(t *testing.T) {
	pods := []corev1.Pod{
		makeCanaryPod("pod-0", true, true), // running but deleting
		makeCanaryPod("pod-1", true, true),
		makeCanaryPod("pod-2", true, true),
	}
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeAuto, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_MixedEligibility(t *testing.T) {
	pods := []corev1.Pod{
		makeCanaryPod("pod-0", true, false),  // eligible
		makeCanaryPod("pod-1", true, true),   // ineligible (deleting)
		makeCanaryPod("pod-2", true, false),  // eligible
		makeCanaryPod("pod-3", false, false), // ineligible (not running)
		makeCanaryPod("pod-4", true, false),  // eligible
	}
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeAuto, 0)
	assert.Len(t, selected, 3)
	for _, p := range selected {
		assert.Equal(t, corev1.PodRunning, p.Status.Phase)
		assert.Nil(t, p.DeletionTimestamp)
	}
}

func canaryAutoPromotePolicy(period time.Duration) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        20,
		ObservationPeriod: metav1.Duration{Duration: period},
		AutoPromote:       true,
	}
	return policy
}

func TestResolveCanaryPhase_DoesNotStartWatchWithoutInPlaceSuccess(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &metav1.Time{Time: now.Add(-10 * time.Minute)},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })

	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.NotEqual(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_DoesNotPromoteLateResizeWithoutWatchingIt(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	premature := metav1.NewTime(now.Add(-10 * time.Minute))
	lateSuccess := metav1.NewTime(now.Add(-30 * time.Second))
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &premature,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: lateSuccess},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })

	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "late first success must not promote on a premature clock")
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	require.NotNil(t, policy.Status.Canary.StartTime)
	assert.Equal(t, lateSuccess.Time, policy.Status.Canary.StartTime.Time, "watch must re-anchor to the successful resize")

	r.SetNowFunc(func() time.Time { return lateSuccess.Add(10 * time.Minute) })
	mode = r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode, "same resize aged a full period should promote")
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_ResetsWhenSuccessFlippedToReverted(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	resizeAt := metav1.NewTime(now.Add(-4 * time.Minute))
	watchStart := metav1.NewTime(now.Add(-4*time.Minute + 2*time.Second))
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &watchStart,
		Pods:      []string{"api-server-aaa"},
	}
	// Production safety revert flips the Success row in place and keeps
	// the original resize timestamp, which is before StartTime.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: attunev1alpha1.ResizeResultReverted, Timestamp: resizeAt},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })

	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.Nil(t, policy.Status.Canary.StartTime, "in-place Success flipped to Reverted must clear the clock")
	assert.Empty(t, policy.Status.Canary.Pods)
}

func TestResolveCanaryPhase_ResetsOnRevert(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	start := metav1.NewTime(now.Add(-4 * time.Minute))
	resizeAt := start.Add(1 * time.Minute)
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &start,
		Pods:      []string{"api-server-aaa"},
	}
	// Production flip-in-place: same timestamp as the original Success.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		flippedSuccessRevert("api-server", "InPlace", resizeAt, "oomkill"),
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })

	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.Nil(t, policy.Status.Canary.StartTime, "revert must start a new observation instead of freezing")
	assert.Empty(t, policy.Status.Canary.Pods)

	nextSuccess := metav1.NewTime(now.Add(1 * time.Minute))
	policy.Status.ResizeHistory = append(policy.Status.ResizeHistory, attunev1alpha1.ResizeHistoryEntry{
		Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: nextSuccess,
	})
	r.SetNowFunc(func() time.Time { return nextSuccess.Time })
	mode = r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	require.NotNil(t, policy.Status.Canary.StartTime)
	assert.Equal(t, nextSuccess.Time, policy.Status.Canary.StartTime.Time)

	r.SetNowFunc(func() time.Time { return nextSuccess.Add(10 * time.Minute) })
	mode = r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, mode)
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase)
}

func TestResolveCanaryPhase_PromotesOneAppOnly(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	aStart := metav1.NewTime(now.Add(-15 * time.Minute))
	bStart := metav1.NewTime(now.Add(-2 * time.Minute))
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &aStart,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseInProgress, StartTime: &aStart, Pods: []string{"a-1"}},
			{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseInProgress, StartTime: &bStart, Pods: []string{"b-1"}},
		},
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "app-a", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: aStart},
		{Workload: "app-b", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: bStart},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })
	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)

	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode, "fleet stays in canary while B is still watching")
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.WorkloadStatus("app-a").Phase)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.WorkloadStatus("app-b").Phase)
	assert.True(t, policy.Status.Canary.AllowsHPARetune("app-a"))
	assert.False(t, policy.Status.Canary.AllowsHPARetune("app-b"))
}

func TestResolveCanaryPhase_DoesNotPromoteFromLeftoverHistory(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.Add(-2 * time.Hour))
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseInProgress},
		},
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "app-b", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: old},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })
	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.WorkloadStatus("app-b").Phase,
		"leftover Success from before this canary watch must not promote")
}

func TestResolveCanaryPhase_LaterRevertedRowIsNonProductionShape(t *testing.T) {
	// Non-production fixture: a later Reverted row after Success.
	// Production flips Success in place (see ResetsWhenSuccessFlippedToReverted).
	// Kept so a naive "look for a later Reverted row" check is not the only coverage.
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	start := metav1.NewTime(now.Add(-4 * time.Minute))
	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &start,
		Pods:      []string{"api-server-aaa"},
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(start.Add(1 * time.Minute))},
		{Result: attunev1alpha1.ResizeResultReverted, Timestamp: metav1.NewTime(start.Add(2 * time.Minute))},
	}

	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })

	mode := r.resolveCanaryPhase(context.Background(), policy, attunev1alpha1.UpdateTypeCanary)
	assert.Equal(t, attunev1alpha1.UpdateTypeCanary, mode)
	assert.Nil(t, policy.Status.Canary.StartTime, "even a later Reverted row must reset the clock")
}

func TestExecuteResizes_CanaryAutoPromoteStartsWatchAfterInPlaceResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)
	require.NotEmpty(t, history)
	require.NotNil(t, policy.Status.Canary)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase)
	require.NotNil(t, policy.Status.Canary.StartTime, "watch starts after a successful in-place resize")
	assert.Contains(t, policy.Status.Canary.Pods, pod.Name)
}

func TestExecuteResizes_CanaryDoesNotStartWatchWhenNoResize(t *testing.T) {
	pod := newResizePod("api-server", "750m", "384Mi", "1500m", "768Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "0", "0", "750m", "384Mi", "1500m", "768Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	if policy.Status.Canary != nil {
		assert.Nil(t, policy.Status.Canary.StartTime, "skipped resize must not start the observation clock")
	}
}
