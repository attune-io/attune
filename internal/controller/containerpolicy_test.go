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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/recommendation"
	"github.com/attune-io/attune/internal/resize"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

// Not parallel: computeRecommendations calls recommendContainer, which
// touches NanInfSamplesTotal.

func mustQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func qtyPtr(t *testing.T, s string) *resource.Quantity {
	t.Helper()
	q := mustQty(t, s)
	return &q
}

func deploymentWithContainers(t *testing.T, names ...string) *appsv1.Deployment {
	t.Helper()
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	base := deploy.Spec.Template.Spec.Containers[0].DeepCopy()
	containers := make([]corev1.Container, 0, len(names))
	for _, name := range names {
		c := base.DeepCopy()
		c.Name = name
		containers = append(containers, *c)
	}
	deploy.Spec.Template.Spec.Containers = containers
	return deploy
}

func highUsageCollector() *mockCollector {
	return &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			if strings.Contains(query, "cpu") {
				return generateSamples(200, 2.0), nil
			}
			return generateSamples(200, 128*1024*1024), nil
		},
	}
}

func recommendWorkloads(t *testing.T, policy *attunev1alpha1.AttunePolicy, deploy *appsv1.Deployment, cpuEngine, memEngine *recommendation.RecommendationEngine) *attunev1alpha1.WorkloadRecommendation {
	t.Helper()
	reconciler := newReconcilerWithClient()
	rec, _, _, _, _, err := reconciler.computeRecommendations(
		context.Background(), policy, deploy, highUsageCollector(), nil, cpuEngine, memEngine, nil, nil)
	require.NoError(t, err)
	return rec
}

func containerRec(t *testing.T, rec *attunev1alpha1.WorkloadRecommendation, name string) attunev1alpha1.ContainerRecommendation {
	t.Helper()
	require.NotNil(t, rec)
	for _, c := range rec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %s missing from recommendation", name)
	return attunev1alpha1.ContainerRecommendation{}
}

func recNames(rec *attunev1alpha1.WorkloadRecommendation) map[string]bool {
	names := map[string]bool{}
	if rec == nil {
		return names
	}
	for _, c := range rec.Containers {
		names[c.Name] = true
	}
	return names
}

func TestEnginesForContainer_OmittedAndEmptyShareOnePair(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.CPU.MaxAllowed = qtyPtr(t, "200m")
	sharedCPU, sharedMem := buildRecommendationEngines(policy)
	for _, list := range [][]attunev1alpha1.ContainerResourcePolicy{nil, {}} {
		policy.Spec.ContainerPolicies = list
		for _, name := range []string{"app", "sidecar"} {
			gotCPU, gotMem := enginesForContainer(policy, name, sharedCPU, sharedMem)
			assert.Same(t, sharedCPU, gotCPU, "name %s", name)
			assert.Same(t, sharedMem, gotMem, "name %s", name)
		}
		deploy := deploymentWithContainers(t, "app", "sidecar")
		rec := recommendWorkloads(t, policy, deploy, sharedCPU, sharedMem)
		app := containerRec(t, rec, "app")
		side := containerRec(t, rec, "sidecar")
		assert.Equal(t, int64(200), app.Recommended.CPURequest.MilliValue())
		assert.Equal(t, int64(200), side.Recommended.CPURequest.MilliValue())
	}
}

func TestRecommendContainer_SidecarMaxDoesNotCapApp(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	side := containerRec(t, rec, "sidecar")
	app := containerRec(t, rec, "app")
	assert.Equal(t, int64(200), side.Recommended.CPURequest.MilliValue())
	assert.Greater(t, app.Recommended.CPURequest.MilliValue(), int64(200))
}

func TestRecommendContainer_IgnoresPassedEnginesWhenListSet(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.CPU.MaxAllowed = qtyPtr(t, "50m")
	tightCPU, tightMem := buildRecommendationEngines(policy)
	policy.Spec.CPU.MaxAllowed = qtyPtr(t, "4000m")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, tightCPU, tightMem)
	side := containerRec(t, rec, "sidecar")
	app := containerRec(t, rec, "app")
	assert.Equal(t, int64(200), side.Recommended.CPURequest.MilliValue())
	assert.Greater(t, app.Recommended.CPURequest.MilliValue(), int64(50))
}

