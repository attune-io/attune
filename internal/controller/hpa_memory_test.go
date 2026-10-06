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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func qty(t *testing.T, raw string) resource.Quantity {
	t.Helper()
	parsed, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return parsed
}

func memoryHistory(container, from, to string) attunev1alpha1.ResizeHistoryEntry {
	return attunev1alpha1.ResizeHistoryEntry{
		Workload:  "api-server",
		Container: container,
		Resource:  "memory",
		From:      from,
		To:        to,
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}
}

func memoryResourceMetric(util int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceMemory,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(util),
			},
		},
	}
}

func memoryContainerMetric(container string, util int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ContainerResourceMetricSourceType,
		ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
			Name:      corev1.ResourceMemory,
			Container: container,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(util),
			},
		},
	}
}

func memContainer(t *testing.T, name, req, lim string) corev1.Container {
	t.Helper()
	container := corev1.Container{Name: name, Image: "nginx"}
	if req != "" {
		parsed := qty(t, req)
		container.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: parsed}
	}
	if lim != "" {
		parsed := qty(t, lim)
		container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: parsed}
	}
	return container
}

func mixedContainer(t *testing.T, name, cpuReq, cpuLim, memReq, memLim string) corev1.Container {
	t.Helper()
	container := memContainer(t, name, memReq, memLim)
	if cpuReq != "" {
		parsed := qty(t, cpuReq)
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		container.Resources.Requests[corev1.ResourceCPU] = parsed
	}
	if cpuLim != "" {
		parsed := qty(t, cpuLim)
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		container.Resources.Limits[corev1.ResourceCPU] = parsed
	}
	return container
}

type memoryRetuneOpts struct {
	policy     *attunev1alpha1.AttunePolicy
	hpas       []autoscalingv2.HorizontalPodAutoscaler
	pod        *corev1.Pod
	history    []attunev1alpha1.ResizeHistoryEntry
	recs       []attunev1alpha1.WorkloadRecommendation
	recorder   *events.FakeRecorder
	failUpdate bool
}

func runMemoryRetune(t *testing.T, opts memoryRetuneOpts) (client.Client, int) {
	t.Helper()
	scheme := testScheme()
	objs := make([]client.Object, 0, len(opts.hpas))
	for i := range opts.hpas {
		objs = append(objs, opts.hpas[i].DeepCopy())
	}
	updates := 0
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, updateOpts ...client.UpdateOption) error {
			updates++
			if opts.failUpdate {
				return fmt.Errorf("injected update failure")
			}
			return c.Update(ctx, obj, updateOpts...)
		},
	})
	cl := builder.Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = cl
	reconciler.Scheme = scheme
	reconciler.Recorder = opts.recorder
	if opts.policy == nil {
		opts.policy = newTestPolicy("p", "default")
		opts.policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	}
	if opts.recs == nil {
		opts.recs = []attunev1alpha1.WorkloadRecommendation{{
			Workload: "api-server",
			Kind:     "Deployment",
		}}
	}
	var pods map[string][]corev1.Pod
	if opts.pod != nil {
		pods = map[string][]corev1.Pod{"api-server": {*opts.pod}}
	}
	reconciler.retuneHPAAfterResize(context.Background(), opts.policy, opts.policy.Spec.UpdateStrategy.Type,
		opts.history, opts.recs, opts.hpas, pods)
	return cl, updates
}

func recorderNotes(recorder *events.FakeRecorder) []string {
	if recorder == nil {
		return nil
	}
	var notes []string
	for {
		select {
		case note := <-recorder.Events:
			notes = append(notes, note)
		default:
			return notes
		}
	}
}

func memoryBounds(min, max *int32) *attunev1alpha1.HPATargetBounds {
	return &attunev1alpha1.HPATargetBounds{
		Memory: &attunev1alpha1.HPATargetBound{Min: min, Max: max},
	}
}

func policyWithBounds(bounds *attunev1alpha1.HPATargetBounds) *attunev1alpha1.AttunePolicy {
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.HPATargetBounds = bounds
	return policy
}

