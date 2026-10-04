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

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/safety"
)

func oomWireNow() time.Time {
	return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
}

func oomWirePolicy(name string) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy(name, "ns")
	policy.UID = types.UID("uid-" + name)
	policy.Spec.Memory.OOMBump = &attunev1alpha1.OOMBump{}
	return policy
}

func oomWireDeploy(name string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
}

func oomMetric(policy, result string) float64 {
	return promtestutil.ToFloat64(operatormetrics.OOMBumpTotal.WithLabelValues("ns", policy, result))
}

func oomWireRaw(t *testing.T, count int, origin, floor int64, oomAt, holdUntil time.Time, restart int32) string {
	t.Helper()
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: count, Origin: origin, Floor: floor,
		OOMAt: oomAt, Restart: restart, HoldUntil: holdUntil,
	})
	require.NoError(t, err)
	return raw
}

func parsedQty(t *testing.T, raw string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return q
}

func samePodBacking(a, b []corev1.Pod) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

func TestOOMBumpPlan_NilBlock(t *testing.T) {
	policy := newTestPolicy("oom-wire-nil-block", "ns")
	r := &AttunePolicyReconciler{}
	results := []string{oomBumpApplied, oomBumpClamped, oomBumpCapped, oomBumpSkipped}
	before := make(map[string]float64, len(results))
	for _, result := range results {
		before[result] = oomMetric(policy.Name, result)
	}
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(oomWireNow(), 1), "", false)
	plan := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", false, 0, false, []corev1.Pod{pod}, oomWireNow())
	assert.False(t, plan.Active)
	for _, result := range results {
		assert.Equal(t, before[result], oomMetric(policy.Name, result), result)
	}
}

func TestOOMBumpPlan_NilBookDoesNotPanic(t *testing.T) {
	policy := oomWirePolicy("oom-wire-nil-book")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	r := &AttunePolicyReconciler{}
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(oomWireNow(), 1), "", false)
	var plan oomBumpWorkloadPlan
	require.NotPanics(t, func() {
		plan = r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", false, 0, false, []corev1.Pod{pod}, oomWireNow())
	})
	assert.True(t, plan.Active)
	require.NotEmpty(t, plan.Stamps)
	_, ok := r.peekOOMBump(policy, oomWireDeploy("api"), "app", &pod)
	assert.False(t, ok)
}

func TestOOMBumpPlan_ExcludedSkipsOnce(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-excluded")
	r := &AttunePolicyReconciler{}
	fresh := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now, 1), "", false)
	before := oomMetric(policy.Name, oomBumpSkipped)
	plan := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", true, 0, false, []corev1.Pod{fresh}, now)
	assert.Equal(t, before+1, oomMetric(policy.Name, oomBumpSkipped))
	assert.Equal(t, []string{oomBumpSkipped}, plan.MetricNow)
	require.Len(t, plan.Stamps, 1)
	raw, err := formatOOMBumpRecord(plan.Stamps[0].Stamp)
	require.NoError(t, err)
	stamped := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now, 1), raw, false)
	again := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", true, 0, false, []corev1.Pod{stamped}, now)
	assert.Empty(t, again.MetricNow)
	assert.Equal(t, before+1, oomMetric(policy.Name, oomBumpSkipped))

	quiet := oomBumpPod("p", "app", "200Mi", nil, "", false)
	quietPlan := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", true, 0, false, []corev1.Pod{quiet}, now)
	assert.Empty(t, quietPlan.MetricNow)
	assert.Equal(t, before+1, oomMetric(policy.Name, oomBumpSkipped))
}

func TestOOMBumpPlan_RecommendSkipsPutAutoStores(t *testing.T) {
	now := oomWireNow()
	r := NewAttunePolicyReconciler()
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now, 1), "", false)
	wl := oomWireDeploy("api")

	recommend := oomWirePolicy("oom-wire-recommend")
	appliedBefore := oomMetric(recommend.Name, oomBumpApplied)
	plan := r.planContainerOOMBump(context.Background(), recommend, wl, "app", false, 0, false, []corev1.Pod{pod}, now)
	require.NotEmpty(t, plan.Stamps)
	_, ok := r.peekOOMBump(recommend, wl, "app", &pod)
	assert.False(t, ok)
	assert.Equal(t, appliedBefore, oomMetric(recommend.Name, oomBumpApplied))

	auto := oomWirePolicy("oom-wire-auto")
	auto.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	autoApplied := oomMetric(auto.Name, oomBumpApplied)
	autoPlan := r.planContainerOOMBump(context.Background(), auto, wl, "app", false, 0, false, []corev1.Pod{pod}, now)
	stamp, ok := r.peekOOMBump(auto, wl, "app", &pod)
	require.True(t, ok)
	assert.False(t, stamp.AnnotationOnly)
	assert.Equal(t, 1, stamp.Stamp.Count)
	assert.Equal(t, int64(314572800), stamp.Stamp.Floor)
	assert.Equal(t, qtyBytes(t, "200Mi"), stamp.Stamp.Origin)
	assert.Equal(t, autoApplied, oomMetric(auto.Name, oomBumpApplied))
	require.NotEmpty(t, autoPlan.Stamps)
}

func TestOOMBumpRecommend_BelowMinimumDataPoints(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-low-samples")
	r := NewAttunePolicyReconciler()
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now.Add(-time.Minute), 1), "", false)
	q := parsedQty(t, "200Mi")
	rec, ok, partial, points := r.recommendContainer(context.Background(), recommendContainerInput{
		policy:   policy,
		workload: oomWireDeploy("api"),
		container: corev1.Container{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q},
			},
		},
		pods:              []corev1.Pod{pod},
		now:               now,
		minimumDataPoints: 48,
	})
	require.True(t, ok)
	assert.False(t, partial)
	assert.Equal(t, 0, points)
	assert.Equal(t, int64(314572800), rec.Recommended.MemoryRequest.Value())
	require.NotNil(t, rec.Explanation)
	require.NotNil(t, rec.Explanation.Memory)
	assert.Contains(t, rec.Explanation.Memory.FinalAdjustment, "oomBump")
}

