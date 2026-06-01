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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/safety"
)

// ---------- findContainerStatusByName ----------

func TestFindContainerStatusByName(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 1},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init-db", Ready: false, RestartCount: 0},
			},
		},
	}

	tests := []struct {
		name      string
		container string
		wantName  string
		wantNil   bool
	}{
		{
			name:      "finds regular container",
			container: "main",
			wantName:  "main",
		},
		{
			name:      "finds second regular container",
			container: "sidecar",
			wantName:  "sidecar",
		},
		{
			name:      "finds init container",
			container: "init-db",
			wantName:  "init-db",
		},
		{
			name:      "returns nil for missing container",
			container: "nonexistent",
			wantNil:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findContainerStatusByName(pod, tt.container)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantName, got.Name)
			}
		})
	}
}

func TestFindContainerStatusByName_EmptyPod(t *testing.T) {
	pod := &corev1.Pod{}
	got := findContainerStatusByName(pod, "any")
	assert.Nil(t, got)
}

// ---------- runImmediateSafetyCheck ----------

func TestRunImmediateSafetyCheck_AutoRevertDisabled(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				// AutoRevert defaults to nil, but set explicitly to false.
				AutoRevert: boolPtr(false),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(
		context.Background(),
		policy,
		nil, // monitor not needed when autoRevert is disabled
		safety.ResizeRecord{},
	)
	assert.NoError(t, err)
	assert.Empty(t, reason)
}

func TestRunImmediateSafetyCheck_CheckPodError(t *testing.T) {
	// Create a fake clientset that returns an error for pod Get.
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())
	monitor := safety.NewMonitor(cs, logr.Discard())

	record := safety.ResizeRecord{
		PodName:   "test-pod",
		Namespace: "default",
		Container: "main",
		ResizedAt: time.Now(),
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, monitor, record)
	assert.Error(t, err, "should propagate CheckPod error")
	assert.Empty(t, reason, "no revert reason when the check itself fails")
}

func TestRunImmediateSafetyCheck_UnsafePod(t *testing.T) {
	// Create a pod that has restarted 5 times (safety violation).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					RestartCount: 5, // increased by 5 since resize (record has 0)
				},
			},
		},
	}

	cs := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())
	monitor := safety.NewMonitor(cs, logr.Discard())

	record := safety.ResizeRecord{
		PodName:      "test-pod",
		Namespace:    "default",
		Container:    "main",
		ResizedAt:    time.Now().Add(-10 * time.Minute),
		RestartCount: 0, // original restart count before resize
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, monitor, record)
	assert.NoError(t, err)
	assert.NotEmpty(t, reason, "should return a revert reason for unsafe pod")
	assert.Contains(t, reason, "restart", "reason should mention restart increase")
}

func TestRunImmediateSafetyCheck_SafePod(t *testing.T) {
	// Create a healthy pod that passes all safety checks.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "healthy-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					Ready:        true,
					RestartCount: 0,
				},
			},
		},
	}

	cs := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())
	monitor := safety.NewMonitor(cs, logr.Discard())

	record := safety.ResizeRecord{
		PodName:      "healthy-pod",
		Namespace:    "default",
		Container:    "main",
		ResizedAt:    time.Now().Add(-10 * time.Minute),
		RestartCount: 0,
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, monitor, record)
	assert.NoError(t, err)
	assert.Empty(t, reason, "healthy pod should not trigger revert")
}