func TestEnginesForContainer_PercentileInheritsDefaults(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
		{ContainerName: "app"},
		{ContainerName: "sidecar", CPU: &attunev1alpha1.ResourceConfig{Percentile: 50}},
	}
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 90, Overhead: "20"},
		},
	}
	pkgdefaults.MergeDefaults(policy, defaults)
	appCPU, _ := enginesForContainer(policy, "app", nil, nil)
	sideCPU, _ := enginesForContainer(policy, "sidecar", nil, nil)
	assert.Equal(t, 90, appCPU.Percentile())
	assert.Equal(t, 50, sideCPU.Percentile())
}

func TestRecommendContainer_OverheadZeroDoesNotInherit(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{Percentile: 0, Overhead: "0"},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	side := containerRec(t, rec, "sidecar")
	app := containerRec(t, rec, "app")
	require.NotNil(t, side.Explanation)
	require.NotNil(t, side.Explanation.CPU)
	require.NotNil(t, app.Explanation)
	require.NotNil(t, app.Explanation.CPU)
	assert.Equal(t, 0.0, side.Explanation.CPU.Overhead)
	assert.Equal(t, 20.0, app.Explanation.CPU.Overhead)
	sideCPU, _ := enginesForContainer(policy, "sidecar", nil, nil)
	assert.Equal(t, 95, sideCPU.Percentile())
}

func TestRecommendContainer_WildcardMaxAndLiteralReplacement(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ral := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
		{
			ContainerName: attunev1alpha1.ContainerPolicyWildcard,
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "300m")},
		},
		{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &ral},
		},
		{
			ContainerName: "sidecar",
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
		},
	}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	app := containerRec(t, rec, "app")
	side := containerRec(t, rec, "sidecar")
	assert.Equal(t, int64(300), app.Recommended.CPURequest.MilliValue())
	assert.Equal(t, int64(200), side.Recommended.CPURequest.MilliValue())
	appCPU, _ := enginesForContainer(policy, "app", nil, nil)
	sideCPU, _ := enginesForContainer(policy, "sidecar", nil, nil)
	assert.Equal(t, 95, appCPU.Percentile())
	assert.Equal(t, 95, sideCPU.Percentile())
}

func TestRecommendContainer_ContainerMaxChangePercent(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	change := int32(100)
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxChangePercent: &change},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	app := containerRec(t, rec, "app")
	side := containerRec(t, rec, "sidecar")
	assert.Equal(t, int64(750), app.Recommended.CPURequest.MilliValue())
	assert.Equal(t, int64(1000), side.Recommended.CPURequest.MilliValue())
	assert.Greater(t, side.Recommended.CPURequest.MilliValue(), app.Recommended.CPURequest.MilliValue())
}

func TestContainerDecreaseAllowed_NilSplit(t *testing.T) {
	cpuOK, memOK := containerDecreaseAllowed(nil, "app")
	assert.True(t, cpuOK)
	assert.False(t, memOK)

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "app",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	cpuOK, memOK = containerDecreaseAllowed(policy, "app")
	assert.True(t, cpuOK)
	assert.False(t, memOK)

	memAllow := true
	cpuBlock := false
	policy.Spec.ContainerPolicies[0].Memory = &attunev1alpha1.ResourceConfig{AllowDecrease: &memAllow}
	policy.Spec.ContainerPolicies[0].CPU.AllowDecrease = &cpuBlock
	cpuOK, memOK = containerDecreaseAllowed(policy, "app")
	assert.False(t, cpuOK)
	assert.True(t, memOK)

	cpuOK, memOK = containerDecreaseAllowed(policy, "sidecar")
	assert.True(t, cpuOK)
	assert.False(t, memOK)
}

