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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
		ResizedAt:    time.Now().Add(-1 * time.Minute),
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
		ResizedAt:    time.Now().Add(-1 * time.Minute),
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
		ResizedAt:    time.Now().Add(-1 * time.Minute),
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
