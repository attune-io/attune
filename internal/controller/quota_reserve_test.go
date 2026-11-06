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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/resize"
)

func quotaWith(hard, used corev1.ResourceName, hardQty, usedQty string) corev1.ResourceQuota {
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{hard: resource.MustParse(hardQty)},
			Used: corev1.ResourceList{used: resource.MustParse(usedQty)},
		},
	}
}

func reqTarget(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}
}

func TestTryReserveQuota_SecondIncreaseSeesHeadroom(t *testing.T) {
	checks := &resizePreChecks{
		quotas: []corev1.ResourceQuota{
			quotaWith(corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU, "1000m", "700m"),
		},
	}
	current := reqTarget("200m", "128Mi")
	first := reqTarget("400m", "128Mi") // +200m, 300m headroom
	require.NoError(t, tryReserveQuota(checks, current, first))
	got := checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(900), got.MilliValue())

	// +150m still fits the original 300m headroom, and does not fit the
	// 100m left after the first reservation.
	second := reqTarget("350m", "128Mi")
	err := tryReserveQuota(checks, current, second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceed ResourceQuota")
	got = checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(900), got.MilliValue(), "rejected reserve must not charge quota")

	refundQuota(checks, current, first)
	got = checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(700), got.MilliValue())
	exact := reqTarget("500m", "128Mi") // +300m, the full headroom
	require.NoError(t, tryReserveQuota(checks, current, exact))
	got = checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(1000), got.MilliValue())
}

func TestTryReserveQuota_AliasAndLimitKeys(t *testing.T) {
	checks := &resizePreChecks{
		quotas: []corev1.ResourceQuota{{
			ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: "default"},
			Status: corev1.ResourceQuotaStatus{
				Hard: corev1.ResourceList{
					corev1.ResourceCPU:          resource.MustParse("1"),
					corev1.ResourceLimitsMemory: resource.MustParse("1Gi"),
				},
				Used: corev1.ResourceList{
					corev1.ResourceCPU:          resource.MustParse("200m"),
					corev1.ResourceLimitsMemory: resource.MustParse("256Mi"),
				},
			},
		}},
	}
	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("400m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"), // decrease: not charged
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	require.NoError(t, tryReserveQuota(checks, current, target))
	used := checks.quotas[0].Status.Used
	cpuUsed := used[corev1.ResourceCPU]
	memUsed := used[corev1.ResourceLimitsMemory]
	assert.Equal(t, int64(400), cpuUsed.MilliValue())
	assert.Equal(t, int64(512)<<20, memUsed.Value())
	_, hasReq := used[corev1.ResourceRequestsCPU]
	assert.False(t, hasReq, "alias quota must stay on the cpu key")
}

func TestTryReserveQuota_OneWinner(t *testing.T) {
	checks := &resizePreChecks{
		quotas: []corev1.ResourceQuota{
			quotaWith(corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU, "500m", "200m"),
		},
	}
	current := reqTarget("100m", "64Mi")
	target := reqTarget("400m", "64Mi") // +300m, headroom 300m
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tryReserveQuota(checks, current, target); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
	got := checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(500), got.MilliValue())
}

func TestResizeContainer_ReservedQuotaSkipsLaterIncrease(t *testing.T) {
	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"app": "api-server"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	podA, podB := pod("api-a"), pod("api-b")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, podA, podB).Build()
	clientset := kubefake.NewSimpleClientset(podA.DeepCopy(), podB.DeepCopy())
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOnly

	checks := &resizePreChecks{
		quotas: []corev1.ResourceQuota{
			quotaWith(corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU, "1000m", "700m"),
		},
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	rec := attunev1alpha1.ContainerRecommendation{Name: "main"}

	targetA := podA.Spec.Containers[0].Resources.DeepCopy()
	targetA.Requests[corev1.ResourceCPU] = resource.MustParse("400m")
	_, outcomeA := r.resizeContainer(context.Background(), resizeParams{
		Policy: policy, Pod: podA, Workload: deploy, WorkloadName: "api-server",
		ContainerRec: rec, Target: *targetA, Resizer: resizer, Now: metav1.Now(),
		Checks: checks, LiveApplied: true,
	})
	require.Equal(t, resizeOutcomeInPlace, outcomeA)
	usedAfterA := checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(900), usedAfterA.MilliValue())

	targetB := podB.Spec.Containers[0].Resources.DeepCopy()
	// +150m fits the original 300m headroom and misses the 100m left
	// after pod A. Without the reservation this UpdateResize is sent.
	targetB.Requests[corev1.ResourceCPU] = resource.MustParse("350m")
	_, outcomeB := r.resizeContainer(context.Background(), resizeParams{
		Policy: policy, Pod: podB, Workload: deploy, WorkloadName: "api-server",
		ContainerRec: rec, Target: *targetB, Resizer: resizer, Now: metav1.Now(),
		Checks: checks, LiveApplied: true,
	})
	assert.Equal(t, resizeOutcomeNone, outcomeB)
	usedAfterB := checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(900), usedAfterB.MilliValue(), "skipped sibling must not keep a reservation")

	resized := map[string]int{}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		update, ok := action.(k8stesting.UpdateAction)
		if !ok {
			continue
		}
		resizedPod, ok := update.GetObject().(*corev1.Pod)
		if !ok || resizedPod == nil {
			continue
		}
		resized[resizedPod.Name]++
	}
	assert.GreaterOrEqual(t, resized["api-a"], 1)
	assert.Zero(t, resized["api-b"], "second increase must be skipped before UpdateResize")
}

func TestApplyStartupBoosts_ReservedQuotaSkipsSecondContainer(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "2.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	container := func(name string) corev1.Container {
		return corev1.Container{
			Name:  name,
			Image: "nginx",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container("main"), container("sidecar")},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	checks := &resizePreChecks{
		quotas: []corev1.ResourceQuota{
			quotaWith(corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU, "1000m", "500m"),
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "my-app",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			{Name: "sidecar", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
		},
	}}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*pod}}, recs,
		resize.NewPodResizer(clientset, ctrl.Log), checks)

	used := checks.quotas[0].Status.Used[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(800), used.MilliValue(), "only the first boost is reserved")

	resized := map[string]int{}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		update, ok := action.(k8stesting.UpdateAction)
		if !ok {
			continue
		}
		resizedPod, ok := update.GetObject().(*corev1.Pod)
		if !ok || len(resizedPod.Spec.Containers) == 0 {
			continue
		}
		for _, c := range resizedPod.Spec.Containers {
			cpu := c.Resources.Requests[corev1.ResourceCPU]
			if cpu.MilliValue() > 100 {
				resized[c.Name]++
			}
		}
	}
	assert.GreaterOrEqual(t, resized["main"], 1)
	assert.Zero(t, resized["sidecar"], "second boost must be skipped before UpdateResize")
}
