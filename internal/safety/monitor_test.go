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

package safety

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

func TestCheckPod(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	twoHoursAgo := now.Add(-2 * time.Hour)

	tests := []struct {
		name       string
		pod        *corev1.Pod // nil means pod does not exist in the cluster
		record     ResizeRecord
		wantSafe   bool
		wantReason string
	}{
		{
			name: "healthy pod is safe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "app",
							RestartCount: 0,
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			record: ResizeRecord{
				PodName:      "web-0",
				Namespace:    "default",
				Container:    "app",
				ResizedAt:    oneHourAgo,
				RestartCount: 0,
			},
			wantSafe:   true,
			wantReason: "",
		},
		{
			name: "OOMKill after resize is unsafe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default"},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.NewTime(now),
								},
							},
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			record: ResizeRecord{
				PodName:   "web-1",
				Namespace: "default",
				Container: "app",
				ResizedAt: oneHourAgo,
			},
			wantSafe:   false,
			wantReason: "oomkill",
		},
		{
			name: "OOMKill before resize is safe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "default"},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason:     "OOMKilled",
									FinishedAt: metav1.NewTime(twoHoursAgo),
								},
							},
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			record: ResizeRecord{
				PodName:   "web-2",
				Namespace: "default",
				Container: "app",
				ResizedAt: oneHourAgo,
			},
			wantSafe:   true,
			wantReason: "",
		},
		{
			name: "excessive restarts is unsafe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-3", Namespace: "default"},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "app",
							RestartCount: 5,
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			record: ResizeRecord{
				PodName:      "web-3",
				Namespace:    "default",
				Container:    "app",
				ResizedAt:    oneHourAgo,
				RestartCount: 3,
			},
			wantSafe:   false,
			wantReason: "restart",
		},
		{
			name: "restarts within baseline threshold is safe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-baseline", Namespace: "default"},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "app",
							RestartCount: 6,
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			record: ResizeRecord{
				PodName:      "web-baseline",
				Namespace:    "default",
				Container:    "app",
				ResizedAt:    oneHourAgo,
				RestartCount: 5,
			},
			wantSafe:   true,
			wantReason: "",
		},
		{
			name: "pod not ready is unsafe",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-4", Namespace: "default"},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "app",
							RestartCount: 0,
						},
					},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			record: ResizeRecord{
				PodName:      "web-4",
				Namespace:    "default",
				Container:    "app",
				ResizedAt:    oneHourAgo,
				RestartCount: 0,
			},
			wantSafe:   false,
			wantReason: "notready",
		},
		{
			name: "pod not found is safe",
			pod:  nil,
			record: ResizeRecord{
				PodName:   "gone-pod",
				Namespace: "default",
				Container: "app",
				ResizedAt: oneHourAgo,
			},
			wantSafe:   true,
			wantReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fakeClient *fake.Clientset
			if tt.pod != nil {
				fakeClient = fake.NewSimpleClientset(tt.pod)
			} else {
				fakeClient = fake.NewSimpleClientset()
			}

			monitor := NewMonitor(fakeClient, testr.New(t))
			verdict, err := monitor.CheckPod(context.Background(), tt.record, now)
			require.NoError(t, err)

			assert.Equal(t, tt.wantSafe, verdict.Safe, "safe mismatch")
			assert.Equal(t, tt.wantReason, verdict.Reason, "reason mismatch")
		})
	}
}

// ---------- CheckCriticalStatuses ----------

func TestCheckCriticalStatuses_OOMKill(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(now),
						},
					},
				},
			},
		},
	}

	record := ResizeRecord{
		PodName:   "web-0",
		Namespace: "default",
		Container: "app",
		ResizedAt: oneHourAgo,
	}

	v := CheckCriticalStatuses(pod, record)
	require.NotNil(t, v)
	assert.False(t, v.Safe)
	assert.Equal(t, "oomkill", v.Reason)
}

func TestCheckCriticalStatuses_ExcessiveRestarts(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 5},
			},
		},
	}

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-1 * time.Hour),
		RestartCount: 3,
	}

	v := CheckCriticalStatuses(pod, record)
	require.NotNil(t, v)
	assert.False(t, v.Safe)
	assert.Equal(t, "restart", v.Reason)
}

func TestCheckCriticalStatuses_NotReadyIsNotCritical(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-30 * time.Second),
		RestartCount: 0,
	}

	// Healthy pod (even if not ready) should return nil -- critical-only
	// checks don't look at readiness.
	v := CheckCriticalStatuses(pod, record)
	assert.Nil(t, v)
}

