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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
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

func oneshotResizePod(name, cpuReq, memReq string) corev1.Pod {
	pod := makeCanaryPod(name, true, false)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
	}
	return pod
}

func TestFirstOneShotPodNeedingResize_SkipsAlreadyAtTarget(t *testing.T) {
	pods := []corev1.Pod{
		oneshotResizePod("pod-0", "200m", "256Mi"),
		oneshotResizePod("pod-1", "500m", "512Mi"),
		oneshotResizePod("pod-2", "500m", "512Mi"),
	}
	rec := newResizeRecommendation("api", "500m", "512Mi", "0", "0", "200m", "256Mi", "0", "0")

	// Unfixed OneShot is eligible[:1], so it keeps selecting the already-resized pod.
	selected := selectPodsForResize(pods, attunev1alpha1.UpdateTypeOneShot, 0)
	require.Len(t, selected, 1)
	assert.Equal(t, "pod-0", selected[0].Name)

	got := firstOneShotNeeding(pods, rec)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_AllAtTarget(t *testing.T) {
	pods := []corev1.Pod{
		oneshotResizePod("pod-0", "200m", "256Mi"),
		oneshotResizePod("pod-1", "200m", "256Mi"),
		oneshotResizePod("pod-2", "200m", "256Mi"),
	}
	rec := newResizeRecommendation("api", "500m", "512Mi", "0", "0", "200m", "256Mi", "0", "0")

	got := firstOneShotNeeding(pods, rec)
	assert.Empty(t, got)
}

func oneshotResizePodWithLimits(name, cpuReq, memReq, cpuLim, memLim string) corev1.Pod {
	pod := oneshotResizePod(name, cpuReq, memReq)
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpuLim),
		corev1.ResourceMemory: resource.MustParse(memLim),
	}
	return pod
}

