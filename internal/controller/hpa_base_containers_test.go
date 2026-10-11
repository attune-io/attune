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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestParseResourceBaseContainers(t *testing.T) {
	t.Parallel()
	listed, ok := parseResourceBaseContainers(" sidecar,app ")
	require.True(t, ok)
	assert.Equal(t, map[string]struct{}{"app": {}, "sidecar": {}}, listed)

	_, ok = parseResourceBaseContainers("")
	assert.False(t, ok)
	_, ok = parseResourceBaseContainers("app,")
	assert.False(t, ok)
	_, ok = parseResourceBaseContainers("app,SIDECAR")
	assert.False(t, ok)
}

func TestPodResourceContainers_SumMatchesPodMillis(t *testing.T) {
	t.Parallel()
	always := corev1.ContainerRestartPolicyAlways
	proxy := podContainer(t, "istio-proxy", "100m", "200m")
	proxy.RestartPolicy = &always
	pod := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "0", "100m"),
	)
	pod.Spec.InitContainers = []corev1.Container{
		proxy,
		podContainer(t, "migrate", "50m", "100m"),
	}
	rows := map[string]hpaCPURow{
		"app":         {old: 400, neu: 300, ok: true},
		"istio-proxy": {old: 200, neu: 100, ok: true},
		"migrate":     {old: 80, neu: 50, ok: true},
		"ghost":       {old: 25, neu: 25, ok: true},
	}
	observed := podResourceContainers(&pod, rows, corev1.ResourceCPU)
	old, _, ok := podCPUMillis(&pod, rows)
	require.True(t, ok)
	var sum int64
	for _, v := range observed.milli {
		sum += v
	}
	assert.Equal(t, old, sum)
	assert.Equal(t, map[string]int64{"app": 400, "istio-proxy": 200, "ghost": 25}, observed.milli)
	_, migrateListed := observed.names["migrate"]
	assert.False(t, migrateListed)
	_, sidecarPresent := observed.names["sidecar"]
	assert.True(t, sidecarPresent, "a zero request stays present so a stored name is not treated as removed")
	assert.Equal(t, "app,ghost,istio-proxy,sidecar", formatResourceBaseContainers(observed.names))
}

func TestRetuneHPAAfterResize_ZeroRequestStaysOnTheContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "100m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "0", "100m")}, nil)
	first := storedHPA(t, cl, "api-server-hpa")
	require.Equal(t, int32(53), metricUtil(t, first, 0), "80 * 200/300 = 53")
	require.Equal(t, "200m", first.Annotations[annotationHPAOriginalCPURequest])
	require.Equal(t, "app,sidecar", first.Annotations[annotationHPAOriginalCPURequestContainers])

	next := workloadPod("api-server",
		podContainer(t, "app", "150m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, first, next,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "100m", "150m")}, nil)
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(45), metricUtil(t, second, 0), "80 * 200/350 = 45; 68 adds the 100m Attune already applied")
	assert.Equal(t, "200m", second.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalCPURequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_ZeroRequestJoinIsRecordedBeforeItsGrowth(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"))
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")}, nil)
	first := storedHPA(t, cl, "api-server-hpa")
	require.Equal(t, "400m", first.Annotations[annotationHPAOriginalCPURequest])
	require.Equal(t, "app", first.Annotations[annotationHPAOriginalCPURequestContainers])
	require.Equal(t, int32(106), metricUtil(t, first, 0))

	joined := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, first, joined,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "0", "100m")}, nil)
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, "400m", second.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalCPURequestContainers])
	assert.Equal(t, int32(80), metricUtil(t, second, 0), "80 * 400/400 = 80")
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}

	grown := workloadPod("api-server",
		podContainer(t, "app", "300m", "1000m"),
		podContainer(t, "sidecar", "150m", "1000m"),
	)
	cl, _, notes = runScriptedHPARetune(t, nil, second, grown,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "100m", "150m")}, nil)
	third := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(71), metricUtil(t, third, 0), "80 * 400/450 = 71; 88 adds the sidecar's 100m")
	assert.Equal(t, "400m", third.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", third.Annotations[annotationHPAOriginalCPURequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_OffsettingZeroJoinStaysOutOfTheBase(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "300m", "1000m"))
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "300m")}, nil)
	first := storedHPA(t, cl, "api-server-hpa")
	require.Equal(t, "app", first.Annotations[annotationHPAOriginalCPURequestContainers])

	offset := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, first, offset,
		[]attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "300m", "200m"),
			cpuHistory("sidecar", "0", "100m"),
		}, nil)
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, "400m", second.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalCPURequestContainers])
	assert.Equal(t, int32(106), metricUtil(t, second, 0), "net pod total is unchanged, so the percent stays")
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}

	grown := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "150m", "1000m"),
	)
	cl, _, notes = runScriptedHPARetune(t, nil, second, grown,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("sidecar", "100m", "150m")}, nil)
	third := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(91), metricUtil(t, third, 0), "80 * 400/350 = 91; 114 adds the sidecar's 100m")
	assert.Equal(t, "400m", third.Annotations[annotationHPAOriginalCPURequest])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestHPABaseContainerAnnotationNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "attune.io/original-cpu-request-containers", annotationHPAOriginalCPURequestContainers)
	assert.Equal(t, "attune.io/original-memory-request-containers", annotationHPAOriginalMemoryRequestContainers)
	assert.NotEqual(t, annotationHPAOriginalCPURequest, annotationHPAOriginalCPURequestContainers)
	assert.NotEqual(t, annotationHPAOriginalMemoryRequest, annotationHPAOriginalMemoryRequestContainers)
}

