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
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
)

// ---------- executeResizes ----------

func TestExecuteResizes_NoClientset(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	policy := newTestPolicy("test-policy", "default")

	count, history := reconciler.executeResizes(context.Background(), policy, nil, nil, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Nil(t, history)
}

func TestExecuteResizes_SuccessfulResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)
	require.Len(t, history, 2, "expect one cpu + one memory history entry")
	assert.Equal(t, "api-server", history[0].Workload)
	assert.Equal(t, "main", history[0].Container)
	assert.Equal(t, "InPlace", history[0].Method)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result, "cpu resize should succeed")
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[1].Result, "memory resize should succeed")
}

func TestExecuteResizes_OneShot_WalksPastBlockedFirstReplica(t *testing.T) {
	// Replica 0 is already at the applied target. Replica 1 still needs
	// the resize. OneShot must walk past pod-0; eligible[:1] would pick
	// it and no-op.
	pod0 := newResizePod("api-server", "200m", "256Mi", "400m", "512Mi")
	pod0.Name = "pod-0"
	pod1 := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	pod1.Name = "pod-1"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod0, pod1).Build()
	clientset := kubefake.NewSimpleClientset(pod0.DeepCopy(), pod1.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "200m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod0, *pod1}}, nil, nil)
	assert.Equal(t, 1, count)
	require.NotEmpty(t, history)

	var resized []string
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			resized = append(resized, updated.Name)
		}
	}
	require.NotEmpty(t, resized, "UpdateResize must run on the still-needing replica")
	for _, name := range resized {
		assert.Equal(t, "pod-1", name)
	}
}

func TestExecuteResizes_ContextCancelledAbortsRemaining(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, history := reconciler.executeResizes(ctx, policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "no resizes should complete with cancelled context")
	assert.Empty(t, history, "no history entries with cancelled context")
}

func TestExecuteResizes_SkipsMatchingResources(t *testing.T) {
	// Pod already at the recommended values.
	pod := newResizePod("api-server", "750m", "384Mi", "1500m", "768Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "0", "0", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_NoMatchingWorkload(t *testing.T) {
	deploy := newTestDeployment("other-app", "default", nil)
	reconciler := newReconcilerWithClient(deploy)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "api-server", Kind: "Deployment"},
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_SkipsStaleRecommendation(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", nil)
	reconciler := newReconcilerWithClient(deploy)
	reconciler.Clientset = kubefake.NewSimpleClientset()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "api-server", Kind: "Deployment", Stale: true},
	}

	before := promtestutil.ToFloat64(operatormetrics.StaleRecommendationsTotal.WithLabelValues("default", "test-policy"))
	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, nil, nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
	after := promtestutil.ToFloat64(operatormetrics.StaleRecommendationsTotal.WithLabelValues("default", "test-policy"))
	assert.Equal(t, before+1, after, "StaleRecommendationsTotal should increment with policy labels")
}

func TestExecuteResizes_SkipsQoSChange(t *testing.T) {
	// Pod is Guaranteed class (requests == limits).
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_GuaranteedQoS_MemoryClampAllowsCPUResize(t *testing.T) {
	// Regression test for the E2E failure TestE2E_GuaranteedQoS_RequestsAndLimits.
	// Guaranteed QoS pod (requests == limits). The recommendation decreases both
	// CPU and memory. The memory limit clamp preserves the current memory limit
	// (NotRequired resize policy forbids in-place memory limit decreases). Before
	// the fix, the clamped memory limit (256Mi) != recommended memory request (64Mi)
	// caused PreservesQoS to block the entire resize, including CPU.
	// After the fix, the memory request is raised to match the clamped limit,
	// preserving Guaranteed QoS and allowing CPU to resize.
	pod := newResizePod("qos-app", "500m", "256Mi", "500m", "256Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	// No resizePolicy set → defaults to NotRequired for memory.
	deploy := newTestDeployment("qos-app", "default", map[string]string{"app": "qos-app"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv

	// Recommend CPU decrease (500m → 50m) and memory decrease (256Mi → 64Mi).
	// Both limits also decrease (ControlledValues: RequestsAndLimits behavior).
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("qos-app", "500m", "256Mi", "500m", "256Mi", "50m", "64Mi", "50m", "64Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("qos-app", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should succeed: CPU changes even though memory is clamped")
	assert.NotEmpty(t, history, "should have resize history entries")
}

func TestExecuteResizes_QoSBlocked_EmitsResizeSkippedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "500m", "512Mi", "250m", "256Mi", "500m", "512Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)

	var got []string
	for {
		select {
		case event := <-recorder.Events:
			got = append(got, event)
		default:
			require.Len(t, got, 1, "QoS skip must emit exactly one user-visible event")
			assert.Contains(t, got[0], "ResizeSkipped")
			assert.Contains(t, got[0], "controlledValues")
			assert.Contains(t, got[0], "would change QoS class")
			assert.NotContains(t, got[0], "Skipping resize")
			return
		}
	}
}

func TestExecuteResizes_ResizeError(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	// Inject an error on UpdateResize calls.
	reconciler.Clientset.(*kubefake.Clientset).PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("node has insufficient resources")
		}
		return false, nil, nil
	})

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, history[0].Result)
}

