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

package kubeletsim

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/attune-io/attune/internal/resize"
)

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func resizeCPU(t *testing.T, cs *fake.Clientset, milli string) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods("default").Get(context.Background(), "web-0", metav1.GetOptions{})
	require.NoError(t, err)
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse(milli)
	updated, err := cs.CoreV1().Pods("default").UpdateResize(context.Background(), "web-0", pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	return updated
}

func TestInstall_AcceptedWritesStatusResources(t *testing.T) {
	pod := testPod()
	cs := fake.NewSimpleClientset(pod)
	Install(cs, Options{Outcome: Accepted})
	got := resizeCPU(t, cs, "250m")
	require.NotEmpty(t, got.Status.ContainerStatuses)
	assert.True(t, got.Status.ContainerStatuses[0].Resources.Requests.Cpu().Equal(resource.MustParse("250m")))
	stored, err := cs.CoreV1().Pods("default").Get(context.Background(), "web-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, stored.Status.ContainerStatuses[0].Resources.Requests.Cpu().Equal(resource.MustParse("250m")),
		"tracker Get must see the accepted status")
	assert.True(t, resize.IsEligibleForResize(stored))
	assert.False(t, resize.IsResizeDeferred(stored))
	assert.False(t, resize.IsResizeInfeasible(stored))
}

func TestInstall_DeferredAndInfeasible(t *testing.T) {
	t.Run("deferred", func(t *testing.T) {
		cs := fake.NewSimpleClientset(testPod())
		Install(cs, Options{Outcome: Deferred})
		got := resizeCPU(t, cs, "250m")
		assert.True(t, resize.IsResizeDeferred(got))
		assert.False(t, resize.IsEligibleForResize(got))
	})
	t.Run("infeasible", func(t *testing.T) {
		cs := fake.NewSimpleClientset(testPod())
		Install(cs, Options{Outcome: Infeasible})
		got := resizeCPU(t, cs, "250m")
		assert.True(t, resize.IsResizeInfeasible(got))
		assert.True(t, resize.IsEligibleForResize(got),
			"infeasible is retryable; eligibility skips only non-infeasible pending")
	})
}

func TestInstall_InProgressTimeout(t *testing.T) {
	cs := fake.NewSimpleClientset(testPod())
	Install(cs, Options{Outcome: InProgress, InProgressAge: time.Hour + time.Minute})
	got := resizeCPU(t, cs, "250m")
	assert.True(t, resize.IsEligibleForResize(got),
		"InProgress older than one hour is treated as stale")

	cs2 := fake.NewSimpleClientset(testPod())
	Install(cs2, Options{Outcome: InProgress})
	fresh := resizeCPU(t, cs2, "250m")
	assert.False(t, resize.IsEligibleForResize(fresh),
		"fresh InProgress must block a new resize")
}