func TestRetuneHPAAfterResize_FirstWriteRecordsContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "600m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	pod.Spec.InitContainers = []corev1.Container{podContainer(t, "migrate", "50m", "100m")}
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "600m"),
			cpuHistory("migrate", "100m", "50m"),
		}, nil)

	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(60), metricUtil(t, updated, 0), "80 * 600/800 = 60")
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers],
		"one-shot init stays off the list")
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemoryRequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_ListedFullBaseSurvivesHistoryOldMatch(t *testing.T) {
	t.Parallel()
	always := corev1.ContainerRestartPolicyAlways
	cases := []struct {
		name    string
		live    int32
		sidecar corev1.Container
		list    string
	}{
		{
			name:    "regular sidecar",
			live:    60,
			sidecar: podContainer(t, "sidecar", "200m", "1000m"),
			list:    "app,sidecar",
		},
		{
			name:    "live target still 80",
			live:    80,
			sidecar: podContainer(t, "sidecar", "200m", "1000m"),
			list:    "app,sidecar",
		},
		{
			name: "native sidecar",
			live: 60,
			sidecar: func() corev1.Container {
				c := podContainer(t, "istio-proxy", "200m", "1000m")
				c.RestartPolicy = &always
				return c
			}(),
			list: "app,istio-proxy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
				annotationHPAOriginalCPU:                  "80",
				annotationHPAOriginalCPURequest:           "600m",
				annotationHPAOriginalCPURequestContainers: tc.list,
			}, cpuResourceMetric(tc.live))
			pod := workloadPod("api-server", podContainer(t, "app", "650m", "2000m"))
			if tc.sidecar.RestartPolicy != nil {
				pod.Spec.InitContainers = []corev1.Container{tc.sidecar}
			} else {
				pod.Spec.Containers = append(pod.Spec.Containers, tc.sidecar)
			}
			cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
				[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "600m", "650m")}, nil)
			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, int32(56), metricUtil(t, updated, 0), "80 * 600/850 = 56")
			assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
			assert.Equal(t, tc.list, updated.Annotations[annotationHPAOriginalCPURequestContainers])
			for _, note := range notes {
				assert.NotContains(t, note, "HPABaseRepaired")
			}
		})
	}
}

func TestRetuneHPAAfterResize_TwoStepFullBaseKeepsContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "600m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "600m")}, nil)
	first := storedHPA(t, cl, "api-server-hpa")
	require.Equal(t, "app,sidecar", first.Annotations[annotationHPAOriginalCPURequestContainers])
	require.Equal(t, "600m", first.Annotations[annotationHPAOriginalCPURequest])

	next := workloadPod("api-server",
		podContainer(t, "app", "650m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, first, next,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "600m", "650m")}, nil)
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(56), metricUtil(t, second, 0))
	assert.Equal(t, "600m", second.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalCPURequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_ListLessFullBaseStillRewritesWhenHistoryOldMatches(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:        "80",
		annotationHPAOriginalCPURequest: "600m",
	}, cpuResourceMetric(60))
	pod := workloadPod("api-server",
		podContainer(t, "app", "650m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "600m", "650m")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(75), metricUtil(t, updated, 0), "80 * 800/850 = 75")
	assert.Equal(t, "800m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
	require.NotEmpty(t, notes)
	assert.Contains(t, notes[0], "HPABaseRepaired")
	assert.Contains(t, notes[0], annotationHPAOriginalCPURequest)
}

func TestRetuneHPAAfterResize_MalformedContainerListUsesEqualityRule(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:                  "80",
		annotationHPAOriginalCPURequest:           "600m",
		annotationHPAOriginalCPURequestContainers: "app,SIDECAR",
	}, cpuResourceMetric(60))
	pod := workloadPod("api-server",
		podContainer(t, "app", "650m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "600m", "650m")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(75), metricUtil(t, updated, 0))
	assert.Equal(t, "800m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
	require.NotEmpty(t, notes)
	assert.Contains(t, notes[0], "HPABaseRepaired")
}

func TestRetuneHPAAfterResize_ListAddsSidecarThatAppearedLater(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:                  "80",
		annotationHPAOriginalCPURequest:           "400m",
		annotationHPAOriginalCPURequestContainers: "app",
	}, cpuResourceMetric(106)) // 80 * 600/450 truncates to the live percent, so only the base changes
	pod := workloadPod("api-server",
		podContainer(t, "app", "250m", "2000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "300m", "250m")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0), "80 * 600/450 = 106")
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
	require.NotEmpty(t, notes)
	assert.Contains(t, notes[0], "HPABaseRepaired")
}

func TestRetuneHPAAfterResize_RemovedContainerStaysInStoredBase(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalCPU:                  "80",
		annotationHPAOriginalCPURequest:           "600m",
		annotationHPAOriginalCPURequestContainers: "app,sidecar",
	}, cpuResourceMetric(80))
	pod := workloadPod("api-server", podContainer(t, "app", "500m", "2000m"))
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "500m")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(96), metricUtil(t, updated, 0), "80 * 600/500 = 96")
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAAfterResize_ContainerResourceDoesNotWritePodContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuContainerMetric("app", 80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPURequestContainers])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalCPURequest])
}