func containerMemory(t *testing.T, hpa autoscalingv2.HorizontalPodAutoscaler) map[string]hpaContainerMemoryEntry {
	t.Helper()
	raw := hpa.Annotations[annotationHPAOriginalContainerMemory]
	var entries map[string]hpaContainerMemoryEntry
	require.NoError(t, json.Unmarshal([]byte(raw), &entries))
	return entries
}

func TestRetuneHPAMemory_SingleContainer(t *testing.T) {
	t.Parallel()
	max90 := int32(90)
	min120 := int32(120)
	cases := []struct {
		name   string
		limit  string
		bounds *attunev1alpha1.HPATargetBounds
		want   int32
		event  string
	}{
		{name: "limit 2Gi publishes 160", limit: "2Gi", want: 160},
		{name: "limit equals request publishes 100", limit: "512Mi", want: 100},
		{name: "no limit publishes 100", want: 100},
		{name: "limit below request publishes 100", limit: "256Mi", want: 100},
		{
			name:   "memory max 90 clamps 160",
			limit:  "2Gi",
			bounds: memoryBounds(nil, &max90),
			want:   90,
			event:  "Normal HPATargetClamped HPA default/api-server-hpa memory target 160 clamped to 90",
		},
		{
			name:   "cpu max does not change memory",
			limit:  "2Gi",
			bounds: &attunev1alpha1.HPATargetBounds{CPU: &attunev1alpha1.HPATargetBound{Max: &max90}},
			want:   160,
		},
		{
			name:   "user min cannot exceed the limit cap",
			limit:  "512Mi",
			bounds: memoryBounds(&min120, nil),
			want:   100,
		},
		{
			name:   "user max and limit cap both bind",
			limit:  "512Mi",
			bounds: memoryBounds(nil, &max90),
			want:   90,
			event:  "Normal HPATargetClamped HPA default/api-server-hpa memory target 100 clamped to 90",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
			pod := workloadPod("api-server", memContainer(t, "app", "1Gi", tc.limit))
			recorder := events.NewFakeRecorder(4)
			cl, updates := runMemoryRetune(t, memoryRetuneOpts{
				policy:   policyWithBounds(tc.bounds),
				hpas:     []autoscalingv2.HorizontalPodAutoscaler{hpa},
				pod:      &pod,
				history:  []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
				recorder: recorder,
			})
			updated := storedHPA(t, cl, "api-server-hpa")
			assert.Equal(t, tc.want, metricUtil(t, updated, 0))
			assert.NotEqual(t, int32(50), metricUtil(t, updated, 0))
			assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalMemory])
			assert.Equal(t, "1Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
			assert.Empty(t, updated.Annotations[annotationHPAOriginalContainerMemory])
			assert.Equal(t, 1, updates)
			if tc.event == "" {
				assert.Empty(t, recorderNotes(recorder))
			} else {
				assert.Equal(t, []string{tc.event}, recorderNotes(recorder))
			}
		})
	}
}

func TestRetuneHPAMemory_ResourceSumAndContainerOneUpdate(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		memoryContainerMetric("app", 80),
		memoryContainerMetric("sidecar", 80),
	)
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0), "80 * 2048/1536 truncates to 106, not 160")
	assert.Equal(t, int32(160), metricUtil(t, updated, 1), "app ContainerResource is 80 * 1024/512")
	assert.Equal(t, int32(80), metricUtil(t, updated, 2), "sidecar request did not change")
	assert.Equal(t, 1, updates)
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalMemory])
	assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	entries := containerMemory(t, updated)
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, entries["app"])
	_, hasSidecar := entries["sidecar"]
	assert.False(t, hasSidecar)
}

