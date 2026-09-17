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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/resize"
)

func TestStartupBoostBlocksCPUDecrease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("p", "default")
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 2 * time.Minute},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-0",
			Annotations: map[string]string{
				annotationStartupBoostAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			}},
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	assert.Equal(t, "startup boost window",
		startupBoostBlocksCPUDecrease(policy, pod, "app", target, now))

	pod.Annotations[annotationStartupBoostAt] = now.Add(-3 * time.Minute).Format(time.RFC3339)
	assert.Empty(t, startupBoostBlocksCPUDecrease(policy, pod, "app", target, now))
}

func TestStartupBoostBlocksCPUDecrease_MalformedAnnotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	policy := newTestPolicy("p", "default")
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 2 * time.Minute},
	}
	cpuLive, err := resource.ParseQuantity("1")
	require.NoError(t, err)
	cpuTarget, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-0",
			Annotations: map[string]string{
				annotationStartupBoostAt: "not-a-valid-timestamp",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: cpuLive,
					},
				},
			}},
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: cpuTarget,
		},
	}
	assert.Equal(t, "startup boost window",
		startupBoostBlocksCPUDecrease(policy, pod, "app", target, now),
		"malformed startup-boost-at must block CPU decrease")
}

func TestApplyStartupBoosts_IdleScaledToZeroLeavesRunningPods(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cpuReq, err := resource.ParseQuantity("100m")
	require.NoError(t, err)
	memReq, err := resource.ParseQuantity("128Mi")
	require.NoError(t, err)
	recCPU, err := resource.ParseQuantity("200m")
	require.NoError(t, err)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "api-server-leftover",
			Namespace:         "default",
			Labels:            map[string]string{"app": "api-server"},
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    cpuReq,
						corev1.ResourceMemory: memReq,
					},
				},
			}},
		},
	}
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

	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod, hpa).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: recCPU,
			},
		}},
	}}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"api-server": {*pod}},
		recs, resize.NewPodResizer(clientset, ctrl.Log.WithName("test")), nil)

	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "resize", a.GetSubresource(), "idle ScaledToZero must not resize leftover pods")
	}
}

func TestApplyStartupBoosts_HPAListErrorSkipsBoost(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cpuReq, err := resource.ParseQuantity("100m")
	require.NoError(t, err)
	memReq, err := resource.ParseQuantity("128Mi")
	require.NoError(t, err)
	recCPU, err := resource.ParseQuantity("200m")
	require.NoError(t, err)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "api-server-leftover",
			Namespace:         "default",
			Labels:            map[string]string{"app": "api-server"},
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    cpuReq,
						corev1.ResourceMemory: memReq,
					},
				},
			}},
		},
	}

	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cw client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					return fmt.Errorf("simulated HPA list failure")
				}
				return cw.List(ctx, list, opts...)
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api-server",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "main",
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest: recCPU,
			},
		}},
	}}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"api-server": {*pod}},
		recs, resize.NewPodResizer(clientset, ctrl.Log.WithName("test")), nil)

	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "resize", a.GetSubresource(), "HPA list error must not resize leftover pods")
	}
}