func TestOOMBumpRecommend_MemoryFromCPURatioWait(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-ratio-wait")
	policy.Spec.Memory.MemoryFromCPURatio = stringPtr("4")
	r := NewAttunePolicyReconciler()
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now.Add(-time.Minute), 1), "", false)
	q := parsedQty(t, "200Mi")
	rec, ok, _, _ := r.recommendContainer(context.Background(), recommendContainerInput{
		policy:   policy,
		workload: oomWireDeploy("api"),
		container: corev1.Container{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q},
			},
		},
		pods:              []corev1.Pod{pod},
		now:               now,
		minimumDataPoints: 48,
	})
	require.True(t, ok)
	assert.Equal(t, int64(314572800), rec.Recommended.MemoryRequest.Value())
	require.NotNil(t, rec.Explanation)
	require.NotNil(t, rec.Explanation.Memory)
	assert.Contains(t, rec.Explanation.Memory.FinalAdjustment, "oomBump")
}

func TestOOMBumpPlan_HoldThenPercentile(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-hold")
	r := NewAttunePolicyReconciler()
	origin := qtyBytes(t, "200Mi")
	floor := int64(314572800)
	trigger := now.Add(-time.Hour)
	held := oomWireRaw(t, 1, origin, floor, trigger, now.Add(time.Hour), 1)
	pod := oomBumpPod("p", "app", "300Mi", oomKilledStatus(trigger, 1), held, false)
	plan := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", false, qtyBytes(t, "100Mi"), true, []corev1.Pod{pod}, now)
	assert.True(t, plan.UsePublish)
	assert.Equal(t, floor, plan.PublishBytes)
	assert.Contains(t, plan.WorkloadValue, "count=1")

	expired := oomWireRaw(t, 1, origin, floor, trigger, now.Add(-time.Second), 1)
	after := oomBumpPod("p", "app", "300Mi", oomKilledStatus(trigger, 1), expired, false)
	later := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", false, qtyBytes(t, "100Mi"), true, []corev1.Pod{after}, now)
	assert.Equal(t, qtyBytes(t, "100Mi"), later.PublishBytes)
	assert.Empty(t, later.WorkloadValue)
}

func TestOOMBumpApply_OddBytes(t *testing.T) {
	rec := &attunev1alpha1.ContainerRecommendation{}
	explanation := &attunev1alpha1.ContainerRecommendationExplanation{}
	ok := applyOOMBumpToRecommendation(rec, explanation, oomBumpWorkloadPlan{
		UsePublish:   true,
		PublishBytes: 644245095,
		Note:         true,
	})
	assert.True(t, ok)
	assert.Equal(t, int64(644245095), rec.Recommended.MemoryRequest.Value())
	rounded := parsedQty(t, "615Mi")
	assert.NotEqual(t, rounded.Value(), rec.Recommended.MemoryRequest.Value())
	require.NotNil(t, explanation.Memory)
	assert.Contains(t, explanation.Memory.FinalAdjustment, "oomBump")
}

func TestOOMBumpMaxAllowed(t *testing.T) {
	assert.Nil(t, oomBumpMaxAllowed(nil))
	zero := parsedQty(t, "0")
	got := oomBumpMaxAllowed(&zero)
	require.NotNil(t, got)
	assert.Equal(t, int64(0), *got)
}

func TestOOMBumpPreferPending(t *testing.T) {
	policy := oomWirePolicy("oom-wire-prefer")
	r := NewAttunePolicyReconciler()
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}},
	}
	rec := attunev1alpha1.WorkloadRecommendation{
		Workload:   "api",
		Kind:       "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{Name: "app"}},
	}
	got := preferPendingOOMBumpPods(r, policy, pods, rec)
	assert.True(t, samePodBacking(pods, got))

	fill := oomBumpPodStamp{
		Namespace: "ns", PodName: "b", AnnotationOnly: true,
		Stamp: oomBumpRecord{Count: 1, Origin: 1, Floor: 2, Restart: 1, OOMAt: oomWireNow(), HoldUntil: oomWireNow().Add(time.Hour)},
	}
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", []oomBumpPodStamp{fill}, nil)
	got = preferPendingOOMBumpPods(r, policy, pods, rec)
	assert.True(t, samePodBacking(pods, got))

	real := fill
	real.AnnotationOnly = false
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", []oomBumpPodStamp{real}, nil)
	got = preferPendingOOMBumpPods(r, policy, pods, rec)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].Name)
	assert.Equal(t, "a", got[1].Name)
	assert.False(t, samePodBacking(pods, got))

	one := pods[:1]
	got = preferPendingOOMBumpPods(r, policy, one, rec)
	assert.True(t, samePodBacking(one, got))

	policy.Spec.Memory.OOMBump = nil
	got = preferPendingOOMBumpPods(r, policy, pods, rec)
	assert.True(t, samePodBacking(pods, got))
}