func TestRetuneHPAMemory_SwappedMetricOrderOneUpdate(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryContainerMetric("app", 80),
		memoryResourceMetric(80),
	)
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Equal(t, int32(106), metricUtil(t, updated, 1))
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_SecondResizeUsesStoredResourceBase(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	firstPod := workloadPod("api-server", memContainer(t, "app", "768Mi", "2Gi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &firstPod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "768Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0), "80 * 1024/768 truncates to 106")
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalMemory])
	assert.Equal(t, "1Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])

	secondPod := workloadPod("api-server", memContainer(t, "app", "512Mi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{updated},
		pod:     &secondPod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "768Mi", "512Mi")},
	})
	final := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, final, 0), "80 * 1024/512; 106 * 768/512 = 159 must not win")
	assert.NotEqual(t, int32(159), metricUtil(t, final, 0))
	assert.Equal(t, "80", final.Annotations[annotationHPAOriginalMemory])
	assert.Equal(t, "1Gi", final.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_SecondResizeUsesStoredContainerJSON(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryContainerMetric("app", 80))
	firstPod := workloadPod("api-server", memContainer(t, "app", "768Mi", "2Gi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &firstPod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "768Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0))
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, containerMemory(t, updated)["app"])

	secondPod := workloadPod("api-server", memContainer(t, "app", "512Mi", "2Gi"))
	cl, _ = runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{updated},
		pod:     &secondPod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "768Mi", "512Mi")},
	})
	final := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, final, 0))
	assert.NotEqual(t, int32(159), metricUtil(t, final, 0))
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, containerMemory(t, final)["app"])
}

func TestRetuneHPAMemory_AutoTuneAbsent(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	delete(hpa.Annotations, annotationHPAAutoTune)
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemory])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalContainerMemory])
	assert.Equal(t, 0, updates)
}

func TestRetuneHPAMemory_MemoryMaxDoesNotChangeCPU(t *testing.T) {
	t.Parallel()
	max90 := int32(90)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(80),
		memoryResourceMetric(80),
	)
	pod := workloadPod("api-server",
		podContainer(t, "app", "400m", "1000m"),
		podContainer(t, "sidecar", "200m", "1000m"),
	)
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		policy:  policyWithBounds(memoryBounds(nil, &max90)),
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{cpuHistory("app", "400m", "200m")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(120), metricUtil(t, updated, 0), "80 * 600/400 stays 120 when only memory max is set")
	assert.Equal(t, int32(80), metricUtil(t, updated, 1))
}

func TestRetuneHPAMemory_ObjectAndAverageValueStay(t *testing.T) {
	t.Parallel()
	average := qty(t, "100Mi")
	objectValue := qty(t, "10")
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		autoscalingv2.MetricSpec{
			Type: autoscalingv2.ObjectMetricSourceType,
			Object: &autoscalingv2.ObjectMetricSource{
				DescribedObject: autoscalingv2.CrossVersionObjectReference{Kind: "Service", Name: "api"},
				Metric:          autoscalingv2.MetricIdentifier{Name: "queue"},
				Target: autoscalingv2.MetricTarget{
					Type:  autoscalingv2.ValueMetricType,
					Value: &objectValue,
				},
			},
		},
		autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{
					Type:         autoscalingv2.AverageValueMetricType,
					AverageValue: &average,
				},
			},
		},
	)
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Equal(t, 1, updates)
	require.NotNil(t, updated.Spec.Metrics[1].Object.Target.Value)
	assert.Equal(t, int64(10), updated.Spec.Metrics[1].Object.Target.Value.Value())
	require.NotNil(t, updated.Spec.Metrics[2].Resource.Target.AverageValue)
	assert.Equal(t, average.String(), updated.Spec.Metrics[2].Resource.Target.AverageValue.String())
	assert.Nil(t, updated.Spec.Metrics[2].Resource.Target.AverageUtilization)
}

func TestRetuneHPAMemory_LongContainerNameUsesJSON(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("c", 63)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryContainerMetric(name, 80))
	pod := workloadPod("api-server", memContainer(t, name, "1Gi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory(name, "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Equal(t, 1, updates)
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, containerMemory(t, updated)[name])
	for key := range updated.Annotations {
		assert.NotContains(t, key, name)
	}
}

func TestRetuneHPAMemory_PreservesExistingSidecarJSON(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalContainerMemory: `{"sidecar":{"target":"70","request":"256Mi"}}`,
	}, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	entries := containerMemory(t, storedHPA(t, cl, "api-server-hpa"))
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, entries["app"])
	assert.Equal(t, hpaContainerMemoryEntry{Target: "70", Request: "256Mi"}, entries["sidecar"])
}

