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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

func parseQty(t *testing.T, raw string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return q
}

func requestsAndLimitsPolicy(workload string) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("limit-mult", "default")
	policy.Spec.TargetRef.Name = stringPtr(workload)
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &both
	policy.Spec.Memory.ControlledValues = &both
	return policy
}

func runningPod(t *testing.T, deploy, cpuReq, memReq, cpuLim, memLim string, class corev1.PodQOSClass) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy + "-abc-1",
			Namespace: "default",
			Labels:    map[string]string{"app": deploy},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    parseQty(t, cpuReq),
						corev1.ResourceMemory: parseQty(t, memReq),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    parseQty(t, cpuLim),
						corev1.ResourceMemory: parseQty(t, memLim),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: class},
	}
}

func scaledContainerRec(t *testing.T, policy *attunev1alpha1.AttunePolicy, curCPU, curCPULim, curMem, curMemLim, recCPU, recMem string) attunev1alpha1.ContainerRecommendation {
	t.Helper()
	c := attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			CPURequest:    parseQty(t, curCPU),
			CPULimit:      parseQty(t, curCPULim),
			MemoryRequest: parseQty(t, curMem),
			MemoryLimit:   parseQty(t, curMemLim),
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest:    parseQty(t, recCPU),
			MemoryRequest: parseQty(t, recMem),
		},
	}
	scaleControlledLimits(policy, &c, c.Current.CPURequest, c.Current.CPULimit, c.Current.MemoryRequest, c.Current.MemoryLimit)
	return c
}

func workloadRec(name string, c attunev1alpha1.ContainerRecommendation) attunev1alpha1.WorkloadRecommendation {
	return attunev1alpha1.WorkloadRecommendation{
		Workload:   name,
		Kind:       "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{c},
	}
}

func resizeHarness(pod *corev1.Pod, deploy *appsv1.Deployment) *AttunePolicyReconciler {
	r, _ := newResizeReconciler(pod, deploy)
	r.Recorder = events.NewFakeRecorder(8)
	return r
}

func assertNoResizeOrEvict(t *testing.T, r *AttunePolicyReconciler) {
	t.Helper()
	cs := r.Clientset.(*kubefake.Clientset)
	for _, action := range cs.Actions() {
		require.NotEqual(t, "resize", action.GetSubresource(), "QoS skip must not call UpdateResize")
		require.NotEqual(t, "eviction", action.GetSubresource(), "QoS skip must not evict")
	}
}

func resizedPodFromClientset(t *testing.T, r *AttunePolicyReconciler) *corev1.Pod {
	t.Helper()
	cs := r.Clientset.(*kubefake.Clientset)
	var got *corev1.Pod
	for _, action := range cs.Actions() {
		require.NotEqual(t, "eviction", action.GetSubresource(), "in-place resize must not evict")
		if action.GetSubresource() != "resize" {
			continue
		}
		pod, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		require.True(t, ok)
		got = pod
	}
	require.NotNil(t, got, "expected UpdateResize")
	return got
}

func TestLimitMultiplierRatio(t *testing.T) {
	t.Parallel()
	assert.Nil(t, limitMultiplierRatio(nil))
	empty := ""
	assert.Nil(t, limitMultiplierRatio(&empty))
	for _, raw := range []string{"NaN", "Inf", "+Inf", "-Inf", "0", "-1", "0.5", "101", "nope"} {
		v := raw
		assert.Nil(t, limitMultiplierRatio(&v), raw)
	}
	one := "1"
	got := limitMultiplierRatio(&one)
	require.NotNil(t, got)
	assert.InDelta(t, 1, *got, 0.0001)
	two := "2"
	got = limitMultiplierRatio(&two)
	require.NotNil(t, got)
	assert.InDelta(t, 2, *got, 0.0001)
	hundred := "100"
	got = limitMultiplierRatio(&hundred)
	require.NotNil(t, got)
	assert.InDelta(t, 100, *got, 0.0001)
}