func TestOOMBumpRevertGate(t *testing.T) {
	now := oomWireNow()
	oomAt := now.Add(-time.Minute)
	floor := int64(314572800)
	raw := oomWireRaw(t, 1, qtyBytes(t, "200Mi"), floor, oomAt, now.Add(24*time.Hour), 1)
	pod := oomBumpPod("p", "app", "300Mi", oomKilledStatus(oomAt, 1), raw, false)
	policy := oomWirePolicy("oom-wire-revert")
	r := &AttunePolicyReconciler{}

	record := func(request, limit string) safety.ResizeRecord {
		t.Helper()
		out := safety.ResizeRecord{
			PodName:   pod.Name,
			Namespace: pod.Namespace,
			Container: "app",
			ResizedAt: now.Add(-time.Minute),
			OriginalResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: parsedQty(t, request)},
			},
		}
		if limit != "" {
			out.OriginalResources.Limits = corev1.ResourceList{corev1.ResourceMemory: parsedQty(t, limit)}
		}
		return out
	}

	adjusted, suppress := r.oomBumpRevertGate(context.Background(), policy, &pod, record("150Mi", ""), "oomkill", now)
	assert.True(t, suppress)
	assert.Equal(t, qtyBytes(t, "150Mi"), adjusted.OriginalResources.Requests.Memory().Value())

	adjusted, suppress = r.oomBumpRevertGate(context.Background(), policy, &pod, record("200Mi", ""), "Error", now)
	assert.False(t, suppress)
	assert.Equal(t, int64(209715200), adjusted.OriginalResources.Requests.Memory().Value())

	adjusted, suppress = r.oomBumpRevertGate(context.Background(), policy, &pod, record("150Mi", "200Mi"), "throttle", now)
	assert.False(t, suppress)
	assert.Equal(t, floor, adjusted.OriginalResources.Requests.Memory().Value())
	assert.Equal(t, qtyBytes(t, "200Mi"), adjusted.OriginalResources.Limits.Memory().Value())

	adjusted, suppress = r.oomBumpRevertGate(context.Background(), policy, &pod, record("150Mi", "200Mi"), "slo:latency", now)
	assert.False(t, suppress)
	assert.Equal(t, floor, adjusted.OriginalResources.Requests.Memory().Value())
	assert.Equal(t, qtyBytes(t, "200Mi"), adjusted.OriginalResources.Limits.Memory().Value())

	both := policy.DeepCopy()
	ral := attunev1alpha1.ControlledRequestsAndLimits
	both.Spec.Memory.ControlledValues = &ral
	adjusted, suppress = r.oomBumpRevertGate(context.Background(), both, &pod, record("150Mi", "200Mi"), "throttle", now)
	assert.False(t, suppress)
	assert.Equal(t, floor, adjusted.OriginalResources.Requests.Memory().Value())
	assert.Equal(t, floor, adjusted.OriginalResources.Limits.Memory().Value())

	noLimit, suppress := r.oomBumpRevertGate(context.Background(), policy, &pod, record("150Mi", ""), "throttle", now)
	assert.False(t, suppress)
	assert.Equal(t, floor, noLimit.OriginalResources.Requests.Memory().Value())
	assert.Nil(t, noLimit.OriginalResources.Limits)

	off := newTestPolicy("oom-wire-revert-off", "ns")
	adjusted, suppress = r.oomBumpRevertGate(context.Background(), off, &pod, record("150Mi", "200Mi"), "oomkill", now)
	assert.False(t, suppress)
	assert.Equal(t, qtyBytes(t, "150Mi"), adjusted.OriginalResources.Requests.Memory().Value())
	assert.Equal(t, qtyBytes(t, "200Mi"), adjusted.OriginalResources.Limits.Memory().Value())
}

func TestOOMBumpRevertGate_ContainerMode(t *testing.T) {
	now := oomWireNow()
	oomAt := now.Add(-time.Minute)
	floor := int64(314572800)
	raw := oomWireRaw(t, 1, qtyBytes(t, "200Mi"), floor, oomAt, now.Add(24*time.Hour), 1)
	ral := attunev1alpha1.ControlledRequestsAndLimits
	only := attunev1alpha1.ControlledRequestsOnly
	mem := func(cv *string) *attunev1alpha1.ResourceConfig {
		return &attunev1alpha1.ResourceConfig{ControlledValues: cv}
	}
	entry := func(name string, cv *string) attunev1alpha1.ContainerResourcePolicy {
		return attunev1alpha1.ContainerResourcePolicy{ContainerName: name, Memory: mem(cv)}
	}
	star := attunev1alpha1.ContainerPolicyWildcard

	cases := []struct {
		name      string
		policyCV  *string
		policies  []attunev1alpha1.ContainerResourcePolicy
		container string
		limit     string
		wantLimit string
	}{
		{name: "policy requests and limits", policyCV: &ral, container: "app", limit: "200Mi", wantLimit: "floor"},
		{name: "policy requests only", policyCV: &only, container: "app", limit: "200Mi", wantLimit: "200Mi"},
		{name: "policy mode omitted", container: "app", limit: "200Mi", wantLimit: "200Mi"},
		{
			name: "named container requests and limits", policyCV: &only,
			policies:  []attunev1alpha1.ContainerResourcePolicy{entry("app", &ral)},
			container: "app", limit: "200Mi", wantLimit: "floor",
		},
		{
			name: "sibling keeps policy requests only", policyCV: &only,
			policies:  []attunev1alpha1.ContainerResourcePolicy{entry("app", &ral)},
			container: "sidecar", limit: "200Mi", wantLimit: "200Mi",
		},
		{
			name: "named requests only beats policy requests and limits", policyCV: &ral,
			policies:  []attunev1alpha1.ContainerResourcePolicy{entry("sidecar", &only)},
			container: "sidecar", limit: "200Mi", wantLimit: "200Mi",
		},
		{
			name: "unnamed sibling follows policy requests and limits", policyCV: &ral,
			policies:  []attunev1alpha1.ContainerResourcePolicy{entry("sidecar", &only)},
			container: "app", limit: "200Mi", wantLimit: "floor",
		},
		{
			name: "exact name beats star", policyCV: &only,
			policies: []attunev1alpha1.ContainerResourcePolicy{
				entry(star, &ral),
				entry("sidecar", &only),
			},
			container: "sidecar", limit: "200Mi", wantLimit: "200Mi",
		},
		{
			name: "unnamed sibling follows star", policyCV: &only,
			policies: []attunev1alpha1.ContainerResourcePolicy{
				entry(star, &ral),
				entry("sidecar", &only),
			},
			container: "app", limit: "200Mi", wantLimit: "floor",
		},
		{
			name: "star applies when the named entry omits mode", policyCV: &only,
			policies: []attunev1alpha1.ContainerResourcePolicy{
				entry(star, &ral),
				entry("app", nil),
			},
			container: "app", limit: "200Mi", wantLimit: "floor",
		},
		{name: "zero limit stays on requests and limits", policyCV: &ral, container: "app", limit: "0", wantLimit: "0"},
		{name: "zero limit stays on requests only", policyCV: &only, container: "app", limit: "0", wantLimit: "0"},
		{name: "absent limit stays absent", policyCV: &ral, container: "app", limit: "", wantLimit: "absent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := oomWirePolicy("oom-wire-revert-mode")
			policy.Spec.Memory.ControlledValues = tc.policyCV
			policy.Spec.ContainerPolicies = tc.policies
			pod := oomBumpPod("p", tc.container, "300Mi", oomKilledStatus(oomAt, 1), raw, false)
			rec := safety.ResizeRecord{
				PodName: pod.Name, Namespace: pod.Namespace, Container: tc.container,
				ResizedAt: now.Add(-time.Minute),
				OriginalResources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: parsedQty(t, "150Mi")},
				},
			}
			if tc.limit != "" {
				rec.OriginalResources.Limits = corev1.ResourceList{corev1.ResourceMemory: parsedQty(t, tc.limit)}
			}
			adjusted, suppress := (&AttunePolicyReconciler{}).oomBumpRevertGate(
				context.Background(), policy, &pod, rec, "throttle", now)
			assert.False(t, suppress)
			assert.Equal(t, floor, adjusted.OriginalResources.Requests.Memory().Value(), "request floor")
			switch tc.wantLimit {
			case "absent":
				assert.Nil(t, adjusted.OriginalResources.Limits)
			case "floor":
				require.NotNil(t, adjusted.OriginalResources.Limits)
				assert.Equal(t, floor, adjusted.OriginalResources.Limits.Memory().Value())
			case "0":
				require.NotNil(t, adjusted.OriginalResources.Limits)
				assert.True(t, adjusted.OriginalResources.Limits.Memory().IsZero())
			default:
				require.NotNil(t, adjusted.OriginalResources.Limits)
				assert.Equal(t, qtyBytes(t, tc.wantLimit), adjusted.OriginalResources.Limits.Memory().Value())
			}
		})
	}
}