func TestCheckCriticalStatuses_OOMKillBeforeResize(t *testing.T) {
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	oneHourAgo := time.Now().Add(-1 * time.Hour)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(twoHoursAgo),
						},
					},
				},
			},
		},
	}

	record := ResizeRecord{
		PodName:   "web-0",
		Namespace: "default",
		Container: "app",
		ResizedAt: oneHourAgo,
	}

	v := CheckCriticalStatuses(pod, record)
	assert.Nil(t, v, "OOMKill before resize should not trigger critical detection")
}

func TestCheckCriticalStatuses_InitContainerOOMKill(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "istio-proxy",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: metav1.NewTime(now),
						},
					},
				},
			},
		},
	}

	record := ResizeRecord{
		PodName:   "web-0",
		Namespace: "default",
		Container: "istio-proxy",
		ResizedAt: oneHourAgo,
	}

	v := CheckCriticalStatuses(pod, record)
	require.NotNil(t, v, "OOMKill in init container should trigger critical detection")
	assert.Equal(t, "oomkill", v.Reason)
}

func TestRevertPod(t *testing.T) {
	original := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("750m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	logger := testr.New(t)
	monitor := NewMonitor(clientset, logger)

	record := ResizeRecord{
		PodName:           "test-pod",
		Namespace:         "default",
		Container:         "app",
		OriginalResources: original,
		ResizedAt:         time.Now().Add(-1 * time.Minute),
	}

	err := monitor.RevertPod(context.Background(), record)
	require.NoError(t, err)

	// Verify UpdateResize was called with original resources.
	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpu := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			mem := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
			assert.True(t, cpu.Equal(resource.MustParse("500m")),
				"CPU should be reverted to 500m, got %s", cpu.String())
			assert.True(t, mem.Equal(resource.MustParse("256Mi")),
				"memory should be reverted to 256Mi, got %s", mem.String())
		}
	}
	assert.True(t, foundResize, "UpdateResize should have been called")
}

func TestRevertPod_InitContainer(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	original := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name:          "istio-proxy",
					RestartPolicy: &restartAlways,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	clientset := fake.NewSimpleClientset(pod)
	logger := testr.New(t)
	monitor := NewMonitor(clientset, logger)

	record := ResizeRecord{
		PodName:           "test-pod",
		Namespace:         "default",
		Container:         "istio-proxy",
		OriginalResources: original,
		ResizedAt:         time.Now().Add(-1 * time.Minute),
	}

	err := monitor.RevertPod(context.Background(), record)
	require.NoError(t, err)

	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			require.Len(t, updated.Spec.InitContainers, 1)
			cpu := updated.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU]
			mem := updated.Spec.InitContainers[0].Resources.Requests[corev1.ResourceMemory]
			assert.True(t, cpu.Equal(resource.MustParse("100m")),
				"CPU should be reverted to 100m, got %s", cpu.String())
			assert.True(t, mem.Equal(resource.MustParse("64Mi")),
				"memory should be reverted to 64Mi, got %s", mem.String())
		}
	}
	assert.True(t, foundResize, "UpdateResize should have been called for init container revert")
}

func TestRevertPod_PodNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := testr.New(t)
	monitor := NewMonitor(clientset, logger)

	record := ResizeRecord{
		PodName:   "nonexistent-pod",
		Namespace: "default",
		Container: "app",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ResizedAt: time.Now().Add(-1 * time.Minute),
	}

	err := monitor.RevertPod(context.Background(), record)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "getting pod for revert")
}

func TestRevertPod_ContainerNotInPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "other-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("750m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	logger := testr.New(t)
	monitor := NewMonitor(clientset, logger)

	record := ResizeRecord{
		PodName:   "test-pod",
		Namespace: "default",
		Container: "nonexistent-container",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ResizedAt: time.Now().Add(-1 * time.Minute),
	}

	// The container won't be found; RevertPod should return nil without
	// issuing an UpdateResize call.
	err := monitor.RevertPod(context.Background(), record)
	assert.NoError(t, err)

	// Verify no UpdateResize was called (only the Get action should exist).
	actions := clientset.Actions()
	for _, a := range actions {
		assert.NotEqual(t, "update", a.GetVerb(),
			"should not issue UpdateResize when container is not in pod")
	}
}

func TestCheckPod_ContainerNotMatched(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))

	record := ResizeRecord{
		PodName:   "web-0",
		Namespace: "default",
		Container: "app",
		ResizedAt: time.Now().Add(-1 * time.Hour),
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "pod with unmatched container should be considered safe")
}

