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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func mustQty(t *testing.T, raw string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return q
}

func qtyList(t *testing.T, cpu, memory string) corev1.ResourceList {
	t.Helper()
	list := corev1.ResourceList{}
	if cpu != "" {
		list[corev1.ResourceCPU] = mustQty(t, cpu)
	}
	if memory != "" {
		list[corev1.ResourceMemory] = mustQty(t, memory)
	}
	return list
}

func resReq(t *testing.T, cpuReq, memReq, cpuLim, memLim string) corev1.ResourceRequirements {
	t.Helper()
	rr := corev1.ResourceRequirements{}
	if cpuReq != "" || memReq != "" {
		rr.Requests = qtyList(t, cpuReq, memReq)
	}
	if cpuLim != "" || memLim != "" {
		rr.Limits = qtyList(t, cpuLim, memLim)
	}
	return rr
}

func qosPod(t *testing.T, class corev1.PodQOSClass, containers []corev1.Container, inits []corev1.Container) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     containers,
			InitContainers: inits,
		},
		Status: corev1.PodStatus{QOSClass: class},
	}
}

func namedContainer(name string, resources corev1.ResourceRequirements, nativeSidecar bool) corev1.Container {
	c := corev1.Container{Name: name, Resources: resources}
	if nativeSidecar {
		always := corev1.ContainerRestartPolicyAlways
		c.RestartPolicy = &always
	}
	return c
}

func TestQoSClasses_PodLevelResourcesUseOneRule(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
		Spec: corev1.PodSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    mustQty(t, "500m"),
					corev1.ResourceMemory: mustQty(t, "512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    mustQty(t, "500m"),
					corev1.ResourceMemory: mustQty(t, "512Mi"),
				},
			},
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    mustQty(t, "100m"),
						corev1.ResourceMemory: mustQty(t, "128Mi"),
					},
				},
			}},
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    mustQty(t, "150m"),
			corev1.ResourceMemory: mustQty(t, "128Mi"),
		},
	}
	from, to := QoSClasses(pod, "app", target, QoSPlan{})
	require.Equal(t, corev1.PodQOSBurstable, from)
	require.Equal(t, corev1.PodQOSBurstable, to)
	require.True(t, PreservesQoS(pod, "app", target, QoSPlan{}))

	from, to = QoSClasses(pod, "app", target, QoSPlan{InPlacePodLevelResources: true})
	require.Equal(t, corev1.PodQOSGuaranteed, from)
	require.Equal(t, corev1.PodQOSGuaranteed, to)
	require.True(t, PreservesQoS(pod, "app", target, QoSPlan{InPlacePodLevelResources: true}))

	plain := pod.DeepCopy()
	plain.Spec.Resources = nil
	plain.Status.QOSClass = corev1.PodQOSBurstable
	from, to = QoSClasses(plain, "app", target, QoSPlan{})
	require.Equal(t, corev1.PodQOSBurstable, from)
	require.Equal(t, corev1.PodQOSBurstable, to)
}