func TestOOMBumpRevertGate_NilBumpDoesNotGetPod(t *testing.T) {
	now := oomWireNow()
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now, 1), "", false)
	cs := kubefake.NewSimpleClientset(&pod)
	r := &AttunePolicyReconciler{Clientset: cs}
	off := newTestPolicy("oom-wire-revert-off", "ns")
	_, _ = r.oomBumpRevertGate(context.Background(), off, &pod, safety.ResizeRecord{
		PodName: pod.Name, Namespace: pod.Namespace, Container: "app",
	}, "oomkill", now)
	for _, action := range cs.Actions() {
		assert.NotEqual(t, "get", action.GetVerb())
	}
}

func TestOOMBumpBook_DropKeepsBase(t *testing.T) {
	now := oomWireNow()
	hold := now.Add(time.Hour)
	origin := qtyBytes(t, "200Mi")
	floor := qtyBytes(t, "300Mi")
	base := oomBumpRecord{Count: 1, Origin: origin, Floor: floor, OOMAt: now.Add(-time.Hour), Restart: 1, HoldUntil: hold}
	higher := oomBumpRecord{Count: 2, Origin: origin, Floor: floor, OOMAt: now, Restart: 2, HoldUntil: hold}
	book := newOOMBumpPending()
	book.Put("uid", "ns", "pol", "ns", "api", "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p", Stamp: higher,
	}}, []oomBumpRecord{base})
	_, ok := book.DropPod("uid", "ns", "pol", "ns", "api", "Deployment", "app", "ns", "p")
	require.True(t, ok)
	applied, held := book.Applied("uid", "ns", "pol", "ns", "api", "Deployment", "app")
	assert.Empty(t, applied)
	require.Len(t, held, 1)
	assert.Equal(t, 1, held[0].Count)
	value := heldWorkloadValue(held, applied, now)
	assert.Contains(t, value, "count=1")
	assert.NotContains(t, value, "count=2")
}

func TestOOMBumpPendingKeySeparatesKind(t *testing.T) {
	book := newOOMBumpPending()
	book.Put("uid", "ns", "pol", "ns", "api", "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p",
	}}, nil)
	book.Put("uid", "ns", "pol", "ns", "api", "Rollout", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "other",
	}}, nil)
	got, ok := book.PeekPod("uid", "ns", "pol", "ns", "api", "Deployment", "app", "ns", "p")
	require.True(t, ok)
	assert.Equal(t, "p", got.PodName)
	_, ok = book.PeekPod("uid", "ns", "pol", "ns", "api", "Rollout", "app", "ns", "p")
	assert.False(t, ok)
	_, ok = book.PeekPod("uid", "ns", "pol", "ns", "api", "Rollout", "app", "ns", "other")
	assert.True(t, ok)
}

func TestOOMBumpStore_EmptyKeepsAnnotation(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-keep-ann")
	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	kept := oomWireRaw(t, 1, qtyBytes(t, "200Mi"), qtyBytes(t, "300Mi"), now.Add(-48*time.Hour), now.Add(-time.Hour), 1)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "api", Namespace: "ns", Annotations: map[string]string{key: kept},
	}}
	r.Client = fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(dep).Build()
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "gone",
		Stamp: oomBumpRecord{
			Count: 2, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
			OOMAt: now, Restart: 2, HoldUntil: now.Add(time.Hour),
		},
	}}, []oomBumpRecord{{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: now.Add(-48 * time.Hour), Restart: 1, HoldUntil: now.Add(-time.Hour),
	}})
	_, dropped := r.oomBumps.DropPod(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", "ns", "gone")
	require.True(t, dropped)
	r.storeAppliedOOMBumps(context.Background(), policy, dep, attunev1alpha1.WorkloadRecommendation{
		Workload:   "api",
		Containers: []attunev1alpha1.ContainerRecommendation{{Name: "app"}},
	})
	var got appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "api"}, &got))
	assert.Equal(t, kept, got.Annotations[key])
}

