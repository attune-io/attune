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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// RequestsOnly on a Burstable chart whose memory request already equals
// its limit becomes Guaranteed when the CPU request rises to the CPU
// limit. The skip must happen before UpdateResize so InPlaceOrRecreate
// does not evict, and the reason must not suggest RequestsAndLimits.
func TestExecuteResizes_BurstableChartSkipsQoSWithoutEviction(t *testing.T) {
	pod := newResizePod("chart", "250m", "512Mi", "500m", "512Mi")
	pod.Status.QOSClass = corev1.PodQOSBurstable
	deploy := newTestDeployment("chart", "default", map[string]string{"app": "chart"})
	reconciler, _ := newResizeReconciler(pod, deploy)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("chart", "250m", "512Mi", "500m", "512Mi", "500m", "512Mi", "0", "0"),
	}

	cpuReq, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memReq, err := resource.ParseQuantity("512Mi")
	require.NoError(t, err)
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    cpuReq,
			corev1.ResourceMemory: memReq,
		},
	}
	containerRec := recommendations[0].Containers[0]
	skip, reason := reconciler.shouldSkipResize(context.Background(), pod, containerRec, target, nil)
	require.True(t, skip)
	require.Equal(t, "would change QoS class from Burstable to Guaranteed", reason)

	count, history := reconciler.executeResizes(context.Background(), policy, []client.Object{deploy}, recommendations, podMap("chart", pod), nil, nil)
	require.Equal(t, 0, count)
	require.Empty(t, history)

	cs := reconciler.Clientset.(*kubefake.Clientset)
	for _, action := range cs.Actions() {
		require.NotEqual(t, "resize", action.GetSubresource(), "QoS skip must not call UpdateResize")
		require.NotEqual(t, "eviction", action.GetSubresource(), "QoS skip must not evict")
	}

	select {
	case ev := <-recorder.Events:
		require.Contains(t, ev, "ResizeSkipped")
		require.Contains(t, ev, "from Burstable to Guaranteed")
		require.NotContains(t, ev, "RequestsAndLimits")
	default:
		t.Fatal("QoS skip must emit ResizeSkipped")
	}
}
