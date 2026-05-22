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

package resize

import (
	"context"
	"fmt"
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

// newTestPod creates a running pod with a single container that has known
// CPU and memory requests/limits.
func newTestPod(name, namespace, container string, cpuReq, memReq, cpuLim, memLim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: container,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuReq),
							corev1.ResourceMemory: resource.MustParse(memReq),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuLim),
							corev1.ResourceMemory: resource.MustParse(memLim),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// resizeReactor intercepts UpdateResize calls on the fake client and returns
// the submitted pod object so the caller can inspect it.
func resizeReactor(action k8stesting.Action) (bool, runtime.Object, error) {
	if action.GetSubresource() != "resize" {
		return false, nil, nil
	}
	return true, action.(k8stesting.UpdateAction).GetObject(), nil
}

func TestResizePod_CallsUpdateResize(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)
	fakeClient.PrependReactor("update", "pods", resizeReactor)

	resizer := NewPodResizer(fakeClient, testr.New(t))

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}

	_, err := resizer.ResizePod(context.Background(), pod, "app", target)
	require.NoError(t, err)

	// Verify that an update with subresource "resize" was issued.
	var found bool
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "resize" {
			found = true
			updatedPod := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			assert.Equal(t, "web-0", updatedPod.Name)
			assert.Equal(t, "default", updatedPod.Namespace)

			gotCPU := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, gotCPU.Equal(resource.MustParse("250m")),
				"expected cpu request 250m, got %s", gotCPU.String())

			gotMem := updatedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
			assert.True(t, gotMem.Equal(resource.MustParse("1Gi")),
				"expected memory limit 1Gi, got %s", gotMem.String())
			break
		}
	}
	assert.True(t, found, "UpdateResize action was not recorded on the fake client")
}

func TestResizePod_ReturnsCorrectFromTo(t *testing.T) {
	pod := newTestPod("api-1", "prod", "server", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)
	fakeClient.PrependReactor("update", "pods", resizeReactor)

	resizer := NewPodResizer(fakeClient, testr.New(t))

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("600m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}

	results, err := resizer.ResizePod(context.Background(), pod, "server", target)
	require.NoError(t, err)
	require.Len(t, results, 2)

	// First result is CPU.
	cpuResult := results[0]
	assert.Equal(t, "cpu", cpuResult.Resource)
	assert.True(t, cpuResult.From.Equal(resource.MustParse("100m")),
		"cpu from: expected 100m, got %s", cpuResult.From.String())
	assert.True(t, cpuResult.To.Equal(resource.MustParse("300m")),
		"cpu to: expected 300m, got %s", cpuResult.To.String())
	assert.Equal(t, "InPlace", cpuResult.Method)
	assert.True(t, cpuResult.Success)
	assert.NoError(t, cpuResult.Error)

	// Second result is memory.
	memResult := results[1]
	assert.Equal(t, "memory", memResult.Resource)
	assert.True(t, memResult.From.Equal(resource.MustParse("128Mi")),
		"memory from: expected 128Mi, got %s", memResult.From.String())
	assert.True(t, memResult.To.Equal(resource.MustParse("512Mi")),
		"memory to: expected 512Mi, got %s", memResult.To.String())
	assert.Equal(t, "InPlace", memResult.Method)
	assert.True(t, memResult.Success)
	assert.NoError(t, memResult.Error)
}

func TestIsEligibleForResize(t *testing.T) {
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "running pod is eligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: true,
		},
		{
			name: "pod with DeletionTimestamp is ineligible",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: false,
		},
		{
			name: "non-running pod is ineligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			want: false,
		},
		{
			name: "succeeded pod is ineligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
			want: false,
		},
		{
			name: "pod with active resize in progress is ineligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: "PodResizeInProgress", Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "pod with deferred resize is ineligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Deferred"},
					},
				},
			},
			want: false,
		},
		{
			name: "pod with infeasible resize is still eligible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Infeasible"},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsEligibleForResize(tt.pod)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPreservesQoS(t *testing.T) {
	guaranteedPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
	}
	burstablePod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{QOSClass: corev1.PodQOSBurstable},
	}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		container string
		target    corev1.ResourceRequirements
		want      bool
	}{
		{
			name:      "guaranteed pod with requests equal to limits preserves QoS",
			pod:       guaranteedPod,
			container: "app",
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			want: true,
		},
		{
			name:      "guaranteed pod with requests less than limits breaks QoS",
			pod:       guaranteedPod,
			container: "app",
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			want: false,
		},
		{
			name:      "guaranteed pod with missing memory request breaks QoS",
			pod:       guaranteedPod,
			container: "app",
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("200m"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			want: false,
		},
		{
			name:      "burstable pod always preserves QoS",
			pod:       burstablePod,
			container: "app",
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PreservesQoS(tt.pod, tt.container, tt.target)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResizePod_ContainerNotFound(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)

	resizer := NewPodResizer(fakeClient, testr.New(t))

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	results, err := resizer.ResizePod(context.Background(), pod, "nonexistent", target)
	assert.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "not found")
}

func TestResizePod_UpdateResizeAPIError(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)
	fakeClient.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "resize" {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("node has insufficient resources")
	})

	resizer := NewPodResizer(fakeClient, testr.New(t))

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	results, err := resizer.ResizePod(context.Background(), pod, "app", target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "calling UpdateResize for pod default/web-0")
	assert.Contains(t, err.Error(), "node has insufficient resources")

	require.Len(t, results, 2)
	assert.Equal(t, "cpu", results[0].Resource)
	assert.Equal(t, "memory", results[1].Resource)
	assert.False(t, results[0].Success)
	assert.False(t, results[1].Success)
	assert.Error(t, results[0].Error)
	assert.Error(t, results[1].Error)
	assert.Equal(t, "web-0", results[0].PodName)
	assert.Equal(t, "app", results[0].Container)
	assert.Equal(t, "InPlace", results[0].Method)
}