func TestOOMBumpStore_WritesMarkedCount(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-write-ann")
	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	previous := oomWireRaw(t, 1, qtyBytes(t, "200Mi"), qtyBytes(t, "300Mi"), now.Add(-time.Hour), now.Add(time.Hour), 1)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "api", Namespace: "ns", Annotations: map[string]string{key: previous},
	}}
	r.Client = fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(dep).Build()
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p",
		Stamp: oomBumpRecord{
			Count: 2, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
			OOMAt: now, Restart: 2, HoldUntil: now.Add(time.Hour),
		},
	}}, nil)
	r.oomBumps.MarkApplied(string(policy.UID), policy.Namespace, policy.Name, "ns", "api", "Deployment", "app", "ns", "p")
	var current appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "api"}, &current))
	r.storeAppliedOOMBumps(context.Background(), policy, &current, attunev1alpha1.WorkloadRecommendation{
		Workload:   "api",
		Containers: []attunev1alpha1.ContainerRecommendation{{Name: "app"}},
	})
	var got appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "api"}, &got))
	assert.Contains(t, got.Annotations[key], "count=2")
}

func TestOOMBumpFinishStored(t *testing.T) {
	now := oomWireNow()
	r := NewAttunePolicyReconciler()
	wl := oomWireDeploy("api")
	pod := oomBumpPod("p", "app", "300Mi", nil, "", false)
	record := oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: now, Restart: 1, HoldUntil: now.Add(time.Hour),
	}

	appliedPolicy := oomWirePolicy("oom-wire-finish-applied")
	r.oomBumps.Put(string(appliedPolicy.UID), appliedPolicy.Namespace, appliedPolicy.Name, wl.Namespace, wl.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p", Stamp: record,
	}}, nil)
	beforeApplied := oomMetric(appliedPolicy.Name, oomBumpApplied)
	r.finishStoredOOMBump(appliedPolicy, wl, "app", &pod)
	assert.Equal(t, beforeApplied+1, oomMetric(appliedPolicy.Name, oomBumpApplied))

	fillPolicy := oomWirePolicy("oom-wire-finish-fill")
	r.oomBumps.Put(string(fillPolicy.UID), fillPolicy.Namespace, fillPolicy.Name, wl.Namespace, wl.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p", Stamp: record, AnnotationOnly: true, Result: oomBumpApplied,
	}}, nil)
	beforeFill := oomMetric(fillPolicy.Name, oomBumpApplied)
	r.finishStoredOOMBump(fillPolicy, wl, "app", &pod)
	assert.Equal(t, beforeFill, oomMetric(fillPolicy.Name, oomBumpApplied))

	clampPolicy := oomWirePolicy("oom-wire-finish-clamp")
	r.oomBumps.Put(string(clampPolicy.UID), clampPolicy.Namespace, clampPolicy.Name, wl.Namespace, wl.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "p", Stamp: record, Result: oomBumpClamped, Event: "OOMBumpClamped",
	}}, nil)
	beforeClamp := oomMetric(clampPolicy.Name, oomBumpClamped)
	beforeClampApplied := oomMetric(clampPolicy.Name, oomBumpApplied)
	r.finishStoredOOMBump(clampPolicy, wl, "app", &pod)
	assert.Equal(t, beforeClamp+1, oomMetric(clampPolicy.Name, oomBumpClamped))
	assert.Equal(t, beforeClampApplied, oomMetric(clampPolicy.Name, oomBumpApplied))
}

func TestSettleUnchangedOOMBump(t *testing.T) {
	now := oomWireNow()
	record := oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: now, Restart: 1, HoldUntil: now.Add(time.Hour),
	}
	target, err := resource.ParseQuantity("300Mi")
	require.NoError(t, err)
	higher, err := resource.ParseQuantity("400Mi")
	require.NoError(t, err)
	same := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: target}}
	raised := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: higher}}

	realPolicy := oomWirePolicy("oom-wire-settle-real")
	realPod := oomBumpPod("real", "app", "300Mi", nil, "", false)
	realWL := oomWireDeploy("api")
	real := NewAttunePolicyReconciler()
	real.oomBumps.Put(string(realPolicy.UID), realPolicy.Namespace, realPolicy.Name, realWL.Namespace, realWL.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: realPod.Namespace, PodName: realPod.Name, Stamp: record, Result: oomBumpApplied,
	}}, nil)
	beforeReal := oomMetric(realPolicy.Name, oomBumpSkipped)
	real.settleUnchangedOOMBump(context.Background(), realPolicy, &realPod, realWL, "app", same)
	_, still := real.peekOOMBump(realPolicy, realWL, "app", &realPod)
	assert.False(t, still)
	assert.Equal(t, beforeReal+1, oomMetric(realPolicy.Name, oomBumpSkipped))

	raisePolicy := oomWirePolicy("oom-wire-settle-raise")
	raisePod := oomBumpPod("raise", "app", "300Mi", nil, "", false)
	raiseWL := oomWireDeploy("api")
	raising := NewAttunePolicyReconciler()
	raising.oomBumps.Put(string(raisePolicy.UID), raisePolicy.Namespace, raisePolicy.Name, raiseWL.Namespace, raiseWL.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: raisePod.Namespace, PodName: raisePod.Name, Stamp: record, Result: oomBumpApplied,
	}}, nil)
	beforeRaise := oomMetric(raisePolicy.Name, oomBumpSkipped)
	raising.settleUnchangedOOMBump(context.Background(), raisePolicy, &raisePod, raiseWL, "app", raised)
	_, kept := raising.peekOOMBump(raisePolicy, raiseWL, "app", &raisePod)
	assert.True(t, kept)
	assert.Equal(t, beforeRaise, oomMetric(raisePolicy.Name, oomBumpSkipped))

	fillPolicy := oomWirePolicy("oom-wire-settle-fill")
	raw, err := formatOOMBumpRecord(record)
	require.NoError(t, err)
	fillPod := oomBumpPod("fill", "app", "300Mi", nil, raw, false)
	fillWL := oomWireDeploy("api")
	fill := NewAttunePolicyReconciler()
	fill.Clientset = kubefake.NewSimpleClientset(fillPod.DeepCopy())
	fill.oomBumps.Put(string(fillPolicy.UID), fillPolicy.Namespace, fillPolicy.Name, fillWL.Namespace, fillWL.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: fillPod.Namespace, PodName: fillPod.Name, Stamp: record, AnnotationOnly: true,
	}}, nil)
	beforeFill := oomMetric(fillPolicy.Name, oomBumpSkipped)
	fill.settleUnchangedOOMBump(context.Background(), fillPolicy, &fillPod, fillWL, "app", same)
	stamp, ok := fill.peekOOMBump(fillPolicy, fillWL, "app", &fillPod)
	require.True(t, ok)
	assert.True(t, stamp.AnnotationOnly)
	assert.True(t, stamp.applied)
	assert.Equal(t, beforeFill, oomMetric(fillPolicy.Name, oomBumpSkipped))
}