func TestCheckPod_NoPodReadyCondition(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			// No conditions at all.
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-1 * time.Hour),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "pod with no Ready condition should be considered safe")
}

// ---------- CPU Throttle detection ----------

type mockThrottleChecker struct {
	ratio  float64
	err    error
	gotNS  string
	gotPod string
	gotCtr string
}

func (m *mockThrottleChecker) GetThrottleRatio(_ context.Context, ns, pod, ctr string, _ time.Time) (float64, error) {
	m.gotNS = ns
	m.gotPod = pod
	m.gotCtr = ctr
	return m.ratio, m.err
}

func TestCheckPod_ThrottleDetected(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	checker := &mockThrottleChecker{ratio: 0.6}
	monitor.WithThrottleChecker(checker, 0.5)

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m grace period
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	assert.Equal(t, "throttle", verdict.Reason)
	assert.Contains(t, verdict.Message, "60%")

	// Verify correct identifiers were passed to the throttle checker.
	assert.Equal(t, "default", checker.gotNS)
	assert.Equal(t, "web-0", checker.gotPod)
	assert.Equal(t, "app", checker.gotCtr)
}

func TestCheckPod_ThrottleSkippedDuringGracePeriod(t *testing.T) {
	// When a resize happened less than 5 minutes ago, the throttle check
	// should be skipped because the Prometheus rate(…[5m]) window still
	// contains 100% pre-resize data. Without this grace period, containers
	// that were heavily throttled (the ones most in need of upscaling)
	// would be immediately reverted in an infinite loop.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	checker := &mockThrottleChecker{ratio: 0.9} // Very high throttle
	monitor.WithThrottleChecker(checker, 0.5)

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-30 * time.Second), // 30s ago, within grace
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "should skip throttle check during grace period")
	assert.True(t, verdict.ThrottleDeferred, "should signal throttle was deferred")
}

func TestCheckPod_ThrottleDeferredFalseAfterGrace(t *testing.T) {
	// After the grace period, ThrottleDeferred should be false (throttle was checked).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	monitor.WithThrottleChecker(&mockThrottleChecker{ratio: 0.3}, 0.5)

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m, grace elapsed
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
	assert.False(t, verdict.ThrottleDeferred, "throttle was checked, not deferred")
}

func TestCheckPod_ThrottleDeferredFalseNoChecker(t *testing.T) {
	// Without a throttle checker, ThrottleDeferred should always be false.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	// No throttle checker configured.

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-30 * time.Second), // within grace window
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
	assert.False(t, verdict.ThrottleDeferred, "no checker means no deferral")
}

func TestCheckPod_ThrottleBelowThreshold(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	monitor.WithThrottleChecker(&mockThrottleChecker{ratio: 0.3}, 0.5)

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m grace period
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
}

func TestCheckPod_NoThrottleChecker(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	// No throttle checker configured -- should skip throttle check.

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-1 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
}

func TestCheckPod_ThrottleQueryError(t *testing.T) {
	// When the throttle checker returns an error (e.g., Prometheus down),
	// the monitor should fail open: treat the pod as safe and log the error.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	checker := &mockThrottleChecker{err: assert.AnError}
	monitor.WithThrottleChecker(checker, 0.5)

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m grace period so throttle check runs
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "should fail open when throttle check errors")
}

func TestRevertPod_UpdateResizeError(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("750m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, assert.AnError
		}
		return false, nil, nil
	})

	monitor := NewMonitor(clientset, testr.New(t))
	record := ResizeRecord{
		PodName:   "test-pod",
		Namespace: "default",
		Container: "app",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ResizedAt: time.Now().Add(-1 * time.Minute),
	}

	err := monitor.RevertPod(context.Background(), record)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reverting resize for pod")
}

func TestRevertPod_RetriesOnConflict(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("750m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	conflictsLeft := 2
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			if conflictsLeft > 0 {
				conflictsLeft--
				return true, nil, apierrors.NewConflict(
					schema.GroupResource{Group: "", Resource: "pods"},
					"test-pod",
					assert.AnError,
				)
			}
			return false, nil, nil
		}
		return false, nil, nil
	})

	monitor := NewMonitor(clientset, testr.New(t))
	record := ResizeRecord{
		PodName:   "test-pod",
		Namespace: "default",
		Container: "app",
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ResizedAt: time.Now().Add(-1 * time.Minute),
	}

	err := monitor.RevertPod(context.Background(), record)
	require.NoError(t, err, "RevertPod should succeed after retrying past conflicts")
	assert.Equal(t, 0, conflictsLeft, "all conflicts should have been consumed")

	// Verify the pod was reverted to original resources.
	reverted, getErr := clientset.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, resource.MustParse("500m"), reverted.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("256Mi"), reverted.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory])
}