func TestRetuneHPAMemory_CorruptJSONIsNotRewritten(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", map[string]string{
		annotationHPAOriginalContainerMemory: `{`,
	}, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Equal(t, `{`, updated.Annotations[annotationHPAOriginalContainerMemory])
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_InitExcludedFromPodTotal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		memoryContainerMetric("migrate", 80),
	)
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	pod.Spec.InitContainers = []corev1.Container{memContainer(t, "migrate", "1Gi", "2Gi")}
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas: []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:  &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
			memoryHistory("migrate", "1Gi", "512Mi"),
		},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0), "init memory stays out of the pod total")
	assert.Equal(t, int32(160), metricUtil(t, updated, 1))
	assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, hpaContainerMemoryEntry{Target: "80", Request: "1Gi"}, containerMemory(t, updated)["migrate"])
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_NativeSidecarInPodTotal(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	proxy := memContainer(t, "istio-proxy", "1Gi", "2Gi")
	always := corev1.ContainerRestartPolicyAlways
	proxy.RestartPolicy = &always
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	pod.Spec.InitContainers = []corev1.Container{proxy}
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0),
		"native sidecar memory stays in the pod total: 80 * 2Gi/1536Mi = 106")
	assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_OffPodHistoryDilutes(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas: []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:  &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
			memoryHistory("ghost", "256Mi", "256Mi"),
		},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(102), metricUtil(t, updated, 0), "80 * 2304/1792 truncates to 102, not the on-pod 106")
}

func TestRetuneHPAMemory_BadRowSkipsResource(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		memoryContainerMetric("app", 80),
		memoryContainerMetric("sidecar", 80),
	)
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas: []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:  &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "bogus"),
			memoryHistory("sidecar", "1Gi", "512Mi"),
		},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Equal(t, int32(80), metricUtil(t, updated, 1))
	assert.Equal(t, int32(160), metricUtil(t, updated, 2))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemory])
}

func TestRetuneHPAMemory_RequestsAndLimitsUsesRecommendedLimit(t *testing.T) {
	t.Parallel()
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.Memory.ControlledValues = &both
	recommended := qty(t, "2Gi")
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server", memContainer(t, "app", "512Mi", "512Mi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		policy: policy,
		hpas:   []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:    &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
		},
		recs: []attunev1alpha1.WorkloadRecommendation{{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "app",
				Recommended: attunev1alpha1.ResourceValues{
					MemoryLimit: recommended,
				},
			}},
		}},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "recommended 2Gi cap is 400, so 160 stands; the live 512Mi limit would publish 100")
}

func TestRetuneHPAMemory_ContainerCapIgnoresSidecarLimit(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		memoryContainerMetric("app", 80),
	)
	pod := workloadPod("api-server",
		memContainer(t, "app", "512Mi", "512Mi"),
		memContainer(t, "sidecar", "1Gi", "8Gi"),
	)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(106), metricUtil(t, updated, 0), "pod limit sum still leaves room for 106")
	assert.Equal(t, int32(100), metricUtil(t, updated, 1), "app limit equals the new request, so 160 caps at 100")
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_ResourceNoLimitCapsAt100(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", ""),
		memContainer(t, "sidecar", "1Gi", ""),
	)
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	assert.Equal(t, int32(100), metricUtil(t, storedHPA(t, cl, "api-server-hpa"), 0), "106 without a pod limit caps at 100")
}

func TestRetuneHPAMemory_SidecarResizeDoesNotMoveAppContainer(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server",
		memContainer(t, "app", "1Gi", "2Gi"),
		memContainer(t, "sidecar", "1Gi", "2Gi"),
	)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("sidecar", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalContainerMemory])
	assert.Equal(t, 0, updates)
}