func TestExecuteResizes_ResizeError_EmitsResizeFailedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	reconciler.Clientset.(*kubefake.Clientset).PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("node has insufficient resources")
		}
		return false, nil, nil
	})

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)

	// Drain all events and check for at least one ResizeFailed event.
	foundFailed := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeFailed") {
				assert.Contains(t, event, "api-server")
				foundFailed = true
			}
		default:
			goto doneFailed
		}
	}
doneFailed:
	assert.True(t, foundFailed, "expected at least one ResizeFailed event")
}

func TestExecuteResizes_AutoRevert_SafeVerdictNoRevert(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// Resize was attempted. The safety check runs immediately but with a
	// fake clientset the pod won't have conditions set, so CheckPod will
	// return Safe (no restart detected). This exercises the autoRevert
	// code path even though it does not trigger a revert.
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result)
}

// ---------- node capacity pre-check ----------

func TestExecuteResizes_SkipsWhenExceedsNodeCapacity(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("600m"), // less than recommended 750m
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy, node)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be skipped when total requests exceed node allocatable")
	assert.Empty(t, history)
}

func TestExecuteResizes_ProceedsWhenWithinNodeCapacity(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	pod.Spec.NodeName = "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy, node)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should proceed when within node capacity")
}

// ---------- Event emission ----------

func TestExecuteResizes_EmitsResizedEvent(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)

	// Drain all events and check for at least one Resized event.
	foundResized := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Resized") {
				foundResized = true
			}
		default:
			goto done
		}
	}
done:
	assert.True(t, foundResized, "expected at least one Resized event")
}

func TestExecuteResizes_ThrottleNotRevertedDuringGracePeriod(t *testing.T) {
	// The immediate post-resize safety check should NOT revert for throttle
	// because the Prometheus rate(…[5m]) window still contains 100% pre-resize
	// data. Throttle reverts should only happen during deferred observations
	// (>5 minutes after resize). See safety/monitor.go throttleGrace.
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "500m", "256Mi", "250m", "128Mi", "250m", "128Mi"),
	}
	workloads := []client.Object{deploy}

	// Collector reports 60% throttle (above 50% threshold).
	// Despite this, the immediate check should skip the throttle evaluation
	// because the resize just happened (within the 5-minute grace period).
	collector := &mockThrottleCollector{throttleRatio: 0.6}

	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), collector, nil)
	// Resize should succeed and NOT be immediately reverted.
	assert.Equal(t, 1, count, "resize should succeed without immediate throttle revert")

	// History should show success, not revert.
	for _, h := range history {
		assert.NotEqual(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"should not have a throttle revert within the grace period")
	}
}

func TestExecuteResizes_PersistsAnnotations(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 7)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count, "resize should succeed")
	require.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultSuccess, history[0].Result)

	// Verify annotations were persisted on the pod.
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	assert.NotEmpty(t, updated.Annotations[annotationResizedAt], "resized-at annotation should be set")
	_, parseErr := time.Parse(time.RFC3339, updated.Annotations[annotationResizedAt])
	assert.NoError(t, parseErr, "resized-at should be valid RFC3339")

	assert.Contains(t, updated.Annotations[annotationResizedContainers], "main")
	assert.Equal(t, "api-server", updated.Annotations[annotationResizedWorkload])
	assert.Equal(t, "500m", updated.Annotations[annotationOriginalCPUPrefix+"main"])
	assert.Equal(t, "512Mi", updated.Annotations[annotationOriginalMemoryPrefix+"main"])
	assert.Equal(t, "7", updated.Annotations[annotationOriginalRestartCountPrefix+"main"],
		"RestartCount should be captured from pre-resize container status")
}

func TestExecuteResizes_CapturesZeroRestartCount(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count)

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "0", updated.Annotations[annotationOriginalRestartCountPrefix+"main"],
		"zero RestartCount should still be persisted")
}

func TestExecuteResizes_PreservesExistingPodAnnotations(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	pod.Annotations = map[string]string{"existing-key": "existing-value"}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, fakeClient := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, _ := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count)

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "existing-value", updated.Annotations["existing-key"],
		"pre-existing annotations must not be lost")
	assert.NotEmpty(t, updated.Annotations[annotationResizedAt],
		"resize annotations must be added alongside existing ones")
}

// ---------- revert on annotation persistence failure (#35) ----------

func TestExecuteResizes_RevertsOnAnnotationUpdateFailure(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Wrap the controller-runtime fake client to fail on the pod Update call
	// that persists annotations, while letting all other operations succeed.
	wrappedClient := &failOnPodUpdateClient{Client: fakeClient}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// The resize should have been reverted because annotation update failed.
	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	// History should show Reverted entries.
	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a Reverted entry")

	// Verify that a Reverted event was emitted.
	foundRevert := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "annotation-persist-failed") {
				foundRevert = true
			}
		default:
			goto checkRevert
		}
	}
