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

func TestCanResizeInPlace(t *testing.T) {
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
			got := CanResizeInPlace(tt.pod)
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

func TestWaitForResize_ImmediateSuccess(t *testing.T) {
	targetCPU := resource.MustParse("250m")
	targetMem := resource.MustParse("512Mi")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    targetCPU.DeepCopy(),
							corev1.ResourceMemory: targetMem.DeepCopy(),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    targetCPU.DeepCopy(),
							corev1.ResourceMemory: targetMem.DeepCopy(),
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(pod)
	resizer := NewPodResizer(fakeClient, testr.New(t))

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    targetCPU.DeepCopy(),
			corev1.ResourceMemory: targetMem.DeepCopy(),
		},
	}

	err := resizer.WaitForResize(context.Background(), "default", "web-0", "app", target, 10*time.Second)
	assert.NoError(t, err)
}