func TestRetuneHPAAfterResize_ConflictRetryKeepsContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, cpuResourceMetric(80))
	pod := workloadPod("api-server",
		podContainer(t, "app", "200m", "1000m"),
		podContainer(t, "sidecar", "100m", "1000m"),
	)
	cl, updates, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")},
		conflictThenForeignAnnotation(t))
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(2), updates)
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalCPURequestContainers])
	assert.Equal(t, "500m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "keep", updated.Annotations[otherControllerAnnotation])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAMemory_ListedFullBaseSurvivesHistoryOldMatch(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalMemory:                  "80",
		annotationHPAOriginalMemoryRequest:           "1536Mi",
		annotationHPAOriginalMemoryRequestContainers: "app,sidecar",
	}, memoryResourceMetric(60))
	pod := workloadPod("api-server",
		memContainer(t, "app", "2Gi", "4Gi"),
		memContainer(t, "sidecar", "512Mi", "1Gi"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1536Mi", "2Gi")}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(48), metricUtil(t, updated, 0), "80 * 1536Mi/2560Mi = 48")
	assert.Equal(t, "1536Mi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, "app,sidecar", updated.Annotations[annotationHPAOriginalMemoryRequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAMemory_TwoStepFullBaseKeepsContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server",
		memContainer(t, "app", "1536Mi", "4Gi"),
		memContainer(t, "sidecar", "512Mi", "1Gi"),
	)
	cl, _, _ := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "1536Mi")}, nil)
	first := storedHPA(t, cl, "api-server-hpa")
	require.Equal(t, "1536Mi", first.Annotations[annotationHPAOriginalMemoryRequest])
	require.Equal(t, "app,sidecar", first.Annotations[annotationHPAOriginalMemoryRequestContainers])
	require.Equal(t, int32(60), metricUtil(t, first, 0))

	next := workloadPod("api-server",
		memContainer(t, "app", "2Gi", "4Gi"),
		memContainer(t, "sidecar", "512Mi", "1Gi"),
	)
	cl, _, notes := runScriptedHPARetune(t, nil, first, next,
		[]attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1536Mi", "2Gi")}, nil)
	second := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(48), metricUtil(t, second, 0))
	assert.Equal(t, "1536Mi", second.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, "app,sidecar", second.Annotations[annotationHPAOriginalMemoryRequestContainers])
	for _, note := range notes {
		assert.NotContains(t, note, "HPABaseRepaired")
	}
}

func TestRetuneHPAMemory_ListLessPartialRepairWritesContainerList(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalMemory:        "80",
		annotationHPAOriginalMemoryRequest: "1Gi",
	}, memoryResourceMetric(80))
	proxy := memContainer(t, "istio-proxy", "1Gi", "2Gi")
	always := corev1.ContainerRestartPolicyAlways
	proxy.RestartPolicy = &always
	pod := workloadPod("api-server", memContainer(t, "app", "512Mi", "2Gi"))
	pod.Spec.InitContainers = []corev1.Container{
		proxy,
		memContainer(t, "migrate", "128Mi", "2Gi"),
	}
	cl, _, notes := runScriptedHPARetune(t, nil, hpa, pod,
		[]attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
			memoryHistory("migrate", "256Mi", "128Mi"),
		}, nil)
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, "app,istio-proxy", updated.Annotations[annotationHPAOriginalMemoryRequestContainers])
	require.NotEmpty(t, notes)
	assert.Contains(t, notes[0], "HPABaseRepaired")
}