// ---------- findContainer ----------

func TestFindContainer_RegularContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	idx, isInit := findContainer(pod, "app")
	assert.Equal(t, 0, idx)
	assert.False(t, isInit)
}

func TestFindContainer_InitContainer(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "istio-proxy", RestartPolicy: &always}},
			Containers:     []corev1.Container{{Name: "app"}},
		},
	}
	idx, isInit := findContainer(pod, "istio-proxy")
	assert.Equal(t, 0, idx)
	assert.True(t, isInit)
}

func TestFindContainer_NotFound(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	idx, _ := findContainer(pod, "missing")
	assert.Equal(t, -1, idx)
}

// ---------- WouldRestartContainer ----------

func TestWouldRestartContainer_RestartPolicy(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					ResizePolicy: []corev1.ContainerResizePolicy{
						{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer},
					},
				},
			},
		},
	}
	assert.True(t, WouldRestartContainer(pod, "app"))
}

func TestWouldRestartContainer_NotRequired(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					ResizePolicy: []corev1.ContainerResizePolicy{
						{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
					},
				},
			},
		},
	}
	assert.False(t, WouldRestartContainer(pod, "app"))
}

func TestWouldRestartContainer_NoPolicy(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	assert.False(t, WouldRestartContainer(pod, "app"))
}

func TestWouldRestartContainer_ContainerNotFound(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "other"},
			},
		},
	}
	assert.False(t, WouldRestartContainer(pod, "app"))
}

func TestMergeResources(t *testing.T) {
	tests := []struct {
		name          string
		current       corev1.ResourceRequirements
		target        corev1.ResourceRequirements
		wantLimitsNil bool
		wantCPULimit  string
		wantMemLimit  string
	}{
		{
			name: "target has limits, use target limits",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
			wantCPULimit: "500m",
			wantMemLimit: "2Gi",
		},
		{
			name: "target has no limits, current has limits, preserve current",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			},
			wantCPULimit: "1",
			wantMemLimit: "1Gi",
		},
		{
			name: "neither has limits",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			},
			wantLimitsNil: true,
		},
		{
			name: "memory limit decrease allowed when target explicitly sets limit",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
			wantMemLimit: "512Mi", // target explicitly set limit, no clamp
		},
		{
			name: "memory limit decrease clamped when target does not set limit",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				// No limits in target: limit carried forward from current, clamped.
			},
			wantMemLimit: "1Gi", // clamped: carried-forward limit not decreased
		},
		{
			name: "memory limit increase allowed",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
			wantMemLimit: "2Gi",
		},
		{
			name: "mixed controlledValues: target has only CPU limit, memory limit preserved from current",
			current: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			target: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
				// Only CPU limit set (CPU: RequestsAndLimits, Memory: RequestsOnly)
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
			wantCPULimit: "500m",
			wantMemLimit: "512Mi", // preserved from current, not dropped
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := mergeResources(tt.current, tt.target)
			assert.Equal(t, tt.target.Requests.Cpu().MilliValue(), merged.Requests.Cpu().MilliValue())
			if tt.wantLimitsNil {
				assert.Nil(t, merged.Limits)
				return
			}
			require.NotNil(t, merged.Limits)
			if tt.wantCPULimit != "" {
				want := resource.MustParse(tt.wantCPULimit)
				assert.Equal(t, want.MilliValue(), merged.Limits.Cpu().MilliValue(), "CPU limit")
			}
			if tt.wantMemLimit != "" {
				want := resource.MustParse(tt.wantMemLimit)
				assert.Equal(t, want.Value(), merged.Limits.Memory().Value(), "memory limit")
			}
		})
	}
}