func TestRetuneHPAMemory_MemoryOnlyPodIsSelected(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas:    []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:     &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0))
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_BandMatchingCurrentSkipsRememberAndEvent(t *testing.T) {
	t.Parallel()
	max90 := int32(90)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(90),
		cpuResourceMetric(80),
	)
	pod := workloadPod("api-server", mixedContainer(t, "app", "400m", "1", "1Gi", "2Gi"))
	recorder := events.NewFakeRecorder(4)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		policy:   policyWithBounds(memoryBounds(nil, &max90)),
		hpas:     []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:      &pod,
		history:  []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi"), cpuHistory("app", "400m", "200m")},
		recorder: recorder,
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(90), metricUtil(t, updated, 0))
	assert.Equal(t, int32(160), metricUtil(t, updated, 1))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemory])
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, "80", updated.Annotations[annotationHPAOriginalCPU])
	assert.Empty(t, recorderNotes(recorder))
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_FailedUpdateEmitsNoEvent(t *testing.T) {
	t.Parallel()
	max90 := int32(90)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryResourceMetric(80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	recorder := events.NewFakeRecorder(4)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		policy:     policyWithBounds(memoryBounds(nil, &max90)),
		hpas:       []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:        &pod,
		history:    []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
		recorder:   recorder,
		failUpdate: true,
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(80), metricUtil(t, updated, 0))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemory])
	assert.Empty(t, recorderNotes(recorder))
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_CPUAndMemoryOneUpdate(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(80),
		memoryResourceMetric(80),
	)
	pod := workloadPod("api-server",
		mixedContainer(t, "app", "400m", "1", "1Gi", "2Gi"),
		mixedContainer(t, "sidecar", "200m", "1", "1Gi", "2Gi"),
	)
	cl, updates := runMemoryRetune(t, memoryRetuneOpts{
		hpas: []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:  &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			cpuHistory("app", "400m", "200m"),
			memoryHistory("app", "1Gi", "512Mi"),
		},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(120), metricUtil(t, updated, 0), "80 * 600/400")
	assert.Equal(t, int32(106), metricUtil(t, updated, 1), "80 * 2048/1536")
	assert.Equal(t, "600m", updated.Annotations[annotationHPAOriginalCPURequest])
	assert.Equal(t, "2Gi", updated.Annotations[annotationHPAOriginalMemoryRequest])
	assert.Equal(t, 1, updates)
}

func TestRetuneHPAMemory_ContainerClampNamesTheContainer(t *testing.T) {
	t.Parallel()
	max90 := int32(90)
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server", memContainer(t, "app", "1Gi", "2Gi"))
	recorder := events.NewFakeRecorder(4)
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		policy:   policyWithBounds(memoryBounds(nil, &max90)),
		hpas:     []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:      &pod,
		history:  []attunev1alpha1.ResizeHistoryEntry{memoryHistory("app", "1Gi", "512Mi")},
		recorder: recorder,
	})
	assert.Equal(t, int32(90), metricUtil(t, storedHPA(t, cl, "api-server-hpa"), 0))
	assert.Equal(t, []string{
		"Normal HPATargetClamped HPA default/api-server-hpa memory container app target 160 clamped to 90",
	}, recorderNotes(recorder))
}

func TestAdjustHPATargets_ScalarDoesNotRetuneMemory(t *testing.T) {
	t.Parallel()
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		cpuResourceMetric(80),
		memoryResourceMetric(80),
	)
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa.DeepCopy()).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = cl
	reconciler.Scheme = scheme
	reconciler.adjustHPATargets(context.Background(), []autoscalingv2.HorizontalPodAutoscaler{hpa},
		"api-server", "Deployment", qty(t, "400m"), qty(t, "200m"), qty(t, "2"))
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "80 * 400/200")
	assert.Equal(t, int32(80), metricUtil(t, updated, 1))
	assert.Empty(t, updated.Annotations[annotationHPAOriginalMemory])
}

func TestPublishHPATarget(t *testing.T) {
	t.Parallel()
	limit := qty(t, "2Gi")
	request := qty(t, "512Mi")
	same := qty(t, "512Mi")
	max90 := int32(90)
	min120 := int32(120)

	published, computed, emit := publishHPATarget(160, limit, request, nil)
	assert.Equal(t, int32(160), published)
	assert.Equal(t, int32(160), computed)
	assert.False(t, emit)

	published, computed, emit = publishHPATarget(160, limit, request, &attunev1alpha1.HPATargetBound{})
	assert.Equal(t, int32(160), published)
	assert.Equal(t, int32(160), computed)
	assert.False(t, emit)

	published, computed, emit = publishHPATarget(160, limit, request, &attunev1alpha1.HPATargetBound{Max: &max90})
	assert.Equal(t, int32(90), published)
	assert.Equal(t, int32(160), computed)
	assert.True(t, emit)

	published, computed, emit = publishHPATarget(0, limit, request, &attunev1alpha1.HPATargetBound{Max: &max90})
	assert.Equal(t, int32(1), published)
	assert.Equal(t, int32(1), computed)
	assert.False(t, emit)

	published, computed, emit = publishHPATarget(160, same, same, &attunev1alpha1.HPATargetBound{Max: &max90})
	assert.Equal(t, int32(90), published)
	assert.Equal(t, int32(100), computed)
	assert.True(t, emit)

	published, computed, emit = publishHPATarget(160, same, same, &attunev1alpha1.HPATargetBound{Min: &min120})
	assert.Equal(t, int32(100), published)
	assert.Equal(t, int32(100), computed)
	assert.False(t, emit)
}