checkRevert:
	assert.True(t, foundRevert, "expected a Reverted event mentioning annotation-persist-failed")

	// Verify the revert was issued via UpdateResize (second call: first is
	// the original resize, second is the revert).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original + revert")
}

// failOnPodUpdateClient wraps a client.Client and fails on Update calls for Pods.
// The first Get after a resize (re-fetch) succeeds, but the Update for
// annotation persistence fails. This simulates a 409 Conflict or similar error.
type failOnPodUpdateClient struct {
	client.Client
}

func TestExecuteResizes_TimeoutAfterCommitDoesNotRevert(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod.DeepCopy()).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	wrapped := &commitThenTimeoutPodClient{Client: fakeClient, cs: clientset, timeoutsLeft: 1}

	r := NewAttunePolicyReconciler()
	r.Client = wrapped
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = events.NewFakeRecorder(10)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	count, history := r.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	require.Equal(t, 1, count, "resize must stick when persist committed before the timeout")
	require.NotEmpty(t, history)
	for _, h := range history {
		assert.NotEqual(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"must not revert a resize whose tracking annotations already landed")
	}

	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 1, resizeCalls, "only the original UpdateResize; no revert")
	assert.Equal(t, 1, wrapped.timeoutsSeen)
}

func TestExecuteResizes_AnnotationConflictRetrySucceeds(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 1}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// Despite a conflict on the first annotation update attempt, the retry
	// should succeed and the resize should NOT be reverted.
	assert.Equal(t, 1, count, "resize should succeed after conflict retry")
	require.NotEmpty(t, history)
	for _, h := range history {
		assert.NotEqual(t, attunev1alpha1.ResizeResultReverted, h.Result,
			"history should not contain Reverted entries after successful retry")
	}
	assert.Equal(t, 1, wrappedClient.conflictsSeen, "should have seen exactly 1 conflict")
}

func TestExecuteResizes_RevertFailureMarksHistoryAsFailed(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Wrap the controller-runtime fake client to fail on Pod Update
	// (annotation persistence), which triggers the revert path.
	wrappedClient := &failOnPodUpdateClient{Client: fakeClient}

	// Also make the revert's UpdateResize fail. The first UpdateResize call
	// (the original resize) should succeed, but the second one (the revert)
	// should fail.
	resizeCallCount := 0
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(k8stesting.UpdateAction)
		if !ok || updateAction.GetSubresource() != "resize" {
			return false, nil, nil
		}
		resizeCallCount++
		if resizeCallCount >= 2 {
			// Fail the revert resize call.
			return true, nil, fmt.Errorf("simulated revert failure")
		}
		return false, nil, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	_, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// History should show Failed (not Reverted or Success) because both
	// annotation persist and revert failed.
	require.NotEmpty(t, history)
	for _, h := range history {
		if h.Workload == "api-server" {
			assert.Equal(t, attunev1alpha1.ResizeResultFailed, h.Result,
				"history should be Failed when revert also fails, got %s", h.Result)
		}
	}
}

func TestExecuteResizes_RevertsOnReFetchFailure(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	allObjects := []client.Object{deploy, pod}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Inject failure on typed clientset Get for pods. OneShot selection
	// live-gets before Infeasible (call 1), resizeContainer live-gets
	// again (call 2), ResizePod does a pre-resize re-fetch (call 3),
	// then persistResizeAnnotations does a post-resize re-fetch (call 4).
	// Fail call 4 to test annotation-persist revert. Subsequent Gets
	// (revert's pod lookup) pass through.
	getCount := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getCount++
		if getCount == 4 {
			return true, nil, fmt.Errorf("simulated re-fetch failure")
		}
		return false, nil, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	assert.Equal(t, 0, count, "net resized count should be 0 after revert")

	require.NotEmpty(t, history)
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a Reverted entry for re-fetch failure")

	// Verify revert event.
	foundRevert := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "re-fetch-failed") {
				foundRevert = true
			}
		default:
			goto checkReFetch
		}
	}
checkReFetch:
	assert.True(t, foundRevert, "expected a Reverted event mentioning re-fetch-failed")

	// Verify revert UpdateResize was called.
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original + revert")
}

func TestExecuteResizes_RequestClampedMetric(t *testing.T) {
	pod := newResizePod("api-server", "600m", "1Gi", "500m", "512Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	clientset := kubefake.NewSimpleClientset(pod)

	reconciler := newReconcilerWithClient(pod, deploy)
	reconciler.Clientset = clientset

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("800m"), // Will be clamped to 500m limit
						MemoryRequest: resource.MustParse("2Gi"),  // Will be clamped to 512Mi limit
						CPULimit:      resource.MustParse("500m"),
						MemoryLimit:   resource.MustParse("512Mi"),
					},
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("600m"),
						MemoryRequest: resource.MustParse("1Gi"),
						CPULimit:      resource.MustParse("500m"),
						MemoryLimit:   resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	beforeCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	beforeMem := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "memory"))

	reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)

	afterCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	afterMem := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "memory"))
	assert.Equal(t, beforeCPU+1, afterCPU, "RequestClampedTotal should increment for CPU")
	assert.Equal(t, beforeMem+1, afterMem, "RequestClampedTotal should increment for memory")
}

