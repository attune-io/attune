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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestFilterPlannedByBudget_DropsOverBudgetBeforeApply(t *testing.T) {
	t.Parallel()
	planned := []plannedPod{
		{Pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1"}}, Actions: []resizeAction{
			{PodName: "p1", Container: "main", CPUIncrease: 200},
		}},
		{Pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2"}}, Actions: []resizeAction{
			{PodName: "p2", Container: "main", CPUIncrease: 200},
		}},
	}
	got, deferred := filterPlannedByBudget(planned, 200, -1)
	require.Len(t, got, 2)
	assert.Len(t, got[0].Actions, 1)
	assert.Empty(t, got[1].Actions)
	require.Len(t, deferred, 1)
	assert.Equal(t, "p2", deferred[0].PodName)
}

func TestFilterActionsByBudget(t *testing.T) {
	t.Parallel()
	act := func(name string, cpu, mem int64) resizeAction {
		return resizeAction{Container: name, CPUIncrease: cpu, MemIncrease: mem}
	}
	tests := []struct {
		name      string
		actions   []resizeAction
		cpu, mem  int64
		wantKeep  []string
		wantDefer []string
	}{
		{
			name:      "unlimited keeps all",
			actions:   []resizeAction{act("a", 100, 0), act("b", 200, 0)},
			cpu:       -1,
			mem:       -1,
			wantKeep:  []string{"a", "b"},
			wantDefer: nil,
		},
		{
			name:      "second increase exceeds remaining",
			actions:   []resizeAction{act("a", 200, 0), act("b", 200, 0)},
			cpu:       300,
			mem:       -1,
			wantKeep:  []string{"a"},
			wantDefer: []string{"b"},
		},
		{
			name:      "decrease is free and does not consume",
			actions:   []resizeAction{act("down", 0, 0), act("up", 250, 0)},
			cpu:       250,
			mem:       -1,
			wantKeep:  []string{"down", "up"},
			wantDefer: nil,
		},
		{
			name:      "exact budget keeps the last action",
			actions:   []resizeAction{act("a", 100, 0), act("b", 200, 0)},
			cpu:       300,
			mem:       -1,
			wantKeep:  []string{"a", "b"},
			wantDefer: nil,
		},
		{
			name:      "memory cap defers independently",
			actions:   []resizeAction{act("a", 0, 100), act("b", 0, 100)},
			cpu:       -1,
			mem:       100,
			wantKeep:  []string{"a"},
			wantDefer: []string{"b"},
		},
		{
			name:      "later cheaper action still kept",
			actions:   []resizeAction{act("big", 200, 0), act("small", 50, 0)},
			cpu:       100,
			mem:       -1,
			wantKeep:  []string{"small"},
			wantDefer: []string{"big"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keep, deferred := filterActionsByBudget(tc.actions, tc.cpu, tc.mem)
			var gotKeep, gotDefer []string
			for _, a := range keep {
				gotKeep = append(gotKeep, a.Container)
			}
			for _, a := range deferred {
				gotDefer = append(gotDefer, a.Container)
			}
			assert.Equal(t, tc.wantKeep, gotKeep)
			assert.Equal(t, tc.wantDefer, gotDefer)
		})
	}
}

func TestPlanPodActions_UsesAppliedTargetAndSkipsAtTarget(t *testing.T) {
	t.Parallel()
	// Guaranteed 256Mi/256Mi. Rec 300Mi request / 400Mi limit is raised
	// to 400/400. Listed already at 400/400 must produce no action.
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("256Mi")
	r := NewAttunePolicyReconciler()
	policy := newTestPolicy("p", "default")
	rec := attunev1alpha1.WorkloadRecommendation{
		Workload: "api-server",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Current: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("256Mi"),
				CPULimit: resource.MustParse("200m"), MemoryLimit: resource.MustParse("256Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("200m"), MemoryRequest: resource.MustParse("300Mi"),
				CPULimit: resource.MustParse("200m"), MemoryLimit: resource.MustParse("400Mi"),
			},
		}},
	}

	actions := r.planPodActions(policy, pod, rec)
	require.Len(t, actions, 1)
	assert.False(t, actions[0].AtTarget)
	assert.Equal(t, int64(400-256)<<20, actions[0].MemIncrease,
		"plan cost must be the Guaranteed-raised delta, not the raw 44Mi rec")

	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("400Mi")
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("400Mi")
	at := r.planPodActions(policy, pod, rec)
	require.Len(t, at, 1)
	assert.True(t, at[0].AtTarget)
	assert.Equal(t, int64(0), at[0].MemIncrease)
}

func TestPlanPodActions_ClampsToLiveLeftoverLimit(t *testing.T) {
	t.Parallel()
	// Live leftover CPU limit 200m (LimitRange). Rec request 500m has no
	// CPU limit, so the plan must dest-clamp before AtTarget/budget.
	pod := newResizePod("api-server", "100m", "256Mi", "200m", "256Mi")
	r := NewAttunePolicyReconciler()
	policy := newTestPolicy("p", "default")
	rec := attunev1alpha1.WorkloadRecommendation{
		Workload: "api-server",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("100m"),
				MemoryRequest: resource.MustParse("256Mi"),
				CPULimit:      resource.MustParse("200m"),
				MemoryLimit:   resource.MustParse("256Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("256Mi"),
			},
		}},
	}

	actions := r.planPodActions(policy, pod, rec)
	require.Len(t, actions, 1)
	assert.False(t, actions[0].AtTarget)
	assert.Equal(t, int64(200), actions[0].Target.Requests.Cpu().MilliValue(),
		"plan target must dest-clamp 500m to leftover live limit 200m")
	assert.Contains(t, actions[0].Clamped, "cpu")
	assert.Equal(t, int64(100), actions[0].CPUIncrease,
		"plan cost must be 200-100, not the unclamped 500-100")

	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
	at := r.planPodActions(policy, pod, rec)
	require.Len(t, at, 1)
	assert.True(t, at[0].AtTarget)
	assert.Equal(t, int64(0), at[0].CPUIncrease)
}

func TestObserveAndPlanPod_SkipsLiveGetWhenListedAtTarget(t *testing.T) {
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	r, _ := newResizeReconciler(pod, deploy)
	policy := newTestPolicy("p", "default")
	rec := newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "200m", "256Mi", "0", "0")

	got, err := r.observeAndPlanPod(context.Background(), policy, *pod, rec, deploy)
	require.NoError(t, err)
	require.Len(t, got.Actions, 1)
	assert.True(t, got.Actions[0].AtTarget)

	gets := 0
	for _, a := range r.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "pods" {
			gets++
		}
	}
	assert.Equal(t, 0, gets, "listed already at target must skip the live Get")
}

func TestObserveAndPlanPod_LiveGetError(t *testing.T) {
	pod := newResizePod("api-server", "500m", "256Mi", "500m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	r, _ := newResizeReconciler(pod, deploy)
	cs := r.Clientset.(*kubefake.Clientset)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected live Get 403")
	})
	policy := newTestPolicy("p", "default")
	rec := newResizeRecommendation("api-server", "500m", "256Mi", "0", "0", "200m", "256Mi", "0", "0")

	_, err := r.observeAndPlanPod(context.Background(), policy, *pod, rec, deploy)
	require.Error(t, err)
}