func TestRecommendContainer_ExcludedNameStaysExcluded(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ExcludedContainers = []string{"sidecar"}
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	names := recNames(rec)
	assert.True(t, names["app"])
	assert.False(t, names["sidecar"])
}

func TestRecommendContainer_KnownSidecarExclusion(t *testing.T) {
	deploy := deploymentWithContainers(t, "app", "istio-proxy")
	withPolicy := func(known *bool, excluded []string) map[string]bool {
		t.Helper()
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.ExcludeKnownSidecars = known
		policy.Spec.ExcludedContainers = excluded
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "istio-proxy",
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
		}}
		return recNames(recommendWorkloads(t, policy, deploy, nil, nil))
	}
	unset := withPolicy(nil, nil)
	assert.True(t, unset["app"])
	assert.False(t, unset["istio-proxy"])

	off := false
	managed := withPolicy(&off, nil)
	assert.True(t, managed["app"])
	assert.True(t, managed["istio-proxy"])

	listed := withPolicy(&off, []string{"istio-proxy"})
	assert.True(t, listed["app"])
	assert.False(t, listed["istio-proxy"])
}

func TestRecommendContainer_InitVsNativeSidecar(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
		{ContainerName: "init-setup", CPU: &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")}},
		{ContainerName: "native-sidecar", CPU: &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")}},
	}
	deploy := deploymentWithContainers(t, "app")
	base := deploy.Spec.Template.Spec.Containers[0].DeepCopy()
	normal := base.DeepCopy()
	normal.Name = "init-setup"
	native := base.DeepCopy()
	native.Name = "native-sidecar"
	always := corev1.ContainerRestartPolicyAlways
	native.RestartPolicy = &always
	deploy.Spec.Template.Spec.InitContainers = []corev1.Container{*normal, *native}
	rec := recommendWorkloads(t, policy, deploy, nil, nil)
	names := recNames(rec)
	assert.True(t, names["app"])
	assert.True(t, names["native-sidecar"])
	assert.False(t, names["init-setup"])
}

func TestComputeVPARecommendations_ContainerMax(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "my-vpa"}
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
	}}
	deploy := deploymentWithContainers(t, "app", "sidecar")
	target := mustQty(t, "2")
	mem := mustQty(t, "512Mi")
	vpaRecs := []rsmetrics.VPAContainerRecommendation{
		{ContainerName: "app", CPUTarget: target, MemoryTarget: mem, CPUSet: true, MemorySet: true},
		{ContainerName: "sidecar", CPUTarget: target, MemoryTarget: mem, CPUSet: true, MemorySet: true},
	}
	reconciler := newReconcilerWithClient(deploy)
	rec, _, err := reconciler.computeVPARecommendationsForWorkload(
		context.Background(), policy, deploy, vpaRecs, nil, nil, nil, nil)
	require.NoError(t, err)
	side := containerRec(t, rec, "sidecar")
	app := containerRec(t, rec, "app")
	assert.Equal(t, int64(200), side.Recommended.CPURequest.MilliValue())
	assert.Greater(t, app.Recommended.CPURequest.MilliValue(), int64(200))
}