func TestLimitMultiplierRequestsOnlyConflict_AfterBuiltInDefaults(t *testing.T) {
	t.Parallel()
	two := "2"
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.CPU.LimitMultiplier = &two
	pkgdefaults.ApplyBuiltInDefaults(policy)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.ControlledRequestsOnly, *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.CPU.LimitMultiplier)
	assert.Equal(t, "2", *policy.Spec.CPU.LimitMultiplier)
	err := limitMultiplierRequestsOnlyConflict(policy)
	require.EqualError(t, err, "cpu.limitMultiplier cannot be set when cpu.controlledValues is RequestsOnly")

	both := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &both
	policy.Spec.Memory.ControlledValues = &both
	require.NoError(t, limitMultiplierRequestsOnlyConflict(policy))

	only := attunev1alpha1.ControlledRequestsOnly
	twoStr := "2"
	explicit := &attunev1alpha1.AttunePolicy{}
	explicit.Spec.CPU.ControlledValues = &only
	explicit.Spec.Memory.ControlledValues = &only
	defs := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				LimitMultiplier:  &twoStr,
				ControlledValues: &both,
			},
			Memory: &attunev1alpha1.ResourceConfig{
				LimitMultiplier:  &twoStr,
				ControlledValues: &both,
			},
		},
	}
	pkgdefaults.MergeDefaults(explicit, defs)
	pkgdefaults.ApplyBuiltInDefaults(explicit)
	assert.Nil(t, explicit.Spec.CPU.LimitMultiplier)
	assert.Nil(t, explicit.Spec.Memory.LimitMultiplier)
	require.NoError(t, limitMultiplierRequestsOnlyConflict(explicit))

	omitted := &attunev1alpha1.AttunePolicy{}
	multOnly := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{LimitMultiplier: &twoStr},
		},
	}
	pkgdefaults.MergeDefaults(omitted, multOnly)
	pkgdefaults.ApplyBuiltInDefaults(omitted)
	require.NotNil(t, omitted.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.ControlledRequestsOnly, *omitted.Spec.CPU.ControlledValues)
	require.EqualError(t, limitMultiplierRequestsOnlyConflict(omitted),
		"cpu.limitMultiplier cannot be set when cpu.controlledValues is RequestsOnly")
}

func TestReconcile_RequestsOnlySkipsInheritedLimitMultiplier(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	only := attunev1alpha1.ControlledRequestsOnly
	policy.Spec.CPU.ControlledValues = &only
	policy.Spec.Memory.ControlledValues = &only
	two := "2"
	both := attunev1alpha1.ControlledRequestsAndLimits
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				LimitMultiplier:  &two,
				ControlledValues: &both,
			},
			Memory: &attunev1alpha1.ResourceConfig{
				LimitMultiplier:  &two,
				ControlledValues: &both,
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}
	reconciler, fakeClient := newReconcilerForReconcile(mc, policy, defaults, deploy, pod)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, result.RequeueAfter)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonMonitoring, cond.Reason)
	assert.NotEqual(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
}

func TestScaleLimits_ExplicitMultiplierDoesNotInventZeroLimit(t *testing.T) {
	t.Parallel()
	two := 2.0
	got := scaleLimits(corev1.ResourceCPU, parseQty(t, "250m"), resource.Quantity{}, parseQty(t, "250m"), &two)
	assert.True(t, got.IsZero(), "explicit multiplier must not invent a limit, got %s", got.String())

	got = scaleLimits(corev1.ResourceCPU, resource.Quantity{}, parseQty(t, "500m"), parseQty(t, "250m"), &two)
	assert.True(t, got.IsZero(), "zero current request must not invent a limit, got %s", got.String())
}

func TestScaleControlledLimits_BelowOneKeepsLiveRatio(t *testing.T) {
	t.Parallel()
	policy := requestsAndLimitsPolicy("api")
	half := "0.5"
	policy.Spec.CPU.LimitMultiplier = &half
	policy.Spec.Memory.LimitMultiplier = &half
	// Live CPU ratio is 2 (200m/400m). A 0.5 multiple would set the limit
	// to 100m, and ClampRequestsToLimits would then lower the request.
	c := scaledContainerRec(t, policy, "200m", "400m", "256Mi", "512Mi", "200m", "256Mi")
	assert.True(t, c.Recommended.CPURequest.Equal(parseQty(t, "200m")))
	assert.True(t, c.Recommended.CPULimit.Equal(parseQty(t, "400m")),
		"below 1 must keep the live CPU ratio, got %s", c.Recommended.CPULimit.String())
	assert.True(t, c.Recommended.MemoryRequest.Equal(parseQty(t, "256Mi")))
	assert.True(t, c.Recommended.MemoryLimit.Equal(parseQty(t, "512Mi")),
		"below 1 must keep the live memory ratio, got %s", c.Recommended.MemoryLimit.String())
}