func TestExecuteResizes_DestClampAtTargetDoesNotIncrement(t *testing.T) {
	// Rec 500m, leftover live limit 200m, live already at 200m. Plan dest-clamps
	// every cycle; the counter must not tick after converge.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	reconciler, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("200m"),
						MemoryRequest: resource.MustParse("256Mi"),
						CPULimit:      resource.MustParse("200m"),
						MemoryLimit:   resource.MustParse("256Mi"),
					},
				},
			},
		},
	}

	beforeCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	afterCPU := promtestutil.ToFloat64(operatormetrics.RequestClampedTotal.WithLabelValues("default", "test-policy", "main", "cpu"))
	assert.Equal(t, beforeCPU, afterCPU, "AtTarget dest clamp must not increment RequestClampedTotal")

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeUnchanged") {
				found = true
			}
		default:
			require.True(t, found, "AtTarget dest CPU clamp must emit ResizeUnchanged")
			return
		}
	}
}

func TestExecuteResizes_FloorToCurrentEmitsResizeUnchanged(t *testing.T) {
	pod := newResizePod("api-server", "200m", "550Mi", "200m", "550Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	r, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	r.Recorder = recorder
	r.AllowInPlaceMemoryLimitDecrease = true

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	allowDec := true
	policy.Spec.Memory.AllowDecrease = &allowDec
	margin := int32(10)
	policy.Spec.Memory.DecreaseUsageMarginPercent = &margin

	rec := newResizeRecommendation("api-server",
		"200m", "550Mi", "200m", "550Mi",
		"200m", "200Mi", "200m", "200Mi")
	rec.Containers[0].Explanation = &attunev1alpha1.ContainerRecommendationExplanation{
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{
			RawPercentile: resource.MustParse("500Mi"),
		},
	}

	count, _ := r.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{rec}, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "floor-to-current must not apply a resize")

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeUnchanged") {
				found = true
			}
		default:
			require.True(t, found, "executeResizes clamp/floor no-op must emit ResizeUnchanged")
			return
		}
	}
}

func TestExecuteResizes_RateCapDefersUntilRefill(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	reconciler.SetNowFunc(func() time.Time { return now })

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	rate := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxCPUIncreasePerMinute = &rate
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recs, podMap("api-server", pod1, pod2), nil, nil)
	assert.Equal(t, 1, count, "300m/min allows one 300m increase")

	count, _ = reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recs, podMap("api-server", pod2), nil, nil)
	assert.Equal(t, 0, count, "same minute must not allow a second 300m increase")

	now = now.Add(time.Minute)
	count, _ = reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recs, podMap("api-server", pod2), nil, nil)
	assert.Equal(t, 1, count, "after a minute the rate bucket refills")
}

func TestExecuteResizes_BudgetCapsDefersExcessiveIncrease(t *testing.T) {
	// Pod at 200m CPU, recommendation is 800m (increase of 600m).
	// Budget cap is 500m, so the resize should be skipped.
	pod := newResizePod("api-server", "200m", "256Mi", "800m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "800m", "256Mi", "0", "0"),
	}

	beforeBudget := promtestutil.ToFloat64(operatormetrics.BudgetExhaustedTotal.WithLabelValues("default", "test-policy"))
	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be deferred when CPU increase exceeds budget")
	assert.Empty(t, history, "budget gate must not record Success history")
	afterBudget := promtestutil.ToFloat64(operatormetrics.BudgetExhaustedTotal.WithLabelValues("default", "test-policy"))
	assert.Equal(t, beforeBudget+1, afterBudget, "BudgetExhaustedTotal should increment")

	// status.workloads.resized is set only from executeResizes count (and Success
	// history self-heal). Both must stay zero when the gate fires.
	policy.Status.Workloads.Resized = safeInt32(count)
	if policy.Status.Workloads.Resized == 0 {
		resizedWorkloads := make(map[string]bool)
		for _, h := range history {
			if isSuccessfulInPlaceHistory(h) {
				resizedWorkloads[h.Workload] = true
			}
		}
		if derived := safeInt32(len(resizedWorkloads)); derived > policy.Status.Workloads.Resized {
			policy.Status.Workloads.Resized = derived
		}
	}
	assert.Equal(t, int32(0), policy.Status.Workloads.Resized,
		"workloads.resized must stay 0 when budget defers the only increase")

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "BudgetExhausted") {
				found = true
			}
		default:
			goto doneEvents
		}
	}
