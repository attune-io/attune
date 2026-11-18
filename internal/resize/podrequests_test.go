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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func reqList(t *testing.T, cpu, mem string) corev1.ResourceList {
	t.Helper()
	l := corev1.ResourceList{}
	if cpu != "" {
		l[corev1.ResourceCPU] = qty(t, cpu)
	}
	if mem != "" {
		l[corev1.ResourceMemory] = qty(t, mem)
	}
	return l
}

func reqContainer(t *testing.T, name, cpu, mem string) corev1.Container {
	t.Helper()
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: reqList(t, cpu, mem)}}
}

func sidecarContainer(t *testing.T, name, cpu, mem string) corev1.Container {
	t.Helper()
	c := reqContainer(t, name, cpu, mem)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// TestPodRequestTotals is the contract with the scheduler's spec formula
// (k8s.io/component-helpers/resource.PodRequests). Each row is a pod shape
// where a containers-plus-sidecars sum gives a different answer.
func TestPodRequestTotals(t *testing.T) {
	const mi = int64(1024 * 1024)
	tests := []struct {
		name    string
		pod     func(t *testing.T) *corev1.Pod
		wantCPU int64
		wantMem int64
	}{
		{
			name: "regular containers only are summed",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
					reqContainer(t, "a", "100m", "64Mi"),
					reqContainer(t, "b", "200m", "128Mi"),
				}}}
			},
			wantCPU: 300,
			wantMem: 192 * mi,
		},
		{
			name: "regular init container larger than the containers wins",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{reqContainer(t, "init", "2", "64Mi")},
					Containers:     []corev1.Container{reqContainer(t, "main", "500m", "128Mi")},
				}}
			},
			wantCPU: 2000,
			wantMem: 128 * mi,
		},
		{
			name: "sidecar declared before an init container adds to that init",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						sidecarContainer(t, "proxy", "100m", ""),
						reqContainer(t, "init", "1", ""),
					},
					Containers: []corev1.Container{reqContainer(t, "main", "500m", "")},
				}}
			},
			wantCPU: 1100,
			wantMem: 0,
		},
		{
			name: "sidecar declared after an init container does not add to it",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						reqContainer(t, "init", "1", ""),
						sidecarContainer(t, "proxy", "100m", ""),
					},
					Containers: []corev1.Container{reqContainer(t, "main", "500m", "")},
				}}
			},
			wantCPU: 1000,
			wantMem: 0,
		},
		{
			name: "pod-level CPU request replaces the container CPU sum only",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{
					Resources:  &corev1.ResourceRequirements{Requests: reqList(t, "2", "")},
					Containers: []corev1.Container{reqContainer(t, "main", "500m", "256Mi")},
				}}
			},
			wantCPU: 2000,
			wantMem: 256 * mi,
		},
		{
			name: "pod overhead is added",
			pod: func(t *testing.T) *corev1.Pod {
				return &corev1.Pod{Spec: corev1.PodSpec{
					Overhead:   reqList(t, "250m", "64Mi"),
					Containers: []corev1.Container{reqContainer(t, "main", "500m", "128Mi")},
				}}
			},
			wantCPU: 750,
			wantMem: 192 * mi,
		},
		{
			// Guard: the stub already returns zero here.
			name:    "nil pod is zero",
			pod:     func(*testing.T) *corev1.Pod { return nil },
			wantCPU: 0,
			wantMem: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mem := PodRequestTotals(tt.pod(t))
			assert.Equal(t, tt.wantCPU, cpu, "cpu millicores")
			assert.Equal(t, tt.wantMem, mem, "memory bytes")
		})
	}
}

// TestPlannedPod pins the shape the resize engine writes: the target merged
// into the container (an omitted resource keeps its value) and, only for
// in-place pod-level resize, the envelope raised to cover it.
func TestPlannedPod(t *testing.T) {
	livePod := func(t *testing.T, env *corev1.ResourceRequirements) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{
			Resources:  env,
			Containers: []corev1.Container{reqContainer(t, "main", "500m", "256Mi")},
		}}
	}
	cpuOnly := func(t *testing.T) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Requests: reqList(t, "1500m", "")}
	}
	tests := []struct {
		name       string
		envelope   bool // live envelope 500m/256Mi
		inPlace    bool
		wantEnvCPU string // "" means no envelope
	}{
		{name: "no envelope", inPlace: true},
		{name: "envelope raised for in-place pod-level resize", envelope: true, inPlace: true, wantEnvCPU: "1500m"},
		{name: "envelope kept without in-place pod-level resize", envelope: true, wantEnvCPU: "500m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env *corev1.ResourceRequirements
			if tt.envelope {
				env = &corev1.ResourceRequirements{Requests: reqList(t, "500m", "256Mi")}
			}
			live := livePod(t, env)
			before := live.DeepCopy()
			planned := PlannedPod(live, "main", cpuOnly(t), tt.inPlace)
			require.NotNil(t, planned)
			assert.Equal(t, before, live, "live pod must not be mutated")

			got := planned.Spec.Containers[0].Resources.Requests
			assert.Equal(t, int64(1500), got.Cpu().MilliValue(), "target CPU applied")
			assert.True(t, got.Memory().Equal(qty(t, "256Mi")), "omitted memory keeps its value")

			if tt.wantEnvCPU == "" {
				assert.Nil(t, planned.Spec.Resources)
				return
			}
			require.NotNil(t, planned.Spec.Resources)
			wantEnv := qty(t, tt.wantEnvCPU)
			assert.Equal(t, wantEnv.MilliValue(), planned.Spec.Resources.Requests.Cpu().MilliValue(), "envelope CPU")
		})
	}
}