func TestPlanContainerOOMBump_ContainerMaxCaps(t *testing.T) {
	now := oomWireNow()
	policy := oomWirePolicy("oom-wire-container-max")
	policy.Spec.Memory.MaxAllowed = nil
	maxAllowed := parsedQty(t, "256Mi")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "app",
		Memory:        &attunev1alpha1.ResourceConfig{MaxAllowed: &maxAllowed},
	}}
	r := NewAttunePolicyReconciler()
	pod := oomBumpPod("p", "app", "200Mi", oomKilledStatus(now.Add(-time.Minute), 1), "", false)
	plan := r.planContainerOOMBump(context.Background(), policy, oomWireDeploy("api"), "app", false, 0, false, []corev1.Pod{pod}, now)
	assert.True(t, plan.UsePublish)
	assert.Equal(t, maxAllowed.Value(), plan.PublishBytes)
	require.NotEmpty(t, plan.Stamps)
	assert.Equal(t, oomBumpClamped, plan.Stamps[0].Result)
	assert.NotContains(t, plan.MetricNow, oomBumpClamped)
}

func TestClearOOMBumpAfterFullRevert(t *testing.T) {
	now := oomWireNow()
	oomAt := now.Add(-time.Minute)
	raw := oomWireRaw(t, 1, qtyBytes(t, "200Mi"), qtyBytes(t, "300Mi"), oomAt, now.Add(time.Hour), 1)
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	ctx := context.Background()

	policy := oomWirePolicy("oom-wire-clear-one")
	pod := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	deploy := oomWireDeploy("api")
	deploy.Annotations = map[string]string{key: raw}
	scheme := testScheme()
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy(), deploy.DeepCopy()).Build()
	r.Scheme = scheme
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: pod.Namespace, PodName: pod.Name, Stamp: oomBumpRecord{
			Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
			OOMAt: oomAt, Restart: 1, HoldUntil: now.Add(time.Hour),
		}, Result: oomBumpApplied,
	}}, nil)
	before := oomMetric(policy.Name, oomBumpSkipped)
	r.clearOOMBumpAfterFullRevert(ctx, policy, deploy, &pod, "app", nil, true)
	assert.Equal(t, raw, pod.Annotations[key])
	assert.Equal(t, before, oomMetric(policy.Name, oomBumpSkipped))
	_, still := r.peekOOMBump(policy, deploy, "app", &pod)
	assert.False(t, still)

	var gotPod corev1.Pod
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gotPod))
	_, podKey := gotPod.Annotations[key]
	assert.False(t, podKey)
	var gotDeploy appsv1.Deployment
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: deploy.Namespace, Name: deploy.Name}, &gotDeploy))
	_, workloadKey := gotDeploy.Annotations[key]
	assert.False(t, workloadKey)

	plan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{pod}, now)
	assert.Empty(t, plan.Stamps)
	assert.False(t, plan.UsePublish)
	assert.Equal(t, before, oomMetric(policy.Name, oomBumpSkipped))
	r.oomBumps.ResetUID(string(policy.UID))
	afterReset := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{pod}, now)
	assert.Empty(t, afterReset.Stamps)
	assert.False(t, afterReset.UsePublish)
	bare := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)
	barePlan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{bare}, now)
	assert.Empty(t, barePlan.Stamps)
	assert.False(t, barePlan.UsePublish)

	sibPolicy := oomWirePolicy("oom-wire-clear-sibling")
	reverted := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	sibling := oomBumpPod("b", "app", "300Mi", nil, raw, false)
	sibDeploy := oomWireDeploy("api")
	sibDeploy.Annotations = map[string]string{key: raw}
	sib := NewAttunePolicyReconciler()
	sib.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(reverted.DeepCopy(), sibling.DeepCopy(), sibDeploy.DeepCopy()).Build()
	sib.Scheme = scheme
	sib.clearOOMBumpAfterFullRevert(ctx, sibPolicy, sibDeploy, &reverted, "app", []corev1.Pod{reverted, sibling}, true)
	var sibPod corev1.Pod
	require.NoError(t, sib.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "a"}, &sibPod))
	_, clearedKey := sibPod.Annotations[key]
	assert.False(t, clearedKey)
	var sibWL appsv1.Deployment
	require.NoError(t, sib.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "api"}, &sibWL))
	assert.Equal(t, raw, sibWL.Annotations[key])
	var sibKeep corev1.Pod
	require.NoError(t, sib.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "b"}, &sibKeep))
	assert.Equal(t, raw, sibKeep.Annotations[key])
}