doneEvents:
	assert.True(t, found, "BudgetExhausted event must fire when increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsAllowsWithinBudget(t *testing.T) {
	// Pod at 200m CPU, recommendation is 500m (increase of 300m).
	// Budget cap is 500m, so the resize should proceed.
	pod := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "resize should proceed when within budget")
}

func TestExecuteResizes_BudgetCapsDecreasesFree(t *testing.T) {
	// Pod at 800m CPU, recommendation is 400m (decrease of 400m).
	// Budget cap is 100m. Decreases should NOT consume budget.
	pod := newResizePod("api-server", "800m", "256Mi", "800m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("100m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "800m", "256Mi", "0", "0", "400m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "decreases should not consume budget")
}

func TestExecuteResizes_BudgetCapsMemory(t *testing.T) {
	// Pod at 256Mi memory, recommendation is 1Gi (increase of 768Mi).
	// Memory budget is 512Mi, so the resize should be deferred.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	memBudget := resource.MustParse("512Mi")
	policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = &memBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "200m", "1Gi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "resize should be deferred when memory increase exceeds budget")
}

func TestExecuteResizes_BudgetCapsExactlyEqualsPasses(t *testing.T) {
	// Increase of exactly 500m with budget of 500m should pass (not strict >).
	pod := newResizePod("api-server", "200m", "256Mi", "700m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("500m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "700m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "increase exactly equal to budget should proceed")
}

func TestExecuteResizes_BudgetCapsClampedTargetUsesAppliedIncrease(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "600m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("100m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "600m", "256Mi", "800m", "256Mi", "600m", "256Mi"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "budget should use the clamped applied increase, not the raw recommendation delta")
}

func TestExecuteResizes_BudgetUsesGuaranteedRaisedTarget(t *testing.T) {
	// Guaranteed 256Mi/256Mi. Rec is 300Mi request / 400Mi limit.
	// applyLiveResizeTarget raises the request to 400Mi. Budget 64Mi sits
	// between the raw request delta (44Mi) and the applied delta (144Mi).
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	memBudget := resource.MustParse("64Mi")
	policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = &memBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "200m", "256Mi", "200m", "300Mi", "200m", "400Mi"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count, "budget must see the Guaranteed-raised request, not the raw rec request")
}

func TestExecuteResizes_AlreadyAtTargetSkipsLiveGet(t *testing.T) {
	// Converged two-container pod: listed snapshot already matches the
	// applied target, so executeResizes must not live-Get.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true},
				{Name: "sidecar", Ready: true},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name: "main",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("256Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("256Mi"),
				},
			},
			{
				Name: "sidecar",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("32Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("32Mi"),
				},
			},
		},
	}}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)
	assert.Equal(t, 0, count)

	gets := 0
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "pods" {
			gets++
		}
	}
	assert.Equal(t, 0, gets, "listed already at target must skip the live pod Get")
}

func TestExecuteResizes_MultiContainerStillResizesBoth(t *testing.T) {
	// Two containers that both need work still both reach UpdateResize
	// after the per-pod live Get hoist. Get-count is covered by
	// TestExecuteResizes_AlreadyAtTargetSkipsLiveGet.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true},
				{Name: "sidecar", Ready: true},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name: "main",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("250m"), MemoryRequest: resource.MustParse("128Mi"),
				},
			},
			{
				Name: "sidecar",
				Current: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
				},
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("32Mi"),
				},
			},
		},
	}}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)
	assert.Equal(t, 1, count)

	updates := 0
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updates++
		}
	}
	assert.GreaterOrEqual(t, updates, 2, "both containers should reach UpdateResize")
}

func TestExecuteResizes_BudgetCapsSkipDoesNotConsumeBudget(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	pod2 := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, podMap("api-server", pod1, pod2), nil, nil)
	assert.Equal(t, 1, count, "a skipped pod should not consume budget needed by another pod")
}

func TestExecuteResizes_BudgetCapsResizeFailureSpendsFilterSlot(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	failedPod := pod1.Name
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		if updated.Name == failedPod {
			failedPod = ""
			return true, nil, fmt.Errorf("simulated resize failure")
		}
		return false, nil, nil
	})
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 0, count, "filter-then-apply spends the cycle slot on the first planned increase even if apply fails")
	require.NotEmpty(t, history)
	assert.Contains(t, []attunev1alpha1.ResizeResult{history[0].Result}, attunev1alpha1.ResizeResultFailed)
}

func TestExecuteResizes_EvictionSpendsFilterSlot(t *testing.T) {
	pod1 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod1.Name = "api-server-abc-1"
	pod1.Status.Conditions = append(pod1.Status.Conditions, corev1.PodCondition{
		Type:   "PodResizePending",
		Status: corev1.ConditionTrue,
		Reason: "Infeasible",
	})
	pod2 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 0, count, "filter-then-apply spends the cycle slot on the first planned increase even if it evicts")
	evicted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultEvicted {
			evicted = true
		}
	}
	assert.True(t, evicted, "history should record the fallback eviction explicitly")
}