func TestScaleControlledLimits_ExplicitTwo(t *testing.T) {
	t.Parallel()
	policy := requestsAndLimitsPolicy("api")
	two := "2"
	policy.Spec.CPU.LimitMultiplier = &two
	c := scaledContainerRec(t, policy, "250m", "400m", "256Mi", "256Mi", "250m", "256Mi")
	assert.True(t, c.Recommended.CPURequest.Equal(parseQty(t, "250m")))
	assert.True(t, c.Recommended.CPULimit.Equal(parseQty(t, "500m")),
		"explicit 2 on a 1.6 live ratio must be 500m, got %s", c.Recommended.CPULimit.String())
	assert.True(t, c.Recommended.MemoryLimit.Equal(parseQty(t, "256Mi")),
		"omitted memory multiplier keeps the live ratio")
}

func TestScaleControlledLimits_MemoryOnePointFiveBinarySI(t *testing.T) {
	t.Parallel()
	policy := requestsAndLimitsPolicy("api")
	oneFive := "1.5"
	policy.Spec.Memory.LimitMultiplier = &oneFive
	c := scaledContainerRec(t, policy, "100m", "200m", "512Mi", "1Gi", "100m", "512Mi")
	got := c.Recommended.MemoryLimit
	want := parseQty(t, "768Mi")
	assert.True(t, got.Equal(want), "1.5 * 512Mi must be 768Mi, got %s", got.String())
	assert.Equal(t, resource.BinarySI, got.Format)
	round, err := resource.ParseQuantity(got.String())
	require.NoError(t, err)
	assert.True(t, round.Equal(want), "parsed %s from %q", round.String(), got.String())
}

func TestScaleControlledLimits_ExplicitMultiplierLeavesMissingLimitOmitted(t *testing.T) {
	t.Parallel()
	policy := requestsAndLimitsPolicy("api")
	two := "2"
	policy.Spec.CPU.LimitMultiplier = &two
	policy.Spec.Memory.LimitMultiplier = &two
	c := scaledContainerRec(t, policy, "250m", "0", "128Mi", "0", "250m", "128Mi")
	assert.True(t, c.Recommended.CPULimit.IsZero(), "got %s", c.Recommended.CPULimit.String())
	assert.True(t, c.Recommended.MemoryLimit.IsZero(), "got %s", c.Recommended.MemoryLimit.String())
}

func TestComputeRecommendations_RequestsAndLimits_ExplicitTwo(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	both := attunev1alpha1.ControlledRequestsAndLimits
	two := "2"
	policy.Spec.CPU.ControlledValues = &both
	policy.Spec.Memory.ControlledValues = &both
	policy.Spec.CPU.LimitMultiplier = &two
	cpuMin := parseQty(t, "250m")
	cpuMax := parseQty(t, "250m")
	policy.Spec.CPU.MinAllowed = &cpuMin
	policy.Spec.CPU.MaxAllowed = &cpuMax

	deploy := newTestDeployment("api-server", "default", nil)
	container := &deploy.Spec.Template.Spec.Containers[0]
	container.Resources.Requests[corev1.ResourceCPU] = parseQty(t, "250m")
	container.Resources.Limits[corev1.ResourceCPU] = parseQty(t, "400m")

	reconciler := newReconcilerWithClient()
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.05), nil
		},
	}
	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	got := rec.Containers[0]
	assert.True(t, got.Recommended.CPURequest.Equal(parseQty(t, "250m")),
		"engine request got %s", got.Recommended.CPURequest.String())
	assert.True(t, got.Recommended.CPULimit.Equal(parseQty(t, "500m")),
		"explicit 2 must replace live ratio 1.6, got %s", got.Recommended.CPULimit.String())
}