func TestApplyStartupBoosts_ContainerMaxCapsBoost(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				MaxAllowed: qtyPtr(t, "4000m"),
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
			ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{{
				ContainerName: "sidecar",
				CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: qtyPtr(t, "200m")},
			}},
		},
	}
	mem := mustQty(t, "128Mi")
	cpu := mustQty(t, "100m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "api-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: cpu.DeepCopy(), corev1.ResourceMemory: mem.DeepCopy(),
				}}},
				{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: cpu.DeepCopy(), corev1.ResourceMemory: mem.DeepCopy(),
				}}},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset
	reconciler.SetNowFunc(func() time.Time { return now })
	steady := mustQty(t, "500m")
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "sidecar", Recommended: attunev1alpha1.ResourceValues{CPURequest: steady.DeepCopy()}},
			{Name: "app", Recommended: attunev1alpha1.ResourceValues{CPURequest: steady.DeepCopy()}},
		},
	}}
	reconciler.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"api": {*pod}}, recs, resize.NewPodResizer(clientset, ctrl.Log), nil)
	got := map[string]int64{}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		for _, c := range updated.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				got[c.Name] = q.MilliValue()
			}
		}
	}
	assert.Equal(t, int64(200), got["sidecar"])
	assert.Equal(t, int64(1500), got["app"])

	policy.Spec.CPU.MaxAllowed = qtyPtr(t, "600m")
	policy.Spec.ContainerPolicies = nil
	appOnly := pod.DeepCopy()
	appOnly.Spec.Containers = []corev1.Container{pod.Spec.Containers[1]}
	clientset = kubefake.NewSimpleClientset(appOnly)
	fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(appOnly).Build()
	reconciler.Client = fakeClient
	reconciler.Clientset = clientset
	recs = []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "app", Recommended: attunev1alpha1.ResourceValues{CPURequest: steady.DeepCopy()}},
		},
	}}
	reconciler.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"api": {*appOnly}}, recs, resize.NewPodResizer(clientset, ctrl.Log), nil)
	var appMilli int64
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		appMilli = updated.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
	}
	assert.Equal(t, int64(600), appMilli)
}

func TestScaleAndLiveResize_PerContainerControlledValues(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ral := attunev1alpha1.ControlledRequestsAndLimits
	empty := ""
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
		{ContainerName: "app", CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &ral}},
		{ContainerName: "sidecar"},
		{ContainerName: "blank", CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &empty}},
	}
	currentReq := mustQty(t, "500m")
	currentLim := mustQty(t, "1000m")
	memReq := mustQty(t, "512Mi")
	memLim := mustQty(t, "1Gi")
	recReq := mustQty(t, "800m")
	appRec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			CPURequest: currentReq, CPULimit: currentLim, MemoryRequest: memReq, MemoryLimit: memLim,
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest: recReq, MemoryRequest: memReq, MemoryLimit: memLim,
		},
	}
	scaleControlledLimits(policy, &appRec, currentReq, currentLim, memReq, memLim)
	assert.Equal(t, int64(1600), appRec.Recommended.CPULimit.MilliValue())

	blankRec := appRec
	blankRec.Name = "blank"
	blankRec.Recommended.CPULimit = resource.Quantity{}
	scaleControlledLimits(policy, &blankRec, currentReq, currentLim, memReq, memLim)
	assert.True(t, blankRec.Recommended.CPULimit.IsZero())
	assert.True(t, containerControlledRequestsOnly(policy, "blank", corev1.ResourceCPU))
	assert.False(t, containerControlledRequestsOnly(policy, "app", corev1.ResourceCPU))
	assert.True(t, containerControlledRequestsOnly(policy, "sidecar", corev1.ResourceCPU))

	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "app", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: currentLim.DeepCopy(), corev1.ResourceMemory: memLim.DeepCopy(),
		}}},
		{Name: "sidecar", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: currentLim.DeepCopy(), corev1.ResourceMemory: memLim.DeepCopy(),
		}}},
	}}}
	reconciler := newReconcilerWithClient()
	appTarget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: recReq.DeepCopy()},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: appRec.Recommended.CPULimit.DeepCopy()},
	}
	applied, _ := reconciler.applyLiveResizeTarget(policy, pod, appRec, appTarget)
	assert.Equal(t, int64(1600), applied.Limits.Cpu().MilliValue())

	sideRec := appRec
	sideRec.Name = "sidecar"
	sideTarget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: recReq.DeepCopy()},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "1600m")},
	}
	sideApplied, _ := reconciler.applyLiveResizeTarget(policy, pod, sideRec, sideTarget)
	assert.Equal(t, int64(1000), sideApplied.Limits.Cpu().MilliValue())
}