func TestExecuteResizes_MixedOutcomePodDoesNotLeakSuccessOrBudget(t *testing.T) {
	apiPod1 := newResizePod("api-server", "200m", "256Mi", "500m", "256Mi")
	apiPod1.Name = "api-server-abc-1"
	apiPod1.Spec.Containers = append(apiPod1.Spec.Containers, corev1.Container{
		Name:  "sidecar",
		Image: "busybox",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	})
	apiPod2 := apiPod1.DeepCopy()
	apiPod2.Name = "api-server-abc-2"
	workerPod := newResizePod("worker", "200m", "256Mi", "1000m", "256Mi")
	workerPod.Name = "worker-abc-1"

	apiDeploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	workerDeploy := newTestDeployment("worker", "default", map[string]string{"app": "worker"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(apiDeploy, workerDeploy, apiPod1, apiPod2, workerPod).Build()
	clientset := kubefake.NewSimpleClientset(apiPod1.DeepCopy(), apiPod2.DeepCopy(), workerPod.DeepCopy())
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		if updated.Name != apiPod1.Name {
			return false, nil, nil
		}
		for _, c := range updated.Spec.Containers {
			if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
				return true, nil, fmt.Errorf("simulated sidecar resize failure")
			}
		}
		return false, nil, nil
	})
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 1
	cpuBudget := resource.MustParse("400m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("64Mi"),
					},
				},
			},
		},
		newResizeRecommendation("worker", "200m", "256Mi", "200m", "256Mi", "500m", "256Mi", "500m", "256Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{apiDeploy, workerDeploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*apiPod1}, "worker": {*workerPod}}, nil, nil)

	assert.Equal(t, 1, count, "only the worker workload should count as resized after api-server falls back to eviction")

	apiSuccesses := 0
	workerSuccesses := 0
	apiEvictions := 0
	for _, h := range history {
		if h.Workload == "api-server" && h.Method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			apiSuccesses++
		}
		if h.Workload == "worker" && h.Method == "InPlace" && h.Result == attunev1alpha1.ResizeResultSuccess {
			workerSuccesses++
		}
		if h.Workload == "api-server" && h.Method == "Eviction" && h.Result == attunev1alpha1.ResizeResultEvicted {
			apiEvictions++
		}
	}
	assert.Equal(t, 0, apiSuccesses, "eviction fallback should clear earlier in-place success history for the same pod")
	assert.Equal(t, 2, workerSuccesses, "worker workload should still resize after api-server refunds its reserved budget")
	assert.Equal(t, 1, apiEvictions, "api-server should record the fallback eviction explicitly")
}

func TestExecuteResizes_BudgetCapsRevertSpendsFilterSlot(t *testing.T) {
	pod1 := newResizePodWithStatus("api-server", "200m", "256Mi", "500m", "256Mi", 0)
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePodWithStatus("api-server", "200m", "256Mi", "500m", "256Mi", 0)
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	wrappedClient := &failOnNamedPodUpdateClient{Client: fakeClient, failPodName: pod1.Name}
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "500m", "256Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		recommendations, map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 0, count, "filter-then-apply spends the cycle slot on the first planned increase even if it reverts")
	reverted := false
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted {
			reverted = true
			break
		}
	}
	assert.True(t, reverted, "history should contain a reverted entry for the failed first pod")
}

func TestExecuteResizes_ConcurrentResizes(t *testing.T) {
	// Test that maxConcurrentResizes > 1 processes multiple pods without races.
	pod1 := newResizePod("api-server", "500m", "256Mi", "750m", "384Mi")
	pod1.Name = "api-server-abc-1"
	pod2 := newResizePod("api-server", "500m", "256Mi", "750m", "384Mi")
	pod2.Name = "api-server-abc-2"
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod1, pod2).Build()
	clientset := kubefake.NewSimpleClientset(pod1.DeepCopy(), pod2.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 5 // allow parallelism

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "256Mi", "0", "0", "750m", "384Mi", "0", "0"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod1, *pod2}}, nil, nil)
	assert.Equal(t, 1, count, "workload should count as resized once")
	assert.NotEmpty(t, history, "should produce resize history entries")
}

func TestExecuteResizes_MultiContainerSequential(t *testing.T) {
	// A pod with two containers should be resized sequentially.
	// persistResizeAnnotations propagates the fresh pod back to the caller
	// so the second container uses an up-to-date resourceVersion.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	count, _ := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)
	assert.Equal(t, 1, count, "workload should be resized")

	// Both containers should have UpdateResize called.
	// In tests, the kubefake and controller-runtime fake are separate stores,
	// so the second container's annotation persistence may conflict. We verify
	// correctness by checking that UpdateResize was called for both containers
	// via the clientset actions.
	resizedContainers := make(map[string]bool)
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			for _, c := range updated.Spec.Containers {
				if c.Name == "main" && c.Resources.Requests.Cpu().MilliValue() == 750 {
					resizedContainers["main"] = true
				}
				if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
					resizedContainers["sidecar"] = true
				}
			}
		}
	}
	assert.True(t, resizedContainers["main"], "main container should have UpdateResize called")
	assert.True(t, resizedContainers["sidecar"], "sidecar container should have UpdateResize called")
}