func TestComputeRecommendations_LimitMultiplierMaxAllowedCapsRequest(t *testing.T) {
	// The applied memory request above maxAllowed must stay equal to the
	// multiplied memory limit. Capping only that request would make memory
	// Burstable (issue 970).
	policy := newTestPolicy("test-policy", "default")
	both := attunev1alpha1.ControlledRequestsAndLimits
	two := "2"
	noDecrease := false
	policy.Spec.CPU.ControlledValues = &both
	policy.Spec.Memory.ControlledValues = &both
	policy.Spec.CPU.LimitMultiplier = &two
	policy.Spec.Memory.LimitMultiplier = &two
	policy.Spec.CPU.AllowDecrease = &noDecrease
	cpuMax := parseQty(t, "500m")
	memMax := parseQty(t, "256Mi")
	policy.Spec.CPU.MaxAllowed = &cpuMax
	policy.Spec.Memory.MaxAllowed = &memMax

	deploy := newTestDeployment("api-server", "default", nil)
	container := &deploy.Spec.Template.Spec.Containers[0]
	container.Resources.Requests[corev1.ResourceCPU] = parseQty(t, "500m")
	container.Resources.Limits[corev1.ResourceCPU] = parseQty(t, "500m")
	container.Resources.Requests[corev1.ResourceMemory] = parseQty(t, "256Mi")
	container.Resources.Limits[corev1.ResourceMemory] = parseQty(t, "256Mi")

	reconciler := newReconcilerWithClient()
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if strings.Contains(query, "memory") {
				return generateSamples(200, 2*1024*1024*1024), nil
			}
			return generateSamples(200, 2.0), nil
		},
	}
	rec, _, _, _, _, err := reconciler.computeRecommendations(context.Background(), policy, deploy, mc, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Containers, 1)
	got := rec.Containers[0]
	assert.True(t, got.Recommended.CPURequest.Equal(parseQty(t, "500m")),
		"engine CPU request got %s", got.Recommended.CPURequest.String())
	assert.True(t, got.Recommended.CPULimit.Equal(parseQty(t, "1")),
		"engine CPU limit got %s", got.Recommended.CPULimit.String())
	assert.True(t, got.Recommended.MemoryRequest.Equal(parseQty(t, "256Mi")),
		"engine memory request got %s", got.Recommended.MemoryRequest.String())
	assert.True(t, got.Recommended.MemoryLimit.Equal(parseQty(t, "512Mi")),
		"engine memory limit got %s", got.Recommended.MemoryLimit.String())

	pod := runningPod(t, "api-server", "500m", "256Mi", "500m", "256Mi", corev1.PodQOSGuaranteed)
	target, _ := buildResizeTarget(got)
	applied, _ := reconciler.applyLiveResizeTarget(policy, pod, got, target)
	assert.True(t, applied.Requests.Cpu().Equal(parseQty(t, "500m")),
		"post-apply CPU request got %s", applied.Requests.Cpu().String())
	appliedMem := applied.Requests.Memory()
	assert.True(t, appliedMem.Equal(*applied.Limits.Memory()),
		"applied memory request %s limit %s", appliedMem.String(), applied.Limits.Memory().String())
	assert.True(t, appliedMem.Cmp(memMax) > 0,
		"applied memory request %s must exceed maxAllowed %s", appliedMem.String(), memMax.String())
	assert.True(t, appliedMem.Equal(parseQty(t, "512Mi")),
		"applied memory request got %s", appliedMem.String())
}

func TestExecuteResizes_GuaranteedCPUMultiplierSkipsWithoutEviction(t *testing.T) {
	const workload = "gcpu"
	pod := runningPod(t, workload, "200m", "256Mi", "200m", "256Mi", corev1.PodQOSGuaranteed)
	policy := requestsAndLimitsPolicy(workload)
	two := "2"
	policy.Spec.CPU.LimitMultiplier = &two
	c := scaledContainerRec(t, policy, "200m", "200m", "256Mi", "256Mi", "250m", "256Mi")
	require.True(t, c.Recommended.CPULimit.Equal(parseQty(t, "500m")),
		"CPU limit got %s", c.Recommended.CPULimit.String())

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	target, _ := buildResizeTarget(c)
	applied, _ := reconciler.applyLiveResizeTarget(policy, pod, c, target)
	skip, reason := reconciler.shouldSkipResize(context.Background(), pod, c, applied, nil)
	require.True(t, skip)
	require.Equal(t, "would change QoS class from Guaranteed. Use controlledValues: RequestsAndLimits, or on K8s v1.33 set resizePolicy to RestartContainer for memory", reason)

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 0, count)
	require.Empty(t, history)
	assertNoResizeOrEvict(t, reconciler)

	select {
	case ev := <-reconciler.Recorder.(*events.FakeRecorder).Events:
		require.Contains(t, ev, "ResizeSkipped")
		require.Contains(t, ev, "would change QoS class from Guaranteed")
	default:
		t.Fatal("QoS skip must emit ResizeSkipped")
	}
}