func TestConsumedOOMSignalSurvivesResetUID(t *testing.T) {
	now := oomWireNow()
	oomAt := now.Add(-time.Minute)
	later := now
	origin := qtyBytes(t, "200Mi")
	floor := qtyBytes(t, "300Mi")
	raw := oomWireRaw(t, 1, origin, floor, oomAt, now.Add(time.Hour), 1)
	ctx := context.Background()
	policy := oomWirePolicy("oom-wire-reset-consumed")
	deploy := oomWireDeploy("api")
	r := NewAttunePolicyReconciler()
	clear := oomBumpClear{
		policyNamespace: policy.Namespace,
		policyName:      policy.Name,
		namespace:       "ns",
		podName:         "a",
		container:       "app",
		raw:             raw,
	}
	r.oomBumps.NoteClear(string(policy.UID), clear)
	r.oomBumps.NoteClear(string(policy.UID), clear)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)

	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "a", Stamp: oomBumpRecord{
			Count: 1, Origin: origin, Floor: floor, OOMAt: oomAt, Restart: 1, HoldUntil: now.Add(time.Hour),
		},
	}}, nil)
	r.oomBumps.ResetUID(string(policy.UID))
	_, still := r.oomBumps.PeekPod(string(policy.UID), policy.Namespace, policy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", "ns", "a")
	assert.False(t, still)
	kept := r.oomBumps.Clears(string(policy.UID))
	require.Len(t, kept, 1)
	assert.True(t, kept[0].oomAt.Equal(oomAt))
	assert.Equal(t, int32(1), kept[0].restart)

	stale := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	stalePlan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{stale}, now)
	assert.Empty(t, stalePlan.Stamps)
	assert.False(t, stalePlan.UsePublish)

	gone := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)
	gonePlan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{gone}, now)
	assert.Empty(t, gonePlan.Stamps)
	assert.False(t, gonePlan.UsePublish)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)

	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	fresh := oomBumpPod("a", "app", "200Mi", oomKilledStatus(later, 2), "", false)
	next := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{fresh}, now)
	require.Len(t, next.Stamps, 1)
	assert.Equal(t, 1, next.Stamps[0].Stamp.Count)
	assert.Equal(t, origin, next.Stamps[0].Stamp.Origin)
	assert.Equal(t, floor, next.Stamps[0].Stamp.Floor)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)

	r.oomBumps.ResetUID(string(policy.UID))
	again := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{fresh}, now)
	require.Len(t, again.Stamps, 1)
	assert.Equal(t, 1, again.Stamps[0].Stamp.Count)
	assert.Equal(t, origin, again.Stamps[0].Stamp.Origin)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)
	r.finishStoredOOMBump(policy, deploy, "app", &fresh)
	assert.Empty(t, r.oomBumps.Clears(string(policy.UID)))

	zeroRaw := oomWireRaw(t, 1, origin, floor, oomAt, now.Add(time.Hour), 2)
	r.oomBumps.NoteClear(string(policy.UID), oomBumpClear{
		policyNamespace: policy.Namespace,
		policyName:      policy.Name,
		namespace:       "ns",
		podName:         "z",
		container:       "app",
		raw:             zeroRaw,
	})
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)
	zeroStatus := oomKilledStatus(time.Time{}, 3)
	zeroPod := oomBumpPod("z", "app", "200Mi", zeroStatus, "", false)
	zeroPlan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{zeroPod}, now)
	require.Len(t, zeroPlan.Stamps, 1)
	assert.Equal(t, 1, zeroPlan.Stamps[0].Stamp.Count)
	assert.Equal(t, origin, zeroPlan.Stamps[0].Stamp.Origin)
	r.finishStoredOOMBump(policy, deploy, "app", &zeroPod)
	assert.Empty(t, r.oomBumps.Clears(string(policy.UID)))

	older := oomBumpClear{
		policyNamespace: policy.Namespace, policyName: policy.Name,
		namespace: "ns", podName: "q", container: "app", raw: raw,
	}
	newerRaw := oomWireRaw(t, 1, origin, floor, later, now.Add(time.Hour), 2)
	newer := older
	newer.raw = newerRaw
	r.oomBumps.NoteClear(string(policy.UID), newer)
	r.oomBumps.NoteClear(string(policy.UID), older)
	replaced := r.oomBumps.Clears(string(policy.UID))
	require.Len(t, replaced, 1)
	assert.True(t, replaced[0].oomAt.Equal(later))
	assert.Equal(t, int32(2), replaced[0].restart)

	r.oomBumps.ForgetPolicy(policy.Namespace, policy.Name)
	assert.Empty(t, r.oomBumps.Clears(string(policy.UID)))
}

func TestPlanContainerOOMBump_UnstoredClearKeepsFreshStart(t *testing.T) {
	now := oomWireNow()
	consumedAt := now.Add(-2 * time.Hour)
	beforeSibling := now.Add(-30 * time.Minute)
	siblingAt := now.Add(-time.Minute)
	hold := now.Add(time.Hour)
	live100 := qtyBytes(t, "100Mi")
	bump100 := qtyBytes(t, "200Mi")
	origin512 := qtyBytes(t, "512Mi")
	siblingFloor := int64(773094114)
	ctx := context.Background()

	policy := oomWirePolicy("oom-wire-unstored-clear")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	raw := oomWireRaw(t, 2, origin512, siblingFloor, siblingAt, hold, 2)
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	deploy := oomWireDeploy("api")
	deploy.Annotations = map[string]string{key: raw}

	reverted := oomBumpPod("reverted", "app", "100Mi", oomKilledStatus(beforeSibling, 3), "", false)
	sibling := oomBumpPod("sib", "app", "512Mi", nil, raw, false)
	consumedRaw := oomWireRaw(t, 1, live100, bump100, consumedAt, hold, 1)
	r := NewAttunePolicyReconciler()
	r.oomBumps.NoteClear(string(policy.UID), oomBumpClear{
		policyNamespace: policy.Namespace,
		policyName:      policy.Name,
		namespace:       reverted.Namespace,
		podName:         reverted.Name,
		container:       "app",
		raw:             consumedRaw,
	})

	first := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{reverted, sibling}, now)
	require.Len(t, first.Stamps, 1)
	assert.Equal(t, "reverted", first.Stamps[0].PodName)
	assert.Equal(t, 1, first.Stamps[0].Stamp.Count)
	assert.Equal(t, live100, first.Stamps[0].Stamp.Origin)
	assert.Equal(t, bump100, first.Stamps[0].bumpBytes)
	assert.Equal(t, siblingFloor, first.Stamps[0].Stamp.Floor)
	assert.Equal(t, siblingFloor, first.PublishBytes)
	work, wok := parseOOMBumpRecord(first.WorkloadValue)
	require.True(t, wok)
	assert.Equal(t, 2, work.Count)
	assert.Equal(t, origin512, work.Origin)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)

	r.oomBumps.ResetUID(string(policy.UID))
	second := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{reverted, sibling}, now)
	require.Len(t, second.Stamps, 1)
	assert.Equal(t, 1, second.Stamps[0].Stamp.Count)
	assert.Equal(t, live100, second.Stamps[0].Stamp.Origin)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)

	r.oomBumps.ResetUID(string(policy.UID))
	laterPod := reverted.DeepCopy()
	laterStatus := *oomKilledStatus(now, 4)
	laterStatus.Name = "app"
	laterPod.Status.ContainerStatuses[0] = laterStatus
	third := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{*laterPod, sibling}, now)
	require.Len(t, third.Stamps, 1)
	assert.Equal(t, 1, third.Stamps[0].Stamp.Count)
	assert.Equal(t, live100, third.Stamps[0].Stamp.Origin)
	require.Len(t, r.oomBumps.Clears(string(policy.UID)), 1)
	r.finishStoredOOMBump(policy, deploy, "app", laterPod)
	assert.Empty(t, r.oomBumps.Clears(string(policy.UID)))
}