func TestExecuteResizes_EvictionBlockedKeepsPriorInPlaceSuccess(t *testing.T) {
	// One Running replica, two containers. Main resizes in-place first.
	// After that UpdateResize, live Get reports Infeasible so sidecar
	// attempts eviction, which last-replica blocks. Prior success must
	// still count the workload as resized.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.AvailableReplicas = 1

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	var inPlaceApplied atomic.Bool
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		inPlaceApplied.Store(true)
		return false, nil, nil
	})
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !inPlaceApplied.Load() {
			return false, nil, nil
		}
		ga, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, err := clientset.Tracker().Get(ga.GetResource(), ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		live, ok := obj.(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		live = live.DeepCopy()
		live.Status.Conditions = append(live.Status.Conditions, corev1.PodCondition{
			Type:   "PodResizePending",
			Status: corev1.ConditionTrue,
			Reason: "Infeasible",
		})
		return true, live, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)
	assert.Equal(t, 1, count, "prior in-place success must still count the workload as resized")

	mainSuccess := false
	sidecarBlocked := false
	for _, h := range history {
		if h.Container == "main" && h.Method == resize.MethodInPlace && h.Result == attunev1alpha1.ResizeResultSuccess {
			mainSuccess = true
		}
		if h.Container == "sidecar" && h.Reason == reasonEvictionLastReplica {
			sidecarBlocked = true
		}
		assert.NotEqual(t, attunev1alpha1.ResizeResultEvicted, h.Result,
			"last-replica eviction must not evict")
	}
	assert.True(t, mainSuccess, "history should keep in-place Success for main")
	assert.True(t, sidecarBlocked, "history should record eviction_last_replica for sidecar")
}

func TestExecuteResizes_EvictionBlockedStillResizesSiblingInPlace(t *testing.T) {
	// First container's in-place resize fails and last-replica blocks
	// eviction. The sibling must still get an in-place attempt.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.AvailableReplicas = 1

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	var resizeAttempts atomic.Int32
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		if resizeAttempts.Add(1) == 1 {
			return true, nil, fmt.Errorf("in-place resize rejected for first container")
		}
		return false, nil, nil
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("250m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("32Mi"),
					},
				},
			},
		},
	}

	_, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)

	mainBlocked := false
	sidecarSuccess := false
	for _, h := range history {
		if h.Container == "main" && h.Reason == reasonEvictionLastReplica {
			mainBlocked = true
		}
		if h.Container == "sidecar" && h.Method == resize.MethodInPlace && h.Result == attunev1alpha1.ResizeResultSuccess {
			sidecarSuccess = true
		}
	}
	assert.True(t, mainBlocked, "main should record last-replica eviction block")
	assert.True(t, sidecarSuccess, "sidecar must still resize in place after sibling eviction block")
	assert.GreaterOrEqual(t, resizeAttempts.Load(), int32(2), "sidecar must reach UpdateResize")
}

func TestExecuteResizes_EvictionBlockedDoesNotRepeatList(t *testing.T) {
	// Both containers fail in-place. Eviction is last-replica blocked.
	// The second container must still be attempted in-place, but List+Evict
	// must run only once for the pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.AvailableReplicas = 1

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("in-place resize rejected")
	})

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("250m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("32Mi"),
					},
				},
			},
		},
	}

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "last_replica"))
	_, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations,
		map[string][]corev1.Pod{"api-server": {*pod}}, nil, nil)

	lists := 0
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			lists++
		}
	}
	assert.Equal(t, 1, lists, "eviction last-replica List must run once per pod, not once per container")
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "last_replica")),
		"last_replica eviction metric must increment once")

	sawMain := false
	sawSidecar := false
	for _, h := range history {
		if h.Container == "main" && h.Reason == reasonEvictionLastReplica {
			sawMain = true
		}
		if h.Container == "sidecar" {
			sawSidecar = true
		}
	}
	assert.True(t, sawMain, "main must record last-replica block")
	assert.True(t, sawSidecar, "sidecar must still be processed after the eviction block")
}