func TestExecuteResizes_GuaranteedMemoryMultiplierProceeds(t *testing.T) {
	// 512Mi above maxAllowed is required so the request stays equal to the
	// limit. Capping only the request would change QoS (issue 970).
	const workload = "gmem"
	pod := runningPod(t, workload, "200m", "256Mi", "200m", "256Mi", corev1.PodQOSGuaranteed)
	policy := requestsAndLimitsPolicy(workload)
	two := "2"
	policy.Spec.Memory.LimitMultiplier = &two
	memMax := parseQty(t, "256Mi")
	policy.Spec.Memory.MaxAllowed = &memMax
	c := scaledContainerRec(t, policy, "200m", "200m", "256Mi", "256Mi", "200m", "256Mi")
	require.True(t, c.Recommended.MemoryLimit.Equal(parseQty(t, "512Mi")),
		"memory limit got %s", c.Recommended.MemoryLimit.String())
	require.True(t, c.Recommended.CPULimit.Equal(parseQty(t, "200m")))

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	target, _ := buildResizeTarget(c)
	applied, _ := reconciler.applyLiveResizeTarget(policy, pod, c, target)
	skip, reason := reconciler.shouldSkipResize(context.Background(), pod, c, applied, nil)
	require.False(t, skip, "reason %q", reason)
	require.True(t, applied.Requests.Memory().Equal(*applied.Limits.Memory()))
	require.True(t, applied.Requests.Memory().Cmp(memMax) > 0)

	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 1, count)
	resized := resizedPodFromClientset(t, reconciler)
	gotReq := resized.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	gotLim := resized.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.True(t, gotReq.Equal(gotLim), "request %s limit %s", gotReq.String(), gotLim.String())
	assert.True(t, gotReq.Equal(parseQty(t, "512Mi")), "applied memory request got %s", gotReq.String())
	assert.True(t, resized.Spec.Containers[0].Resources.Requests.Cpu().Equal(parseQty(t, "200m")))
	assert.True(t, resized.Spec.Containers[0].Resources.Limits.Cpu().Equal(parseQty(t, "200m")))
}

func TestExecuteResizes_ExplicitOneBurstableToGuaranteedSkips(t *testing.T) {
	const workload = "burst"
	pod := runningPod(t, workload, "250m", "256Mi", "1000m", "256Mi", corev1.PodQOSBurstable)
	policy := requestsAndLimitsPolicy(workload)
	one := "1"
	policy.Spec.CPU.LimitMultiplier = &one
	c := scaledContainerRec(t, policy, "250m", "1000m", "256Mi", "256Mi", "500m", "256Mi")
	require.True(t, c.Recommended.CPURequest.Equal(c.Recommended.CPULimit),
		"explicit 1 must equalize CPU, request %s limit %s",
		c.Recommended.CPURequest.String(), c.Recommended.CPULimit.String())
	require.Equal(t, int64(500), c.Recommended.CPULimit.MilliValue())

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	target, _ := buildResizeTarget(c)
	applied, _ := reconciler.applyLiveResizeTarget(policy, pod, c, target)
	skip, reason := reconciler.shouldSkipResize(context.Background(), pod, c, applied, nil)
	require.True(t, skip)
	require.Equal(t, "would change QoS class from Burstable to Guaranteed", reason)

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 0, count)
	require.Empty(t, history)
	assertNoResizeOrEvict(t, reconciler)
}

