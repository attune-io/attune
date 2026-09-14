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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/cluster"
	"github.com/attune-io/attune/internal/resize"
)

func withPodEnvelope(pod *corev1.Pod, cpuReq, memReq, cpuLim, memLim string) *corev1.Pod {
	env := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
	}
	if cpuLim != "" || memLim != "" {
		env.Limits = corev1.ResourceList{}
		if cpuLim != "" {
			env.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
		}
		if memLim != "" {
			env.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
		}
	}
	pod.Spec.Resources = env
	return pod
}

func firstResizeUpdate(actions []k8stesting.Action) *corev1.Pod {
	for _, action := range actions {
		if action.GetVerb() == "update" && action.GetSubresource() == "resize" {
			return action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		}
	}
	return nil
}

func TestExecuteResizes_EnvelopeInPlaceOff_SkipsIncrease(t *testing.T) {
	pod := withPodEnvelope(newResizePod("api-server", "200m", "256Mi", "1000m", "1Gi"),
		"200m", "256Mi", "", "")
	pod.Status.QOSClass = corev1.PodQOSBurstable
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server",
			"200m", "256Mi", "1000m", "1Gi",
			"500m", "256Mi", "1000m", "1Gi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	require.NotEmpty(t, history)
	assert.Equal(t, attunev1alpha1.ResizeResultFailed, history[0].Result)
	assert.Equal(t, resize.ReasonEnvelopeConstraint, history[0].Reason)

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, resize.EnvelopeSkipMessage) {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped with envelope message")
			return
		}
	}
}

func TestExecuteResizes_EnvelopeInPlaceOn_RaisesInSameUpdate(t *testing.T) {
	pod := withPodEnvelope(newResizePod("api-server", "200m", "256Mi", "1000m", "1Gi"),
		"200m", "256Mi", "", "")
	pod.Status.QOSClass = corev1.PodQOSBurstable
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	reconciler.Capabilities = &cluster.Capabilities{InPlacePodLevelResources: true}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server",
			"200m", "256Mi", "1000m", "1Gi",
			"500m", "256Mi", "1000m", "1Gi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, history)

	cs := reconciler.Clientset.(*kubefake.Clientset)
	got := firstResizeUpdate(cs.Actions())
	require.NotNil(t, got, "expected one UpdateResize")
	require.NotNil(t, got.Spec.Resources, "payload must include spec.resources")
	assert.True(t, got.Spec.Resources.Requests.Cpu().Equal(resource.MustParse("500m")),
		"envelope cpu request should rise to 500m, got %s", got.Spec.Resources.Requests.Cpu().String())
	assert.True(t, got.Spec.Containers[0].Resources.Requests.Cpu().Equal(resource.MustParse("500m")),
		"container cpu should be in the same payload, got %s", got.Spec.Containers[0].Resources.Requests.Cpu().String())
}

func TestExecuteResizes_RequestsOnlyBurstable_SkipsLimitLift(t *testing.T) {
	pod := withPodEnvelope(newResizePod("api-server", "200m", "256Mi", "400m", "512Mi"),
		"200m", "256Mi", "300m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSBurstable
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	reconciler.Capabilities = &cluster.Capabilities{InPlacePodLevelResources: true}
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server",
			"200m", "256Mi", "400m", "512Mi",
			"500m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	require.NotEmpty(t, history)
	assert.Equal(t, resize.ReasonEnvelopeConstraint, history[0].Reason)

	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "ResizeSkipped") && strings.Contains(event, "envelope") {
				found = true
			}
		default:
			require.True(t, found, "expected ResizeSkipped for RequestsOnly limit lift")
			return
		}
	}
}

func TestExecuteResizes_EnvelopeQoSUsesEnvelopeOnly(t *testing.T) {
	pod := withPodEnvelope(newResizePod("api-server", "500m", "512Mi", "500m", "512Mi"),
		"500m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	reconciler.Capabilities = &cluster.Capabilities{InPlacePodLevelResources: true}

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server",
			"500m", "512Mi", "500m", "512Mi",
			"250m", "256Mi", "500m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count, "container request!=limit must not block when envelope stays Guaranteed")
	assert.NotEmpty(t, history)
}

func TestEvaluatePodEnvelope_NilCapabilitiesIsSafeDefaults(t *testing.T) {
	r := NewAttunePolicyReconciler()
	assert.False(t, r.inPlacePodLevelResources())

	pod := withPodEnvelope(newResizePod("api-server", "200m", "256Mi", "1000m", "1Gi"),
		"200m", "256Mi", "", "")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	dec := r.evaluatePodEnvelope(newTestPolicy("p", "default"), pod, "main", target)
	assert.True(t, dec.Skip)
	assert.Equal(t, resize.ReasonEnvelopeConstraint, dec.Reason)
}
