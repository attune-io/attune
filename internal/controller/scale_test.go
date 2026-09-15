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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func hpaWithScaledToZero(status corev1.ConditionStatus) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
				Type:   autoscalingv2.ScaledToZero,
				Status: status,
			}},
		},
	}
}

func TestClassifyWorkloadScale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		specReplicas *int32
		hpa          *autoscalingv2.HorizontalPodAutoscaler
		want         workloadScaleState
		wantIdle     bool
	}{
		{
			name:         "ScaledToZero True with nil spec is HPA zero",
			specReplicas: nil,
			hpa:          hpaWithScaledToZero(corev1.ConditionTrue),
			want:         scaleHPAZero,
			wantIdle:     true,
		},
		{
			name:         "ScaledToZero True with spec greater than 0 is HPA zero",
			specReplicas: int32Ptr(3),
			hpa:          hpaWithScaledToZero(corev1.ConditionTrue),
			want:         scaleHPAZero,
			wantIdle:     true,
		},
		{
			name:         "spec.replicas 0 and no ScaledToZero is manual zero",
			specReplicas: int32Ptr(0),
			hpa:          nil,
			want:         scaleManualZero,
			wantIdle:     true,
		},
		{
			name:         "spec.replicas greater than 0 and no condition is active",
			specReplicas: int32Ptr(2),
			hpa:          nil,
			want:         scaleActive,
			wantIdle:     false,
		},
		{
			name:         "status.replicas is ignored; spec greater than 0 is active",
			specReplicas: int32Ptr(2),
			hpa:          nil,
			want:         scaleActive,
			wantIdle:     false,
		},
		{
			name:         "ScaledToZero False with spec 0 is manual zero",
			specReplicas: int32Ptr(0),
			hpa:          hpaWithScaledToZero(corev1.ConditionFalse),
			want:         scaleManualZero,
			wantIdle:     true,
		},
		{
			name:         "nil spec and no HPA is active",
			specReplicas: nil,
			hpa:          nil,
			want:         scaleActive,
			wantIdle:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyWorkloadScale(tt.specReplicas, tt.hpa)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantIdle, got.idle())
		})
	}
}

func TestClassifyWorkloadScale_DoesNotReadStatusReplicas(t *testing.T) {
	t.Parallel()
	// Classifier takes spec only. A caller that passed status.replicas
	// would be a bug; this documents the contract.
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(3)
	deploy.Status.Replicas = 0
	got := classifyWorkloadScale(workloadSpecReplicas(deploy), nil)
	assert.Equal(t, scaleActive, got)
}

func TestExecuteResizes_IdleScaledToZeroLeavesRunningPods(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api-hpa", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "api-server",
			},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
				Type:   autoscalingv2.ScaledToZero,
				Status: corev1.ConditionTrue,
			}},
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod, hpa).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "200m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)

	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "resize", a.GetSubresource(), "idle ScaledToZero must not resize leftover pods")
		if a.GetVerb() == "create" {
			_, isEvict := a.(k8stesting.CreateAction)
			if isEvict && a.GetResource().Resource == "pods" {
				assert.NotEqual(t, "eviction", a.GetSubresource(), "idle ScaledToZero must not evict leftover pods")
			}
		}
	}
}

func TestExecuteResizes_ManualZeroSkipsResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(0)
	deploy.Status.Replicas = 1

	reconciler, _ := newResizeReconciler(pod, deploy)
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "200m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)
}

func TestExecuteResizes_StatusReplicasZeroStillActive(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(2)
	deploy.Status.Replicas = 0

	reconciler, _ := newResizeReconciler(pod, deploy)
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "200m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 1, count)
	require.NotEmpty(t, history)
}

func TestExecuteResizes_HPAListErrorSkipsResize(t *testing.T) {
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					return fmt.Errorf("simulated HPA list failure")
				}
				return cw.List(ctx, list, opts...)
			},
		}).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	recommendations := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "500m", "512Mi", "1000m", "1Gi", "200m", "256Mi", "400m", "512Mi"),
	}

	count, history := reconciler.executeResizes(context.Background(), policy,
		[]client.Object{deploy}, recommendations, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Empty(t, history)

	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "resize", a.GetSubresource(), "HPA list error must not resize leftover pods")
	}
}
