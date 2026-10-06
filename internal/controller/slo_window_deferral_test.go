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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestCheckPendingSafetyObservations_SLODeferredKeepsThenReverts(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pod := sloObservationPod("slo-breach", resizeTime, corev1.ConditionTrue, []corev1.Container{
		sloResizedContainer("main", "250m", "256Mi"),
	}, nil)
	window := 10 * time.Minute
	policy := policyWithSLOWindow(2*time.Minute, &window)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockCollector{queryFunc: func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0.99, nil
	}}

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(3 * time.Minute) })
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.True(t, pending, "open window keeps observation pending")
	assert.Equal(t, 0, calls, "query waits for the evaluation window")
	assert.True(t, sloTrackingKept(t, fakeClient, "slo-breach"))
	assert.Equal(t, 0, sloResizeUpdates(reconciler))

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(11 * time.Minute) })
	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending, "breach reverts and clears observation")
	assert.Equal(t, 1, calls)
	assert.GreaterOrEqual(t, sloResizeUpdates(reconciler), 1)
}

func TestCheckPendingSafetyObservations_SLODeferredHealthyThenClears(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pod := sloObservationPod("slo-healthy", resizeTime, corev1.ConditionTrue, []corev1.Container{
		sloResizedContainer("main", "250m", "256Mi"),
	}, nil)
	window := 10 * time.Minute
	policy := policyWithSLOWindow(2*time.Minute, &window)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockCollector{queryFunc: func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0.1, nil
	}}

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(3 * time.Minute) })
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.True(t, pending)
	assert.True(t, sloTrackingKept(t, fakeClient, "slo-healthy"))
	assert.Equal(t, 0, calls)

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(11 * time.Minute) })
	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending, "healthy query after the window clears tracking")
	assert.Equal(t, 1, calls)
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.False(t, sloTrackingKept(t, fakeClient, "slo-healthy"))
}

func TestCheckPendingSafetyObservations_SLODeferredKeepsAfterThrottlePasses(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pod := sloObservationPod("slo-throttle", resizeTime, corev1.ConditionTrue, []corev1.Container{
		sloResizedContainer("main", "250m", "256Mi"),
	}, nil)
	window := 10 * time.Minute
	policy := policyWithSLOWindow(2*time.Minute, &window)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockThrottleCollector{throttleRatio: 0}
	collector.queryFunc = func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0.99, nil
	}

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(6 * time.Minute) })
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.True(t, pending, "SLO window keeps tracking after throttle grace")
	assert.Equal(t, 0, calls)
	assert.True(t, sloTrackingKept(t, fakeClient, "slo-throttle"))

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(11 * time.Minute) })
	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending)
	assert.Equal(t, 1, calls)
	assert.GreaterOrEqual(t, sloResizeUpdates(reconciler), 1)
}

func TestCheckPendingSafetyObservations_SLODeferredHonoursLiveReady(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pod := sloObservationPod("slo-flap", resizeTime, corev1.ConditionFalse, []corev1.Container{
		sloResizedContainer("main", "250m", "256Mi"),
	}, nil)
	window := 10 * time.Minute
	policy := policyWithSLOWindow(2*time.Minute, &window)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	cs := reconciler.Clientset.(*kubefake.Clientset)
	live, err := cs.CoreV1().Pods("default").Get(context.Background(), "slo-flap", metav1.GetOptions{})
	require.NoError(t, err)
	for i := range live.Status.Conditions {
		if live.Status.Conditions[i].Type == corev1.PodReady {
			live.Status.Conditions[i].Status = corev1.ConditionTrue
		}
	}
	_, err = cs.CoreV1().Pods("default").UpdateStatus(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(3 * time.Minute) })
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, safetyWorkloads())
	assert.True(t, pending, "live Ready pod with an open window stays tracked")
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.True(t, sloTrackingKept(t, fakeClient, "slo-flap"))
}