func TestFirstOneShotPodNeedingResize_SkipsClampedMemoryLimit(t *testing.T) {
	// Guaranteed replica already at the post-raise applied state (512/512)
	// after a 1.33 clamp. Rec still wants 64/64. Without raising the
	// request to the clamped limit, compare sees live 512 vs target 64
	// and re-selects pod-0 forever. pod-1 still needs a CPU change.
	pod0 := oneshotResizePodWithLimits("pod-0", "200m", "512Mi", "200m", "512Mi")
	pod0.Status.QOSClass = corev1.PodQOSGuaranteed
	pod1 := oneshotResizePodWithLimits("pod-1", "500m", "512Mi", "500m", "512Mi")
	pod1.Status.QOSClass = corev1.PodQOSGuaranteed
	pods := []corev1.Pod{pod0, pod1}
	rec := newResizeRecommendation("api", "500m", "512Mi", "500m", "512Mi", "200m", "64Mi", "200m", "64Mi")

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = false
	got := r.firstOneShotPodNeedingResize(context.Background(), newTestPolicy("test-policy", "default"), pods, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_SkipsMemoryPressureIncrease(t *testing.T) {
	pod0 := oneshotResizePodWithLimits("pod-0", "500m", "512Mi", "500m", "1Gi")
	pod0.Spec.NodeName = "pressure-node"
	pod1 := oneshotResizePodWithLimits("pod-1", "500m", "512Mi", "500m", "1Gi")
	pod1.Spec.NodeName = "healthy-node"
	pressureNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pressure-node"},
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
	healthyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy-node"},
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
	// Memory request increase (512Mi -> 1Gi). MemoryPressure blocks pod-0.
	rec := newResizeRecommendation("api", "500m", "512Mi", "500m", "1Gi", "200m", "1Gi", "200m", "2Gi")

	r := newReconcilerWithClient(pressureNode, healthyNode)
	r.AllowInPlaceMemoryLimitDecrease = true
	got := r.firstOneShotPodNeedingResize(context.Background(), newTestPolicy("test-policy", "default"), []corev1.Pod{pod0, pod1}, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_SkipsQoSChange(t *testing.T) {
	// pod-0 is Guaranteed (500/500). Rec lowers memory request only, so
	// PreservesQoS is false and shouldSkipResize blocks it. pod-1 is
	// Burstable request-only and can take the same rec.
	pod0 := oneshotResizePodWithLimits("pod-0", "500m", "512Mi", "500m", "512Mi")
	pod0.Status.QOSClass = corev1.PodQOSGuaranteed
	pod1 := oneshotResizePod("pod-1", "500m", "512Mi")
	pod1.Status.QOSClass = corev1.PodQOSBurstable
	rec := newResizeRecommendation("api", "500m", "512Mi", "0", "0", "500m", "256Mi", "0", "0")

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = true
	got := r.firstOneShotPodNeedingResize(context.Background(), newTestPolicy("test-policy", "default"), []corev1.Pod{pod0, pod1}, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_SkipsInfeasibleInPlaceOnly(t *testing.T) {
	pod0 := oneshotResizePod("pod-0", "500m", "512Mi")
	pod0.Status.Conditions = append(pod0.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	pod1 := oneshotResizePod("pod-1", "500m", "512Mi")
	rec := newResizeRecommendation("api", "500m", "512Mi", "0", "0", "200m", "256Mi", "0", "0")

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOnly

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = true
	got := r.firstOneShotPodNeedingResize(context.Background(), policy, []corev1.Pod{pod0, pod1}, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_SkipsFlooredGuaranteed(t *testing.T) {
	// After the usage floor, a Guaranteed replica already at the floored
	// pair (550/550) must count as applied. Without raising the request
	// after the floor, compare sees live 550/550 vs target 200/550.
	pod0 := oneshotResizePodWithLimits("pod-0", "200m", "550Mi", "200m", "550Mi")
	pod0.Status.QOSClass = corev1.PodQOSGuaranteed
	pod1 := oneshotResizePodWithLimits("pod-1", "200m", "1Gi", "200m", "1Gi")
	pod1.Status.QOSClass = corev1.PodQOSGuaranteed
	pods := []corev1.Pod{pod0, pod1}

	rec := newResizeRecommendation("api", "200m", "1Gi", "200m", "1Gi", "200m", "200Mi", "200m", "200Mi")
	rec.Containers[0].Explanation = &attunev1alpha1.ContainerRecommendationExplanation{
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{
			RawPercentile: resource.MustParse("500Mi"),
		},
	}

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = true
	got := r.firstOneShotPodNeedingResize(context.Background(), newTestPolicy("test-policy", "default"), pods, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name)
}

func TestFirstOneShotPodNeedingResize_PrefetchQuotaFailClosedWalksPast(t *testing.T) {
	// pod-0 needs a request increase; pod-1 needs a decrease to the same rec.
	// checks.quotaListErr fail-closes increases only. If checks is still nil,
	// live quota list on an empty client succeeds and pod-0 is selected.
	pod0 := oneshotResizePod("pod-0", "200m", "256Mi")
	pod1 := oneshotResizePod("pod-1", "800m", "1Gi")
	rec := newResizeRecommendation("api", "200m", "256Mi", "0", "0", "500m", "512Mi", "0", "0")
	policy := newTestPolicy("test-policy", "default")

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = true

	got := r.firstOneShotPodNeedingResize(context.Background(), policy, []corev1.Pod{pod0, pod1}, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-0", got[0].Name, "nil checks uses live list success and must pick the first needing replica")

	checks := &resizePreChecks{quotaListErr: fmt.Errorf("list forbidden")}
	got = r.firstOneShotPodNeedingResize(context.Background(), policy, []corev1.Pod{pod0, pod1}, rec, checks)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name, "prefetch fail-closed must walk past the increase replica")
}

func TestFirstOneShotPodNeedingResize_UsesLivePodForInfeasible(t *testing.T) {
	listed0 := oneshotResizePod("pod-0", "500m", "512Mi")
	live0 := listed0.DeepCopy()
	live0.Status.Conditions = append(live0.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	pod1 := oneshotResizePod("pod-1", "500m", "512Mi")
	rec := newResizeRecommendation("api", "500m", "512Mi", "0", "0", "200m", "256Mi", "0", "0")

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOnly

	r := newReconcilerWithClient()
	r.AllowInPlaceMemoryLimitDecrease = true
	r.Clientset = kubefake.NewSimpleClientset(live0, pod1.DeepCopy())

	got := r.firstOneShotPodNeedingResize(context.Background(), policy, []corev1.Pod{listed0, pod1}, rec, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pod-1", got[0].Name, "live Infeasible on pod-0 must walk to pod-1")
}

// firstOneShotNeeding wraps firstOneShotPodNeedingResize for request-only tests.
func firstOneShotNeeding(pods []corev1.Pod, rec attunev1alpha1.WorkloadRecommendation) []corev1.Pod {
	r := newReconcilerWithClient()
	return r.firstOneShotPodNeedingResize(context.Background(), newTestPolicy("test-policy", "default"), pods, rec, nil)
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

func TestExecuteResizes_PromotedAppResizesAll_UnpromotedStaysCanary(t *testing.T) {
	makePods := func(app string, n int) []*corev1.Pod {
		out := make([]*corev1.Pod, n)
		for i := range n {
			p := newResizePod(app, "500m", "512Mi", "2000m", "2Gi")
			p.Name = fmt.Sprintf("%s-%d", app, i)
			out[i] = p
		}
		return out
	}
	aPods := makePods("app-a", 3)
	bPods := makePods("app-b", 3)
	deployA := newTestDeployment("app-a", "default", map[string]string{"app": "app-a"})
	deployB := newTestDeployment("app-b", "default", map[string]string{"app": "app-b"})

	extras := make([]client.Object, 0, 2+(len(aPods)-1)+len(bPods))
	extras = append(extras, deployA, deployB)
	csObjs := make([]runtime.Object, 0, 2+len(aPods)+len(bPods))
	csObjs = append(csObjs, deployA.DeepCopy(), deployB.DeepCopy())
	for _, p := range append(aPods, bPods...) {
		csObjs = append(csObjs, p.DeepCopy())
	}
	for _, p := range append(aPods[1:], bPods...) {
		extras = append(extras, p)
	}
	reconciler, _ := newResizeReconciler(aPods[0], extras...)
	reconciler.Clientset = kubefake.NewSimpleClientset(csObjs...)

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Spec.UpdateStrategy.Canary.Percentage = 10
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseFullRollout},
			{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"app-b-0"}},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("app-a", "500m", "512Mi", "2000m", "2Gi", "750m", "512Mi", "2000m", "2Gi"),
		newResizeRecommendation("app-b", "500m", "512Mi", "2000m", "2Gi", "750m", "512Mi", "2000m", "2Gi"),
	}
	podsBy := map[string][]corev1.Pod{
		"app-a": derefPods(aPods),
		"app-b": derefPods(bPods),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deployA, deployB}, recs, podsBy, nil, nil)
	assert.Equal(t, 2, count, "both apps have at least one resize")

	aOK, bOK := 0, 0
	for _, h := range history {
		if !isSuccessfulInPlaceHistory(h) {
			continue
		}
		switch h.Workload {
		case "app-a":
			aOK++
		case "app-b":
			bOK++
		}
	}
	// History writes one Success row per resource. 3 pods × cpu+memory = 6;
	// canary 10% of 3 pods is 1 pod × cpu+memory = 2.
	assert.Equal(t, 6, aOK, "promoted app must resize every eligible pod")
	assert.Equal(t, 2, bOK, "unpromoted app must stay on the canary percentage (min 1)")
}

func TestExecuteResizes_CanarySeedSkipsBatchAndStale(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	reconciler, _ := newResizeReconciler(pod, deploy, cj)

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
		newResizeRecommendation("nightly", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
		func() attunev1alpha1.WorkloadRecommendation {
			r := newResizeRecommendation("stale-app", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi")
			r.Stale = true
			return r
		}(),
	}

	_, _ = reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy, cj}, recs, podMap("api-server", pod), nil, nil)
	require.NotNil(t, policy.Status.Canary)
	assert.NotNil(t, policy.Status.Canary.WorkloadStatus("api-server"))
	assert.Nil(t, policy.Status.Canary.WorkloadStatus("nightly"), "batch workloads must not block FullRollout")
	assert.Nil(t, policy.Status.Canary.WorkloadStatus("stale-app"), "stale recs must not block FullRollout")
}

func TestExecuteResizes_CanaryPrunesLeftoverBatchRow(t *testing.T) {
	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)
	start := metav1.NewTime(now.Add(-15 * time.Minute))
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	reconciler, _ := newResizeReconciler(pod, deploy, cj)
	reconciler.SetNowFunc(func() time.Time { return now })

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase:     attunev1alpha1.CanaryPhaseInProgress,
		StartTime: &start,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "api-server", Phase: attunev1alpha1.CanaryPhaseInProgress, StartTime: &start, Pods: []string{pod.Name}},
			{Workload: "nightly", Phase: attunev1alpha1.CanaryPhaseInProgress},
			{Workload: "gone-app", Phase: attunev1alpha1.CanaryPhaseInProgress},
		},
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "api-server", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: start},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
		newResizeRecommendation("nightly", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	_, _ = reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy, cj}, recs, podMap("api-server", pod), nil, nil)
	require.NotNil(t, policy.Status.Canary)
	assert.Nil(t, policy.Status.Canary.WorkloadStatus("nightly"), "pre-fix Job/CronJob row must be dropped")
	assert.Nil(t, policy.Status.Canary.WorkloadStatus("gone-app"), "unmatched leftover row must be dropped")
	require.NotNil(t, policy.Status.Canary.WorkloadStatus("api-server"))
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.WorkloadStatus("api-server").Phase)
	assert.Equal(t, attunev1alpha1.CanaryPhaseFullRollout, policy.Status.Canary.Phase,
		"leftover batch row must not keep the policy in CanaryInProgress")
}

func TestExecuteResizes_CanaryPruneToEmptyDoesNotLegacyPromote(t *testing.T) {
	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.Add(-2 * time.Hour))
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	reconciler, _ := newResizeReconciler(pod, cj)
	reconciler.SetNowFunc(func() time.Time { return now })

	policy := canaryAutoPromotePolicy(10 * time.Minute)
	policy.Status.Canary = &attunev1alpha1.CanaryStatus{
		Phase: attunev1alpha1.CanaryPhaseInProgress,
		Workloads: []attunev1alpha1.CanaryWorkloadStatus{
			{Workload: "nightly", Phase: attunev1alpha1.CanaryPhaseInProgress},
		},
	}
	// Leftover Success from a Deployment that is no longer matched.
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{
		{Workload: "old-app", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: old},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("nightly", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	_, _ = reconciler.executeResizes(context.Background(), policy,
		[]client.Object{cj}, recs, map[string][]corev1.Pod{}, nil, nil)
	require.NotNil(t, policy.Status.Canary)
	assert.Empty(t, policy.Status.Canary.Workloads)
	assert.Equal(t, attunev1alpha1.CanaryPhaseInProgress, policy.Status.Canary.Phase,
		"empty table after prune must not FullRollout from leftover Success")
}

func derefPods(pods []*corev1.Pod) []corev1.Pod {
	out := make([]corev1.Pod, len(pods))
	for i, p := range pods {
		out[i] = *p
	}
	return out
}
