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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

// makeCanaryPod creates a pod for canary selection tests. When running is true
// the pod phase is set to Running; when deleting is true a DeletionTimestamp
// is set so that resize.IsEligibleForResize returns false.
func makeCanaryPod(name string, running bool, deleting bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	if !running {
		pod.Status.Phase = corev1.PodPending
	}
	if deleting {
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
	}
	return pod
}

// makeRunningPods creates the requested number of running, non-deleting pods.
func makeRunningPods(count int) []corev1.Pod {
	pods := make([]corev1.Pod, count)
	for i := range pods {
		pods[i] = makeCanaryPod(fmt.Sprintf("pod-%d", i), true, false)
	}
	return pods
}

func TestSelectPodsForResize_OneShot_SelectsExactlyOne(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeOneShot, 0)
	assert.Len(t, selected, 1)
}

func TestSelectPodsForResize_Canary_10PercentOf20(t *testing.T) {
	pods := makeRunningPods(20)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeCanary, 10)
	assert.Len(t, selected, 2) // 10% of 20 = 2
}

func TestSelectPodsForResize_Canary_10PercentOf3_RoundsUp(t *testing.T) {
	pods := makeRunningPods(3)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeCanary, 10)
	assert.Len(t, selected, 1) // 10% of 3 = 0.3, rounds up to 1
}

func TestSelectPodsForResize_Canary_100Percent_SelectsAll(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeCanary, 100)
	assert.Len(t, selected, 5)
}

func TestSelectPodsForResize_Auto_SelectsAllEligible(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeAuto, 0)
	assert.Len(t, selected, 5)
}

func TestSelectPodsForResize_Observe_SelectsNone(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeObserve, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_Recommend_SelectsNone(t *testing.T) {
	pods := makeRunningPods(5)
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeRecommend, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_AllIneligible_ReturnsNil(t *testing.T) {
	pods := []corev1.Pod{
		makeCanaryPod("pod-0", true, true), // running but deleting
		makeCanaryPod("pod-1", true, true),
		makeCanaryPod("pod-2", true, true),
	}
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeAuto, 0)
	assert.Nil(t, selected)
}

func TestSelectPodsForResize_MixedEligibility(t *testing.T) {
	pods := []corev1.Pod{
		makeCanaryPod("pod-0", true, false),  // eligible
		makeCanaryPod("pod-1", true, true),   // ineligible (deleting)
		makeCanaryPod("pod-2", true, false),  // eligible
		makeCanaryPod("pod-3", false, false), // ineligible (not running)
		makeCanaryPod("pod-4", true, false),  // eligible
	}
	selected := selectPodsForResize(pods, rightsizev1alpha1.UpdateModeAuto, 0)
	assert.Len(t, selected, 3)
	for _, p := range selected {
		assert.Equal(t, corev1.PodRunning, p.Status.Phase)
		assert.Nil(t, p.DeletionTimestamp)
	}
}