func TestHPAMemoryAnnotationNames(t *testing.T) {
	t.Parallel()
	keys := []string{
		annotationHPAOriginalMemory,
		annotationHPAOriginalMemoryRequest,
		annotationHPAOriginalContainerMemory,
	}
	for _, key := range keys {
		name := key[strings.LastIndex(key, "/")+1:]
		assert.LessOrEqual(t, len(name), 63, key)
		assert.Regexp(t, `^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`, name)
		assert.False(t, strings.HasPrefix(key, annotationOriginalMemoryPrefix), key)
	}
	assert.Equal(t, "attune.io/original-target-memory", annotationHPAOriginalMemory)
	assert.Equal(t, "attune.io/original-memory-request", annotationHPAOriginalMemoryRequest)
	assert.Equal(t, "attune.io/original-container-memory", annotationHPAOriginalContainerMemory)
	assert.NotEqual(t, annotationOriginalMemoryPrefix, annotationHPAOriginalMemoryRequest)
}

func TestRetuneHPAMemory_ContainerRequestsAndLimitsCapsBothMetrics(t *testing.T) {
	t.Parallel()
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "app",
		Memory:        &attunev1alpha1.ResourceConfig{ControlledValues: &both},
	}}
	recommended := qty(t, "2Gi")
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil,
		memoryResourceMetric(80),
		memoryContainerMetric("app", 80),
	)
	pod := workloadPod("api-server", memContainer(t, "app", "512Mi", "512Mi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		policy: policy,
		hpas:   []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:    &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
		},
		recs: []attunev1alpha1.WorkloadRecommendation{{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "app",
				Recommended: attunev1alpha1.ResourceValues{
					MemoryLimit: recommended,
				},
			}},
		}},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(160), metricUtil(t, updated, 0), "pod metric uses the app RequestsAndLimits cap")
	assert.Equal(t, int32(160), metricUtil(t, updated, 1), "container metric uses the app RequestsAndLimits cap")
}

func TestRetuneHPAMemory_ContainerRequestsOnlyOverridesPolicy(t *testing.T) {
	t.Parallel()
	both := attunev1alpha1.ControlledRequestsAndLimits
	only := attunev1alpha1.ControlledRequestsOnly
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.Memory.ControlledValues = &both
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "app",
		Memory:        &attunev1alpha1.ResourceConfig{ControlledValues: &only},
	}}
	recommended := qty(t, "2Gi")
	hpa := newAutoTuneHPA("api-server-hpa", "Deployment", nil, memoryContainerMetric("app", 80))
	pod := workloadPod("api-server", memContainer(t, "app", "512Mi", "512Mi"))
	cl, _ := runMemoryRetune(t, memoryRetuneOpts{
		policy: policy,
		hpas:   []autoscalingv2.HorizontalPodAutoscaler{hpa},
		pod:    &pod,
		history: []attunev1alpha1.ResizeHistoryEntry{
			memoryHistory("app", "1Gi", "512Mi"),
		},
		recs: []attunev1alpha1.WorkloadRecommendation{{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "app",
				Recommended: attunev1alpha1.ResourceValues{
					MemoryLimit: recommended,
				},
			}},
		}},
	})
	updated := storedHPA(t, cl, "api-server-hpa")
	assert.Equal(t, int32(100), metricUtil(t, updated, 0), "container RequestsOnly keeps the live 512Mi cap")
}