// ---------- SLO Guardrail checking ----------

type mockSLOQuerier struct {
	value float64
	err   error
	// gotQuery captures the last query for assertion.
	gotQuery string
}

func (m *mockSLOQuerier) Query(_ context.Context, query string, _ time.Time) (float64, error) {
	m.gotQuery = query
	return m.value, m.err
}

func TestCheckPod_SLOBreachedAbove(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 0.95}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:       "p99-latency",
			Query:      `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="{{ .Namespace }}"}[5m]))`,
			Threshold:  "0.5",
			Comparison: "above",
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m default eval window
		RestartCount: 0,
		WorkloadName: "my-deployment",
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	assert.Equal(t, "slo:p99-latency", verdict.Reason)
	assert.Contains(t, verdict.Message, "0.9500")
	assert.Contains(t, verdict.Message, "above")
	// Verify template interpolation replaced {{ .Namespace }}.
	assert.Contains(t, querier.gotQuery, `namespace="default"`)
}

func TestCheckPod_SLOBreachedBelow(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 0.90}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:       "success-rate",
			Query:      `sum(rate(http_requests_total{code=~"2.."}[5m])) / sum(rate(http_requests_total[5m]))`,
			Threshold:  "0.95",
			Comparison: "below",
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	assert.Equal(t, "slo:success-rate", verdict.Reason)
	assert.Contains(t, verdict.Message, "below")
}

func TestCheckPod_SLOWithinThreshold(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 0.3}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:       "p99-latency",
			Query:      `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`,
			Threshold:  "0.5",
			Comparison: "above",
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
}

func TestCheckPod_SLOSkippedDuringEvalWindow(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 999.0} // Would breach, but should be skipped
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:       "p99-latency",
			Query:      `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`,
			Threshold:  "0.5",
			Comparison: "above",
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-30 * time.Second), // within 5m eval window
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "SLO check should be skipped within evaluation window")
}

func TestCheckPod_SLOCustomEvalWindow(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 1.0}
	tenMin := metav1.Duration{Duration: 10 * time.Minute}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:             "p99-latency",
			Query:            `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`,
			Threshold:        "0.5",
			Comparison:       "above",
			EvaluationWindow: &tenMin,
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute), // >5m but <10m custom eval
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "SLO check should be skipped within custom 10m evaluation window")
}

func TestCheckPod_SLOQueryFailsOpen(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{err: assert.AnError}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:       "p99-latency",
			Query:      `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`,
			Threshold:  "0.5",
			Comparison: "above",
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "should fail open when SLO query errors")
}

func TestCheckPod_SLODefaultComparisonAbove(t *testing.T) {
	// When comparison is empty, it should default to "above".
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	querier := &mockSLOQuerier{value: 1.0}
	monitor.WithSLOChecker(querier, []rightsizev1alpha1.SLOGuardrail{
		{
			Name:      "error-rate",
			Query:     `sum(rate(errors_total[5m]))`,
			Threshold: "0.5",
			// Comparison intentionally empty - should default to "above"
		},
	})

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	assert.Equal(t, "slo:error-rate", verdict.Reason)
}

func TestInterpolateSLOQuery(t *testing.T) {
	tests := []struct {
		name     string
		template string
		record   ResizeRecord
		want     string
		wantErr  bool
	}{
		{
			name:     "all variables",
			template: `rate(errors_total{namespace="{{ .Namespace }}", pod="{{ .PodName }}", workload="{{ .WorkloadName }}"}[5m])`,
			record: ResizeRecord{
				Namespace:    "prod",
				PodName:      "web-abc123",
				WorkloadName: "web",
			},
			want: `rate(errors_total{namespace="prod", pod="web-abc123", workload="web"}[5m])`,
		},
		{
			name:     "no variables",
			template: `sum(rate(http_requests_total[5m]))`,
			record:   ResizeRecord{Namespace: "default"},
			want:     `sum(rate(http_requests_total[5m]))`,
		},
		{
			name:     "invalid template",
			template: `{{ .Invalid`,
			record:   ResizeRecord{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := interpolateSLOQuery(tt.template, tt.record)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckPod_SLONoQuerier(t *testing.T) {
	// When no SLO querier is configured, SLO checks should be skipped.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)
	monitor := NewMonitor(clientset, testr.New(t))
	// No SLO querier configured

	record := ResizeRecord{
		PodName:      "web-0",
		Namespace:    "default",
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		RestartCount: 0,
	}

	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
}
