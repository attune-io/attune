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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/attune-io/attune/internal/testutil/kubeletsim"
)

func simPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
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

func TestSummarizeResizeBlockers_SimulatorDeferredAndInfeasible(t *testing.T) {
	deferredCS := fake.NewSimpleClientset(simPod("deferred-0"))
	kubeletsim.Install(deferredCS, kubeletsim.Options{Outcome: kubeletsim.Deferred})
	dPod, err := deferredCS.CoreV1().Pods("default").Get(context.Background(), "deferred-0", metav1.GetOptions{})
	require.NoError(t, err)
	dPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	dPod, err = deferredCS.CoreV1().Pods("default").UpdateResize(context.Background(), "deferred-0", dPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	infeasCS := fake.NewSimpleClientset(simPod("infeas-0"))
	kubeletsim.Install(infeasCS, kubeletsim.Options{Outcome: kubeletsim.Infeasible})
	iPod, err := infeasCS.CoreV1().Pods("default").Get(context.Background(), "infeas-0", metav1.GetOptions{})
	require.NoError(t, err)
	iPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	iPod, err = infeasCS.CoreV1().Pods("default").UpdateResize(context.Background(), "infeas-0", iPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	sum := summarizeResizeBlockers(map[string][]corev1.Pod{
		"web": {*dPod, *iPod},
	}, time.Now())
	assert.Equal(t, 1, sum.DeferredCount)
	assert.Equal(t, 1, sum.InfeasibleCount)
	assert.Equal(t, []string{"deferred-0"}, sum.DeferredNames)
	assert.Equal(t, []string{"infeas-0"}, sum.InfeasibleNames)
	assert.NotEmpty(t, sum.DeferredAges)
}