func TestDropOOMBumpStampKeepsAnnotationOnly(t *testing.T) {
	now := oomWireNow()
	record := oomBumpRecord{
		Count: 3, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: now, Restart: 4, HoldUntil: now.Add(24 * time.Hour),
	}
	deploy := oomWireDeploy("api")

	policy := oomWirePolicy("oom-wire-drop-annonly")
	r := NewAttunePolicyReconciler()
	r.oomBumps.Put(string(policy.UID), policy.Namespace, policy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "a", Stamp: record, AnnotationOnly: true,
	}}, nil)
	before := oomMetric(policy.Name, oomBumpSkipped)
	r.dropOOMBumpStamp(policy, deploy, "app", "ns", "a", true)
	stamp, ok := r.oomBumps.PeekPod(string(policy.UID), policy.Namespace, policy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", "ns", "a")
	require.True(t, ok)
	assert.True(t, stamp.AnnotationOnly)
	assert.Equal(t, before, oomMetric(policy.Name, oomBumpSkipped))

	realPolicy := oomWirePolicy("oom-wire-drop-real")
	real := NewAttunePolicyReconciler()
	real.oomBumps.Put(string(realPolicy.UID), realPolicy.Namespace, realPolicy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", []oomBumpPodStamp{{
		Namespace: "ns", PodName: "a", Stamp: record, Result: oomBumpApplied,
	}}, nil)
	beforeReal := oomMetric(realPolicy.Name, oomBumpSkipped)
	real.dropOOMBumpStamp(realPolicy, deploy, "app", "ns", "a", true)
	_, still := real.oomBumps.PeekPod(string(realPolicy.UID), realPolicy.Namespace, realPolicy.Name, deploy.Namespace, deploy.Name, "Deployment", "app", "ns", "a")
	assert.False(t, still)
	assert.Equal(t, beforeReal+1, oomMetric(realPolicy.Name, oomBumpSkipped))
}

func TestMaybeClearOOMBumpAfterRevertUsesLivePod(t *testing.T) {
	now := oomWireNow()
	oomAt := now.Add(-time.Minute)
	origin := qtyBytes(t, "200Mi")
	floor := qtyBytes(t, "300Mi")
	raw := oomWireRaw(t, 1, origin, floor, oomAt, now.Add(time.Hour), 1)
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	ctx := context.Background()
	scheme := testScheme()
	record := safety.ResizeRecord{
		PodName: "live-a", Namespace: "ns", Container: "app",
		ResizedAt: now.Add(-time.Minute), WorkloadName: "api",
	}

	policy := oomWirePolicy("oom-wire-live-clear")
	listed := oomBumpPod("live-a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)
	live := oomBumpPod("live-a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	deploy := oomWireDeploy("api")
	r := NewAttunePolicyReconciler()
	r.SetNowFunc(func() time.Time { return now })
	r.Scheme = scheme
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(live.DeepCopy(), deploy).Build()
	r.Clientset = kubefake.NewSimpleClientset(live.DeepCopy())
	r.maybeClearOOMBumpAfterRevert(ctx, policy, []client.Object{deploy}, &listed, []corev1.Pod{listed}, record, "Error", deploy.Name)

	assert.Nil(t, listed.Annotations)
	var got corev1.Pod
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "live-a"}, &got))
	_, podKey := got.Annotations[key]
	assert.False(t, podKey)
	clears := r.oomBumps.Clears(string(policy.UID))
	require.Len(t, clears, 1)
	assert.Equal(t, raw, clears[0].raw)
	assert.True(t, clears[0].oomAt.Equal(oomAt))
	assert.Equal(t, int32(1), clears[0].restart)

	stale := oomBumpPod("live-a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	plan := r.planContainerOOMBump(ctx, policy, deploy, "app", false, 0, false, []corev1.Pod{stale}, now)
	assert.Empty(t, plan.Stamps)
	assert.False(t, plan.UsePublish)

	cachePolicy := oomWirePolicy("oom-wire-cache-clear")
	cached := oomBumpPod("cache-a", "app", "200Mi", oomKilledStatus(oomAt, 1), raw, false)
	apiPod := oomBumpPod("cache-a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)
	cacheDeploy := oomWireDeploy("api")
	cache := NewAttunePolicyReconciler()
	cache.SetNowFunc(func() time.Time { return now })
	cache.Scheme = scheme
	cache.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cached.DeepCopy(), cacheDeploy).Build()
	cache.Clientset = kubefake.NewSimpleClientset(apiPod.DeepCopy())
	cacheRecord := record
	cacheRecord.PodName = "cache-a"
	cache.maybeClearOOMBumpAfterRevert(ctx, cachePolicy, []client.Object{cacheDeploy}, &cached, []corev1.Pod{cached}, cacheRecord, "Error", cacheDeploy.Name)
	assert.Equal(t, raw, cached.Annotations[key])
	var cacheGot corev1.Pod
	require.NoError(t, cache.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "cache-a"}, &cacheGot))
	_, cacheKey := cacheGot.Annotations[key]
	assert.False(t, cacheKey)
	cacheClears := cache.oomBumps.Clears(string(cachePolicy.UID))
	require.Len(t, cacheClears, 1)
	assert.Equal(t, raw, cacheClears[0].raw)
}
