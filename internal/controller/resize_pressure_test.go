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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodePressureBlocksIncrease(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}
	higherMem := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	lowerMem := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	// CPU-only increase (memory flat): MemoryPressure must not block.
	higherCPU := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	// Both increase.
	higherBoth := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	memPressure := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue,
			}},
		},
	}
	diskPressure := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue,
			}},
		},
	}
	pidPressure := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n3"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue,
			}},
		},
	}
	// False conditions must not block.
	pressureFalse := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n4"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse,
			}},
		},
	}

	assert.Contains(t, nodePressureBlocksIncrease(memPressure, pod, "app", higherMem), "MemoryPressure")
	assert.Empty(t, nodePressureBlocksIncrease(memPressure, pod, "app", lowerMem),
		"memory decrease under MemoryPressure is allowed")
	assert.Empty(t, nodePressureBlocksIncrease(memPressure, pod, "app", higherCPU),
		"CPU-only increase under MemoryPressure is allowed")
	assert.Contains(t, nodePressureBlocksIncrease(memPressure, pod, "app", higherBoth), "MemoryPressure",
		"memory increase (with CPU) under MemoryPressure is blocked")

	assert.Contains(t, nodePressureBlocksIncrease(diskPressure, pod, "app", higherMem), "DiskPressure")
	assert.Contains(t, nodePressureBlocksIncrease(diskPressure, pod, "app", higherCPU), "DiskPressure")
	assert.Empty(t, nodePressureBlocksIncrease(diskPressure, pod, "app", lowerMem),
		"decrease under DiskPressure is allowed")

	assert.Contains(t, nodePressureBlocksIncrease(pidPressure, pod, "app", higherCPU), "PIDPressure")
	assert.Contains(t, nodePressureBlocksIncrease(pidPressure, pod, "app", higherMem), "PIDPressure")

	assert.Empty(t, nodePressureBlocksIncrease(pressureFalse, pod, "app", higherMem))
	pressureUnknown := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n5"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionUnknown,
			}},
		},
	}
	assert.Empty(t, nodePressureBlocksIncrease(pressureUnknown, pod, "app", higherMem),
		"Unknown status must not block")
	assert.Empty(t, nodePressureBlocksIncrease(nil, pod, "app", higherMem))
	assert.Empty(t, nodePressureBlocksIncrease(memPressure, pod, "missing", higherMem),
		"unknown container is a no-op")
}

func TestNodeConditionStatus(t *testing.T) {
	assert.Equal(t, "unknown", nodeConditionStatus(nil, corev1.NodeMemoryPressure))
	assert.Equal(t, "absent", nodeConditionStatus(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n"},
	}, corev1.NodeMemoryPressure))
	assert.Equal(t, "True", nodeConditionStatus(&corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue,
			}},
		},
	}, corev1.NodeMemoryPressure))
	assert.Equal(t, "False", nodeConditionStatus(&corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse,
			}},
		},
	}, corev1.NodeDiskPressure))
}
