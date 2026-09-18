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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestShouldSkipResize_LimitRangeViolation(t *testing.T) {
	scheme := testScheme()
	// LimitRange requiring at least 100m CPU.
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lr", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("50m"), // below LimitRange min
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "quota/limitrange violation")
	assert.Contains(t, reason, "below LimitRange minimum")
}

func TestShouldSkipResize_QuotaHeadroomExceeded(t *testing.T) {
	scheme := testScheme()
	// ResourceQuota with only 300m CPU headroom (hard=1000m, used=700m).
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test-quota", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1000m")},
			Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("700m")},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("900m"), // increase of 700m, only 300m headroom
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("900m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "quota/limitrange violation")
	assert.Contains(t, reason, "would exceed ResourceQuota")
}

func TestShouldSkipResize_NodeAllocatableExceeded(t *testing.T) {
	scheme := testScheme()
	// Node with 2000m CPU and 4Gi memory allocatable.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2000m"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
				{Name: "sidecar", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("1600m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
	}
	// Total after resize: app=1600m + sidecar=500m = 2100m > node 2000m.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1600m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip)
	assert.Contains(t, reason, "exceed node allocatable")
}

func TestShouldSkipResize_NodeAllocatableNotExceeded(t *testing.T) {
	scheme := testScheme()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("1Gi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("1000m"),
			MemoryRequest: resource.MustParse("2Gi"),
		},
	}
	// Total after resize: 1000m < 4000m, 2Gi < 8Gi. Should not skip.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}

	skip, _ := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.False(t, skip)
}

func TestShouldSkipResize_AlreadyAtTarget(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target matches current pod resources exactly.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip, "should skip when pod already matches target")
	assert.Empty(t, reason, "reason should be empty for already-at-target skip")
}

func TestShouldSkipResize_RequestMatchLimitDriftDoesNotSkip(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	skip, _ := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.False(t, skip, "must not skip when requests match but target sets a missing live limit")
}

func TestShouldSkipResize_PreChecksLimitRange(t *testing.T) {
	scheme := testScheme()
	// No objects in the client; LimitRange is passed via pre-fetched checks.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("200m"),
			MemoryRequest: resource.MustParse("128Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	checks := &resizePreChecks{
		limitRanges: []corev1.LimitRange{
			{Spec: corev1.LimitRangeSpec{
				Limits: []corev1.LimitRangeItem{
					{Type: corev1.LimitTypeContainer, Min: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					}},
				},
			}},
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, checks)
	assert.True(t, skip, "should skip when target violates pre-fetched LimitRange")
	assert.Contains(t, reason, "quota/limitrange violation")
}

func TestShouldSkipResize_NodeCacheHit(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target exceeds node allocatable.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5000m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Pre-populate the node cache.
	checks := &resizePreChecks{}
	cachedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	checks.nodeCache.Store("test-node", cachedNode)

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, checks)
	assert.True(t, skip, "should skip when target exceeds cached node allocatable")
	assert.Contains(t, reason, "exceed node allocatable")
}

func TestShouldSkipResize_NodeCacheMiss(t *testing.T) {
	scheme := testScheme()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2000m"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target within node allocatable.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
	// Empty cache; node should be fetched and stored.
	checks := &resizePreChecks{}

	skip, _ := r.shouldSkipResize(context.Background(), pod, containerRec, target, checks)
	assert.False(t, skip, "should not skip when target fits in node allocatable")

	// Verify the node was cached.
	cached, ok := checks.nodeCache.Load("test-node")
	assert.True(t, ok, "node should be cached after miss")
	assert.NotNil(t, cached, "cached node should not be nil")
}

// TestShouldSkipResize_ClientsetPrefersLivePressure ensures MemoryPressure is
// read from the typed Clientset (live API) even when the controller-runtime
// client still holds a stale node without pressure. Stale informer data must
// not allow memory request increases under real pressure.
func TestShouldSkipResize_ClientsetPrefersLivePressure(t *testing.T) {
	scheme := testScheme()
	staleNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
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
	liveNode := staleNode.DeepCopy()
	liveNode.Status.Conditions = []corev1.NodeCondition{{
		Type:   corev1.NodeMemoryPressure,
		Status: corev1.ConditionTrue,
	}}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(staleNode).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = kubefake.NewSimpleClientset(liveNode)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("100m"),
			MemoryRequest: resource.MustParse("32Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}

	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip, "live Clientset MemoryPressure must block memory increase")
	assert.Contains(t, reason, "MemoryPressure")
}

func TestShouldSkipResize_QoSClassChange(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Guaranteed pod: requests == limits for all resources.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSGuaranteed,
		},
	}
	containerRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    resource.MustParse("500m"),
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	// Target changes only requests without matching limits, breaking Guaranteed QoS.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	recorder := events.NewFakeRecorder(10)
	r.Recorder = recorder
	skip, reason := r.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	assert.True(t, skip, "should skip when resize would change QoS class")
	assert.Contains(t, reason, "QoS class")
	select {
	case ev := <-recorder.Events:
		t.Fatalf("shouldSkipResize must not Eventf; callers emit ResizeSkipped, got %s", ev)
	default:
	}
}