func TestCheckPendingSafetyObservations_SLODeferredSkipsAlreadyRevertedContainer(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	oom := sloResizedContainer("already", "500m", "512Mi")
	pod := sloObservationPod("slo-two", resizeTime, corev1.ConditionTrue, []corev1.Container{
		oom,
		sloResizedContainer("open", "250m", "256Mi"),
	}, []corev1.ContainerStatus{
		{
			Name: "already",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				FinishedAt: metav1.NewTime(resizeTime),
			}},
		},
		{Name: "open", RestartCount: 0},
	})
	window := 10 * time.Minute
	policy := policyWithSLOWindow(2*time.Minute, &window)
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{{
		Workload:  "api-server",
		Container: "already",
		Resource:  "cpu",
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultReverted,
	}}
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(3 * time.Minute) })

	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, safetyWorkloads())
	assert.True(t, pending, "the open container keeps the pod tracked")
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.True(t, sloTrackingKept(t, fakeClient, "slo-two"))

	pending = reconciler.checkPendingSafetyObservations(context.Background(), policy, &mockCollector{}, safetyWorkloads())
	assert.True(t, pending)
	assert.Equal(t, 0, sloResizeUpdates(reconciler), "the reverted container is not reverted again")
}

func TestCheckPendingSafetyObservations_DefaultWindowAtPeriodBoundaryClears(t *testing.T) {
	resizeTime := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pod := sloObservationPod("slo-boundary", resizeTime, corev1.ConditionTrue, []corev1.Container{
		sloResizedContainer("main", "250m", "256Mi"),
	}, nil)
	policy := policyWithSLOWindow(5*time.Minute, nil)
	reconciler, fakeClient := newSafetyTestReconciler(pod)
	calls := 0
	collector := &mockCollector{queryFunc: func(context.Context, string, time.Time) (float64, error) {
		calls++
		return 0, nil
	}}

	reconciler.SetNowFunc(func() time.Time { return resizeTime.Add(5 * time.Minute) })
	pending := reconciler.checkPendingSafetyObservations(context.Background(), policy, collector, safetyWorkloads())
	assert.False(t, pending, "exact window boundary is evaluated and does not stay pending")
	assert.Equal(t, 1, calls)
	assert.Equal(t, 0, sloResizeUpdates(reconciler))
	assert.False(t, sloTrackingKept(t, fakeClient, "slo-boundary"))
}

func policyWithSLOWindow(period time.Duration, window *time.Duration) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: period}
	guardrail := attunev1alpha1.SLOGuardrail{
		Name:       "latency",
		Query:      "vector(1)",
		Threshold:  "0.5",
		Comparison: "above",
	}
	if window != nil {
		guardrail.EvaluationWindow = &metav1.Duration{Duration: *window}
	}
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{guardrail}
	return policy
}

func sloResizedContainer(name, cpu, mem string) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: "nginx",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func sloObservationPod(name string, resizedAt time.Time, ready corev1.ConditionStatus, containers []corev1.Container, statuses []corev1.ContainerStatus) *corev1.Pod {
	annotations := map[string]string{
		"attune.io/resized-at":       resizedAt.UTC().Format(time.RFC3339),
		"attune.io/resized-workload": "api-server",
		"attune.io/policy":           "test-policy",
	}
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, c.Name)
		annotations["attune.io/original-cpu-request."+c.Name] = "500m"
		annotations["attune.io/original-memory-request."+c.Name] = "512Mi"
	}
	annotations["attune.io/resized-containers"] = strings.Join(names, ",")
	if statuses == nil {
		statuses = make([]corev1.ContainerStatus, 0, len(containers))
		for _, c := range containers {
			statuses = append(statuses, corev1.ContainerStatus{Name: c.Name, RestartCount: 0})
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      map[string]string{"attune.io/tracked": "true"},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{Containers: containers},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: ready},
			},
			ContainerStatuses: statuses,
		},
	}
}

func sloTrackingKept(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var updated corev1.Pod
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &updated))
	_, kept := updated.Annotations["attune.io/resized-at"]
	return kept
}

func sloResizeUpdates(reconciler *AttunePolicyReconciler) int {
	n := 0
	cs, ok := reconciler.Clientset.(*kubefake.Clientset)
	if !ok {
		return 0
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "resize" {
			n++
		}
	}
	return n
}