func TestMaterializeContainerResources_PerContainer(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	ral := attunev1alpha1.ControlledRequestsAndLimits
	margin := int32(50)
	ignored := int32(0)
	allow := true
	policy.Spec.Memory.DecreaseUsageMarginPercent = &margin
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
		{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &ral},
			Memory: &attunev1alpha1.ResourceConfig{
				ControlledValues:           &ral,
				AllowDecrease:              &allow,
				DecreaseUsageMarginPercent: &ignored,
			},
		},
		{ContainerName: "sidecar"},
	}
	currentCPU := mustQty(t, "500m")
	currentCPULim := mustQty(t, "1000m")
	currentMem := mustQty(t, "512Mi")
	currentMemLim := mustQty(t, "1Gi")
	recCPU := mustQty(t, "800m")
	recCPULim := mustQty(t, "1600m")
	lowMem := mustQty(t, "128Mi")
	lowMemLim := mustQty(t, "256Mi")
	usage := mustQty(t, "200Mi")
	base := attunev1alpha1.ContainerRecommendation{
		Current: attunev1alpha1.ResourceValues{
			CPURequest: currentCPU, CPULimit: currentCPULim,
			MemoryRequest: currentMem, MemoryLimit: currentMemLim,
		},
		Recommended: attunev1alpha1.ResourceValues{
			CPURequest: recCPU, CPULimit: recCPULim,
			MemoryRequest: lowMem, MemoryLimit: lowMemLim,
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{RawPercentile: usage},
		},
	}
	appRec := base
	appRec.Name = "app"
	appOut := materializeContainerResources(policy, appRec)
	require.NotNil(t, appOut.Limits)
	assert.Equal(t, int64(1600), appOut.Limits.Cpu().MilliValue())
	assert.Equal(t, lowMem.Value(), appOut.Requests.Memory().Value())
	wantMemLim := mustQty(t, "300Mi")
	assert.Equal(t, wantMemLim.Value(), appOut.Limits.Memory().Value())

	sideRec := base
	sideRec.Name = "sidecar"
	// RequestsOnly leaves the recommended limit at the live limit. A lowered
	// limit here would clamp the request before the limit is stripped.
	sideRec.Recommended.MemoryLimit = currentMemLim.DeepCopy()
	sideOut := materializeContainerResources(policy, sideRec)
	assert.Nil(t, sideOut.Limits)
	assert.Equal(t, currentMem.Value(), sideOut.Requests.Memory().Value())
}

func TestRecordQuerySettings_UsesContainerBurst(t *testing.T) {
	burst := "0.5"
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{BurstSensitivity: &burst},
	}}
	side := &attunev1alpha1.ContainerRecommendationExplanation{
		CPU:    &attunev1alpha1.ResourceRecommendationExplanation{},
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{},
	}
	recordQuerySettings(policy, "sidecar", side)
	assert.Contains(t, side.CPU.FinalAdjustment, "burstSensitivity=0.5")
	app := &attunev1alpha1.ContainerRecommendationExplanation{
		CPU:    &attunev1alpha1.ResourceRecommendationExplanation{},
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{},
	}
	recordQuerySettings(policy, "app", app)
	assert.Contains(t, app.CPU.FinalAdjustment, "burstSensitivity=0.1")
	assert.NotContains(t, app.CPU.FinalAdjustment, "burstSensitivity=0.5")
}

func TestPodResourceRequestsOnly_IgnoresNativeSidecar(t *testing.T) {
	t.Parallel()
	requests := attunev1alpha1.ControlledRequestsOnly
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
				{ContainerName: "app", CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &requests}},
				{ContainerName: "mesh", CPU: &attunev1alpha1.ResourceConfig{ControlledValues: &both}},
			},
		},
	}
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name:          "mesh",
			RestartPolicy: &always,
		}},
		Containers: []corev1.Container{{Name: "app"}},
	}}
	// Resource HPA cap stays on spec.containers. Counting the native sidecar
	// would return false because mesh is RequestsAndLimits.
	assert.True(t, podResourceRequestsOnly(policy, pod, corev1.ResourceCPU, true))
}