func TestIsResizeInfeasible(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "no conditions",
			pod:  &corev1.Pod{},
			want: false,
		},
		{
			name: "infeasible condition present",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Infeasible"},
					},
				},
			},
			want: true,
		},
		{
			name: "resize pending but not infeasible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Deferred"},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsResizeInfeasible(tt.pod))
		})
	}
}

func TestEvictPod_CallsEvictionAPI(t *testing.T) {
	pod := newTestPod("worker-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)

	resizer := NewPodResizer(fakeClient, testr.New(t))
	err := resizer.EvictPod(context.Background(), pod)
	require.NoError(t, err)

	var found bool
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" &&
			action.GetSubresource() == "eviction" {
			found = true
			break
		}
	}
	assert.True(t, found, "Eviction action was not recorded on the fake client")
}

func TestResizePod_InitContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "job-0", Namespace: "batch"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name: "init-sidecar",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	fakeClient := fake.NewSimpleClientset(pod)
	fakeClient.PrependReactor("update", "pods", resizeReactor)

	resizer := NewPodResizer(fakeClient, testr.New(t))
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	results, err := resizer.ResizePod(context.Background(), pod, "init-sidecar", target)
	require.NoError(t, err)
	assert.NotEmpty(t, results)

	// Verify the UpdateResize action targeted InitContainers, not Containers.
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "resize" {
			updatedPod := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			gotCPU := updatedPod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, gotCPU.Equal(resource.MustParse("250m")),
				"expected init container cpu request 250m, got %s", gotCPU.String())
			// Ensure regular container was NOT modified.
			mainCPU := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, mainCPU.Equal(resource.MustParse("500m")),
				"main container should not be modified, got %s", mainCPU.String())
			break
		}
	}
}

func TestEvictPod_ReturnsErrorOnFailure(t *testing.T) {
	pod := newTestPod("worker-1", "default", "app", "100m", "128Mi", "200m", "256Mi")
	fakeClient := fake.NewSimpleClientset(pod)
	fakeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "eviction" {
			return true, nil, fmt.Errorf("eviction denied by PDB")
		}
		return false, nil, nil
	})

	resizer := NewPodResizer(fakeClient, testr.New(t))
	err := resizer.EvictPod(context.Background(), pod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eviction denied by PDB")
}

func TestClampMemoryLimitForPolicy(t *testing.T) {
	tests := []struct {
		name           string
		resizePolicy   []corev1.ContainerResizePolicy
		currentMemLim  string
		targetMemLim   string
		expectedMemLim string
	}{
		{
			name: "NotRequired prevents memory limit decrease",
			resizePolicy: []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
			},
			currentMemLim:  "1Gi",
			targetMemLim:   "64Mi",
			expectedMemLim: "1Gi",
		},
		{
			name:           "no resize policy defaults to NotRequired, prevents decrease",
			resizePolicy:   nil,
			currentMemLim:  "1Gi",
			targetMemLim:   "64Mi",
			expectedMemLim: "1Gi",
		},
		{
			name: "RestartContainer allows memory limit decrease",
			resizePolicy: []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer},
			},
			currentMemLim:  "1Gi",
			targetMemLim:   "64Mi",
			expectedMemLim: "64Mi",
		},
		{
			name: "memory limit increase is always allowed",
			resizePolicy: []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
			},
			currentMemLim:  "64Mi",
			targetMemLim:   "1Gi",
			expectedMemLim: "1Gi",
		},
		{
			name: "no target memory limit is a no-op",
			resizePolicy: []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
			},
			currentMemLim:  "1Gi",
			targetMemLim:   "",
			expectedMemLim: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:         "app",
							ResizePolicy: tt.resizePolicy,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse(tt.currentMemLim),
								},
							},
						},
					},
				},
			}

			target := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			}
			if tt.targetMemLim != "" {
				target.Limits = corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse(tt.targetMemLim),
				}
			}

			result := clampMemoryLimitForPolicy(pod, "app", target)

			if tt.expectedMemLim == "" {
				assert.Empty(t, result.Limits)
			} else {
				expected := resource.MustParse(tt.expectedMemLim)
				actual := result.Limits[corev1.ResourceMemory]
				assert.True(t, expected.Equal(actual),
					"expected memory limit %s, got %s", expected.String(), actual.String())
			}
		})
	}
}
