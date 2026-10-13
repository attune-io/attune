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

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func runLoggedHPARetune(
	t *testing.T,
	hpa autoscalingv2.HorizontalPodAutoscaler,
	pod corev1.Pod,
	history []attunev1alpha1.ResizeHistoryEntry,
) (client.Client, string) {
	t.Helper()
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa.DeepCopy()).Build()
	recorder := events.NewFakeRecorder(8)
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = cl
	reconciler.Scheme = scheme
	reconciler.Recorder = recorder
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	var logged string
	logger := funcr.NewJSON(func(obj string) { logged += obj + "\n" }, funcr.Options{Verbosity: 1})
	ctx := log.IntoContext(context.Background(), logger)
	reconciler.retuneHPAAfterResize(ctx, policy, attunev1alpha1.UpdateTypeAuto,
		history,
		[]attunev1alpha1.WorkloadRecommendation{{Workload: "api-server", Kind: "Deployment"}},
		[]autoscalingv2.HorizontalPodAutoscaler{hpa},
		map[string][]corev1.Pod{"api-server": {pod}},
	)
	return cl, logged
}

func TestRetuneHPAAfterResize_UnusableRequestBaseLog(t *testing.T) {
	t.Parallel()
	const badQty = "zzNotAQuantity"
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "100m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl, logged := runLoggedHPARetune(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		cpuHistory("app", badQty, "50m"),
		cpuHistory("sidecar", "100m", "200m"),
	})
	require.Contains(t, logged, "HPA auto-tune skipped a metric with no usable request base")
	require.Contains(t, logged, hpa.Name)
	assert.NotContains(t, logged, badQty)

	stored := storedHPA(t, cl, hpa.Name)
	assert.Equal(t, int32(80), metricUtil(t, stored, 0))
	_, hasRequest := stored.Annotations[annotationHPAOriginalCPURequest]
	assert.False(t, hasRequest)
}

func TestRetuneHPAAfterResize_UnparsedStoredBaseLog(t *testing.T) {
	t.Parallel()
	const badRequest = "zzNotAQuantityRequest"
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: badRequest,
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "200m", "1000m"))
	cl, logged := runLoggedHPARetune(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		cpuHistory("app", "100m", "200m"),
	})
	require.Contains(t, logged, "HPA auto-tune ignored a stored base that did not parse")
	require.Contains(t, logged, annotationHPAOriginalCPU)
	require.Contains(t, logged, annotationHPAOriginalCPURequest)
	assert.NotContains(t, logged, badRequest)

	stored := storedHPA(t, cl, hpa.Name)
	// This cycle is 80 * 100m / 200m. The unparsed request is left as
	// input; retune does not write that string into a new base.
	assert.Equal(t, int32(40), metricUtil(t, stored, 0))
	assert.Equal(t, badRequest, stored.Annotations[annotationHPAOriginalCPURequest])
	_, _, parsed := storedHPABase(stored.Annotations, annotationHPAOriginalCPU, annotationHPAOriginalCPURequest)
	assert.False(t, parsed)
}

func TestRetuneHPAAfterResize_UnchangedTargetLog(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "99m", "1000m"))
	cl, logged := runLoggedHPARetune(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		cpuHistory("app", "100m", "99m"),
	})
	require.Contains(t, logged, "HPA utilization target unchanged after resize")
	require.Contains(t, logged, hpa.Name)
	assert.NotContains(t, logged, "80")

	stored := storedHPA(t, cl, hpa.Name)
	assert.Equal(t, int32(80), metricUtil(t, stored, 0))
	_, hasRequest := stored.Annotations[annotationHPAOriginalCPURequest]
	assert.False(t, hasRequest)
}

func TestRetuneHPAAfterResize_UnparsedContainerMemoryBaseLog(t *testing.T) {
	t.Parallel()
	const badRequest = "zzNotAMemory"
	const rawMemory = `{"app":{"target":"80","request":"` + badRequest + `"}}`
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalContainerMemory: rawMemory,
	}, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server", memContainer(t, "app", "512Mi", "2Gi"))
	cl, logged := runLoggedHPARetune(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		memoryHistory("app", "1Gi", "512Mi"),
	})
	require.Contains(t, logged, "HPA auto-tune ignored a stored base that did not parse")
	require.Contains(t, logged, annotationHPAOriginalContainerMemory)
	assert.NotContains(t, logged, badRequest)

	stored := storedHPA(t, cl, hpa.Name)
	// 80 * 1Gi / 512Mi. The unparsed JSON request is left as input.
	assert.Equal(t, int32(160), metricUtil(t, stored, 0))
	assert.Equal(t, rawMemory, stored.Annotations[annotationHPAOriginalContainerMemory])
	_, _, parsed := storedContainerMemory(stored.Annotations, "app")
	assert.False(t, parsed)
}

func TestRetuneHPAAfterResize_ZeroMilliRequestSkips(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "100m", "1000m"))
	cl, logged := runLoggedHPARetune(t, hpa, pod, []attunev1alpha1.ResizeHistoryEntry{
		cpuHistory("app", "0", "100m"),
	})
	assert.NotContains(t, logged, "HPA auto-tune skipped a metric with no usable request base")

	stored := storedHPA(t, cl, hpa.Name)
	assert.Equal(t, int32(80), metricUtil(t, stored, 0))
	_, hasRequest := stored.Annotations[annotationHPAOriginalCPURequest]
	assert.False(t, hasRequest)
}