func TestExecuteResizes_MultiContainer_BudgetExhaustion(t *testing.T) {
	// A pod with two containers where the CPU budget is exhausted after the
	// first container resize. The second container should be skipped (budget
	// check returns false), and the budget consumed by the first container
	// should NOT be refunded. The second workload also exceeds the remaining
	// budget and is similarly deferred.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-abc-1", Namespace: "default",
			Labels: map[string]string{"app": "api-server"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
				{Name: "sidecar", Image: "envoy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	// Second workload to verify budget is consumed and not refunded.
	workerPod := newResizePod("worker", "200m", "128Mi", "0", "0")
	workerPod.Name = "worker-abc-1"

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	workerDeploy := newTestDeployment("worker", "default", map[string]string{"app": "worker"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, workerDeploy, pod, workerPod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy(), workerPod.DeepCopy())

	autoRevert := false
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = &autoRevert
	// Budget: 300m CPU. First container increases by 250m (500m→750m),
	// leaving 50m. Worker needs 150m (200m→350m) which exceeds remaining.
	cpuBudget := resource.MustParse("300m")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpuBudget

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"), MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("750m"), MemoryRequest: resource.MustParse("384Mi"),
					},
				},
				{
					Name: "sidecar",
					Current: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("64Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
		newResizeRecommendation("worker", "200m", "128Mi", "0", "0", "350m", "128Mi", "0", "0"),
	}

	workloads := []client.Object{deploy, workerDeploy}
	podsByWorkload := map[string][]corev1.Pod{
		"api-server": {*pod},
		"worker":     {*workerPod},
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		workloads, recommendations, podsByWorkload, nil, nil)

	// 1. totalResized = 1: the api-server workload was resized (first container
	//    succeeded), but worker was not (budget exhausted).
	assert.Equal(t, 1, count, "exactly one workload should be counted as resized")

	// 2. History entries only contain the first container's resize.
	require.NotEmpty(t, history, "history should contain entries for the resized container")
	for _, h := range history {
		assert.Equal(t, "main", h.Container,
			"only main container should appear in history, got container %q", h.Container)
		assert.Equal(t, "api-server", h.Workload,
			"only api-server workload should appear in history, got workload %q", h.Workload)
	}

	// 3. UpdateResize was only called for main (not sidecar, not worker).
	resizedContainers := make(map[string]bool)
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			for _, c := range updated.Spec.Containers {
				if c.Name == "main" && c.Resources.Requests.Cpu().MilliValue() == 750 {
					resizedContainers["main"] = true
				}
				if c.Name == "sidecar" && c.Resources.Requests.Cpu().MilliValue() == 200 {
					resizedContainers["sidecar"] = true
				}
			}
			if updated.Name == "worker-abc-1" {
				resizedContainers["worker"] = true
			}
		}
	}
	assert.True(t, resizedContainers["main"], "main container should have UpdateResize called")
	assert.False(t, resizedContainers["sidecar"], "sidecar container should NOT have UpdateResize called")
	assert.False(t, resizedContainers["worker"], "worker should NOT have UpdateResize called")

	// 4. Budget consumed for main (+250m) and NOT refunded: worker needs
	//    +150m but only 50m remains, so no worker history entries exist.
	for _, h := range history {
		if h.Workload == "worker" {
			t.Error("worker workload should not appear in history (budget exhausted)")
		}
	}
}

// --- Issue #437: persistResizeAnnotations exhausted-retries path ---

func TestExecuteResizes_AnnotationConflictExhaustedRetries(t *testing.T) {
	// When all 3 annotation persist retries are exhausted due to conflicts,
	// the resize should be reverted with reason "annotation-persist-conflict".
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 0)
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	// Set conflictsLeft to 3 (matches maxRetries in persistResizeAnnotations),
	// so all retry attempts fail with 409 Conflict.
	wrappedClient := &conflictThenSucceedClient{Client: fakeClient, conflictsLeft: 3}

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = wrappedClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "750m", "384Mi", "1500m", "768Mi"),
	}

	workloads := []client.Object{deploy}
	count, history := reconciler.executeResizes(context.Background(), policy, workloads, recommendations, podMap("api-server", pod), nil, nil)

	// The resize should have been reverted because all retries were exhausted.
	assert.Equal(t, 0, count, "net resized count should be 0 after conflict exhaustion revert")

	// History should show Reverted entries with annotation-persist-conflict reason.
	require.NotEmpty(t, history)
	var foundReverted bool
	for _, h := range history {
		if h.Result == attunev1alpha1.ResizeResultReverted && h.Reason == "annotation-persist-conflict" {
			foundReverted = true
			break
		}
	}
	assert.True(t, foundReverted, "history should contain a Reverted entry with reason annotation-persist-conflict")

	// Verify that a Reverted event was emitted mentioning annotation-persist-conflict.
	var foundRevertEvent bool
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "Reverted") && strings.Contains(event, "annotation-persist-conflict") {
				foundRevertEvent = true
			}
		default:
			goto done437
		}
	}
done437:
	assert.True(t, foundRevertEvent, "expected a Reverted event mentioning annotation-persist-conflict")

	// All 3 conflict attempts should have been seen.
	assert.Equal(t, 3, wrappedClient.conflictsSeen, "should have exhausted all 3 retries")

	// Verify that a revert was issued via UpdateResize (2 calls: original + revert).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.Equal(t, 2, resizeCalls, "should have 2 UpdateResize calls: original resize + revert")
}