func TestExecuteResizes_MemoryLimitWholeByte(t *testing.T) {
	const workload = "wholebyte"
	pod := runningPod(t, workload, "200m", "300Mi", "400m", "1000Mi", corev1.PodQOSBurstable)
	policy := requestsAndLimitsPolicy(workload)
	c := scaledContainerRec(t, policy, "200m", "400m", "300Mi", "1000Mi", "200m", "400Mi")
	assert.Equal(t, int64(1398101334000), c.Recommended.MemoryLimit.MilliValue(),
		"memory limit got %s", c.Recommended.MemoryLimit.String())

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 1, count)
	resized := resizedPodFromClientset(t, reconciler)
	gotLim := resized.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.Zero(t, gotLim.MilliValue()%1000, "memory limit %s is not a whole byte", gotLim.String())
	assert.Equal(t, int64(1398101334000), gotLim.MilliValue(), "memory limit got %s", gotLim.String())
	gotReq := resized.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	assert.True(t, gotReq.Equal(parseQty(t, "400Mi")), "memory request got %s", gotReq.String())
}

// A fractional live memory limit that rounds up to the scaled whole-byte
// target must not trigger a resize on its own (issue #965).
func TestExecuteResizes_FractionalLiveMemoryLimitAloneDoesNotResize(t *testing.T) {
	const workload = "fracmem"
	pod := runningPod(t, workload, "200m", "200Mi", "400m", "699050666666m", corev1.PodQOSBurstable)
	policy := requestsAndLimitsPolicy(workload)
	c := scaledContainerRec(t, policy, "200m", "400m", "200Mi", "699050666666m", "200m", "200Mi")
	require.Equal(t, int64(699050667000), c.Recommended.MemoryLimit.MilliValue(),
		"memory limit got %s", c.Recommended.MemoryLimit.String())

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 0, count)
	require.Empty(t, history)
	assertNoResizeOrEvict(t, reconciler)
}

// A live memory limit at least one byte below the target still resizes.
func TestExecuteResizes_MemoryLimitOneByteShortResizes(t *testing.T) {
	const workload = "shortmem"
	pod := runningPod(t, workload, "200m", "200Mi", "400m", "699050665500m", corev1.PodQOSBurstable)
	policy := requestsAndLimitsPolicy(workload)
	c := scaledContainerRec(t, policy, "200m", "400m", "200Mi", "699050666666m", "200m", "200Mi")

	deploy := newTestDeployment(workload, "default", map[string]string{"app": workload})
	reconciler := resizeHarness(pod, deploy)
	count, _ := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy},
		[]attunev1alpha1.WorkloadRecommendation{workloadRec(workload, c)}, podMap(workload, pod), nil, nil)
	require.Equal(t, 1, count)
	resized := resizedPodFromClientset(t, reconciler)
	gotLim := resized.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	assert.Equal(t, int64(699050667000), gotLim.MilliValue(), "memory limit got %s", gotLim.String())
}

func TestTargetLimitsMatchLive_MemoryWholeByteCPUExact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		res    corev1.ResourceName
		live   string
		target string
		want   bool
	}{
		{name: "memory fractional live equals its rounded-up byte", res: corev1.ResourceMemory, live: "699050666666m", target: "699050667", want: true},
		{name: "memory one byte short differs", res: corev1.ResourceMemory, live: "699050666", target: "699050667", want: false},
		{name: "memory fractional one byte short differs", res: corev1.ResourceMemory, live: "699050665500m", target: "699050667", want: false},
		{name: "cpu sub-milli difference still differs", res: corev1.ResourceCPU, live: "1", target: "1000500u", want: false},
		{name: "cpu equal matches", res: corev1.ResourceCPU, live: "1", target: "1000m", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := corev1.ResourceList{tt.res: parseQty(t, tt.live)}
			target := corev1.ResourceList{tt.res: parseQty(t, tt.target)}
			assert.Equal(t, tt.want, targetLimitsMatchLive(live, target))
			assert.Equal(t, tt.want, resourcesEqual(
				corev1.ResourceRequirements{Limits: live}, corev1.ResourceRequirements{Limits: target}))
		})
	}
}

func TestResourcesEqual_RequestsStayExact(t *testing.T) {
	t.Parallel()
	a := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: parseQty(t, "699050666666m")}}
	b := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: parseQty(t, "699050667")}}
	assert.False(t, resourcesEqual(a, b), "memory requests keep exact comparison")
}