func TestPreservesQoS_MergedPod(t *testing.T) {
	chart := resReq(t, "250m", "512Mi", "500m", "512Mi")
	below := resReq(t, "100m", "128Mi", "500m", "256Mi")
	equal := resReq(t, "200m", "256Mi", "200m", "256Mi")
	cpuOnlyReq := corev1.ResourceRequirements{Requests: qtyList(t, "100m", "")}
	memLimitOnly := corev1.ResourceRequirements{Limits: qtyList(t, "", "64Mi")}
	zeroCPU := corev1.ResourceRequirements{Requests: qtyList(t, "0", "")}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		container string
		target    corev1.ResourceRequirements
		want      bool
	}{
		{
			name: "requests only cpu raised to live limit becomes guaranteed",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", chart, false),
			}, nil),
			container: "app",
			target:    corev1.ResourceRequirements{Requests: qtyList(t, "500m", "512Mi")},
			want:      false,
		},
		{
			name: "requests only cpu still below live limit stays burstable",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", chart, false),
			}, nil),
			container: "app",
			target:    corev1.ResourceRequirements{Requests: qtyList(t, "400m", "512Mi")},
			want:      true,
		},
		{
			name: "sidecar reaches limits while app stays below",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", below, false),
				namedContainer("sidecar", below, false),
			}, nil),
			container: "sidecar",
			target:    equal,
			want:      true,
		},
		{
			name: "app already equal and sidecar reaches limits",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", equal, false),
				namedContainer("sidecar", below, false),
			}, nil),
			container: "sidecar",
			target:    equal,
			want:      false,
		},
		{
			name: "one-shot init keeps a memory gap",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", equal, false),
			}, []corev1.Container{
				namedContainer("sidecar", equal, true),
				namedContainer("init", below, false),
			}),
			container: "app",
			target:    equal,
			want:      true,
		},
		{
			name: "one-shot init reaching its limit changes class",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", equal, false),
			}, []corev1.Container{
				namedContainer("sidecar", equal, true),
				namedContainer("init", below, false),
			}),
			container: "init",
			target:    equal,
			want:      false,
		},
		{
			name: "besteffort gains a cpu request",
			pod: qosPod(t, corev1.PodQOSBestEffort, []corev1.Container{
				namedContainer("app", corev1.ResourceRequirements{}, false),
			}, nil),
			container: "app",
			target:    cpuOnlyReq,
			want:      false,
		},
		{
			name: "besteffort gains a memory limit",
			pod: qosPod(t, corev1.PodQOSBestEffort, []corev1.Container{
				namedContainer("app", corev1.ResourceRequirements{}, false),
			}, nil),
			container: "app",
			target:    memLimitOnly,
			want:      false,
		},
		{
			name: "besteffort empty overlay stays besteffort",
			pod: qosPod(t, corev1.PodQOSBestEffort, []corev1.Container{
				namedContainer("app", corev1.ResourceRequirements{}, false),
			}, nil),
			container: "app",
			target:    corev1.ResourceRequirements{},
			want:      true,
		},
		{
			name: "besteffort zero cpu request stays besteffort",
			pod: qosPod(t, corev1.PodQOSBestEffort, []corev1.Container{
				namedContainer("app", corev1.ResourceRequirements{}, false),
			}, nil),
			container: "app",
			target:    zeroCPU,
			want:      true,
		},
		{
			name: "memory limit without a memory request stays burstable",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", resReq(t, "100m", "", "100m", "64Mi"), false),
			}, nil),
			container: "app",
			target:    corev1.ResourceRequirements{Requests: qtyList(t, "100m", "")},
			want:      true,
		},
		{
			name: "binary and decimal memory quantities compare equal",
			pod: qosPod(t, corev1.PodQOSGuaranteed, []corev1.Container{
				namedContainer("app", resReq(t, "1", "1Gi", "1", "1024Mi"), false),
			}, nil),
			container: "app",
			target:    resReq(t, "1", "1024Mi", "1", "1Gi"),
			want:      true,
		},
		{
			name: "burstable multiplier keeps a gap while memory is already equal",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", resReq(t, "100m", "256Mi", "400m", "256Mi"), false),
			}, nil),
			container: "app",
			target:    resReq(t, "250m", "256Mi", "500m", "256Mi"),
			want:      true,
		},
		{
			name: "explicit one equalizes burstable into guaranteed",
			pod: qosPod(t, corev1.PodQOSBurstable, []corev1.Container{
				namedContainer("app", resReq(t, "250m", "256Mi", "1000m", "256Mi"), false),
			}, nil),
			container: "app",
			target:    resReq(t, "500m", "256Mi", "500m", "256Mi"),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PreservesQoS(tt.pod, tt.container, tt.target, QoSPlan{})
			require.Equal(t, tt.want, got)
		})
	}
}
