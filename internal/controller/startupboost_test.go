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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
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

func TestApplyStartupBoosts_AppliesBoostToNewPod(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)), // 30s old
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"),
					},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify resize was attempted via clientset actions and memory request preserved.
	actions := clientset.Actions()
	var foundResize bool
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			reqs := updatedPod.Spec.Containers[0].Resources.Requests
			assert.True(t, reqs.Cpu().Cmp(resource.MustParse("100m")) > 0, "CPU should be boosted above 100m")
			memReq := reqs[corev1.ResourceMemory]
			assert.Equal(t, resource.MustParse("128Mi"), memReq, "memory request should be preserved")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for startup boost")
}

func TestApplyStartupBoosts_SkipsWhenAlreadyAtBoostedLevel(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "2.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("50m"),
					},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("startup boost must not resize when current CPU is already at 2*recommendation")
		}
	}

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "my-app-abc", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, now.UTC().Format(time.RFC3339), updated.Annotations[annotationStartupBoostAt],
		"already-at-boosted inside the window must persist a startup-boost annotation so expiry can run")
}

func TestApplyStartupBoosts_RetriesAnnotationWhenAlreadyBoosted(t *testing.T) {
	// After a successful boost resize, if annotation persist fails, the
	// next reconcile sees current >= boosted and must retry the annotation
	// without a second /resize.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"),
					},
				},
			},
		},
	}

	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	failClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return fmt.Errorf("simulated persist failure")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = failClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*pod}}, recs, resizer, nil)

	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			break
		}
	}
	require.True(t, foundResize, "first apply must resize to the boosted CPU")

	boosted, err := clientset.CoreV1().Pods("default").Get(context.Background(), "my-app-abc", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, boosted.Spec.Containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("100m")) > 0,
		"clientset pod should already be at boosted CPU after the first apply")
	assert.Empty(t, boosted.Annotations[annotationStartupBoostAt],
		"annotation persist failed on the first apply")

	// Second apply: current is already boosted, persist must be retried.
	clientset2 := kubefake.NewSimpleClientset(boosted.DeepCopy())
	okClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(boosted.DeepCopy()).Build()
	r.Client = okClient
	r.Clientset = clientset2
	resizer2 := resize.NewPodResizer(clientset2, logger)
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*boosted}}, recs, resizer2, nil)

	for _, a := range clientset2.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("second apply must not resize when current CPU is already boosted")
		}
	}
	var updated corev1.Pod
	err = okClient.Get(context.Background(), types.NamespacedName{
		Name: "my-app-abc", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, now.UTC().Format(time.RFC3339), updated.Annotations[annotationStartupBoostAt],
		"second apply must retry the startup-boost annotation")
}

func TestApplyStartupBoosts_SkipsBatchWorkload(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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

	tests := []struct {
		kind string
	}{
		{kind: "Job"},
		{kind: "CronJob"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "batch-pod",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "main",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			}
			clientset := kubefake.NewSimpleClientset(pod)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.Clientset = clientset
			r.SetNowFunc(func() time.Time { return now })

			recs := []attunev1alpha1.WorkloadRecommendation{
				{
					Workload: "batch-job",
					Kind:     tt.kind,
					Containers: []attunev1alpha1.ContainerRecommendation{
						{
							Name: "main",
							Recommended: attunev1alpha1.ResourceValues{
								CPURequest: resource.MustParse("200m"),
							},
						},
					},
				},
			}
			r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"batch-job": {*pod}},
				recs, resize.NewPodResizer(clientset, ctrl.Log.WithName("test")), nil)

			for _, a := range clientset.Actions() {
				if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
					t.Fatalf("startup boost must not resize %s pods", tt.kind)
				}
			}
			var updated corev1.Pod
			err := fakeClient.Get(context.Background(), types.NamespacedName{
				Name: "batch-pod", Namespace: "default",
			}, &updated)
			require.NoError(t, err)
			assert.Empty(t, updated.Annotations[annotationStartupBoostAt],
				"batch workload must not get a startup-boost annotation")
		})
	}
}

func TestApplyStartupBoosts_NativeSidecars(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	boostAt := now.Add(-3 * time.Minute)

	tests := []struct {
		name       string
		boosted    bool
		currentCPU string
		wantCPU    string
	}{
		{
			name:       "apply boosts sidecar and app",
			boosted:    false,
			currentCPU: "100m",
			wantCPU:    "600m",
		},
		{
			name:       "expiry reduces sidecar and app",
			boosted:    true,
			currentCPU: "600m",
			wantCPU:    "200m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
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
			meta := metav1.ObjectMeta{
				Name:              "my-app-abc",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
			}
			if tt.boosted {
				meta.CreationTimestamp = metav1.NewTime(boostAt)
				meta.Annotations = map[string]string{
					annotationStartupBoostAt: boostAt.UTC().Format(time.RFC3339),
				}
			}
			cpu := resource.MustParse(tt.currentCPU)
			mem := resource.MustParse("128Mi")
			reqs := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: meta,
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{
						Name:          "sidecar",
						RestartPolicy: &always,
						Resources:     reqs,
					}},
					Containers: []corev1.Container{{
						Name:      "app",
						Resources: reqs,
					}},
				},
			}
			clientset := kubefake.NewSimpleClientset(pod)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.Clientset = clientset
			r.SetNowFunc(func() time.Time { return now })

			recs := []attunev1alpha1.WorkloadRecommendation{{
				Workload: "my-app",
				Kind:     "Deployment",
				Containers: []attunev1alpha1.ContainerRecommendation{
					{Name: "sidecar", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
					{Name: "app", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
				},
			}}
			r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*pod}}, recs, resize.NewPodResizer(clientset, ctrl.Log.WithName("test")), nil)

			wantCPU := resource.MustParse(tt.wantCPU)
			resized := map[string]bool{}
			for _, a := range clientset.Actions() {
				if a.GetVerb() != "update" || a.GetSubresource() != "resize" {
					continue
				}
				updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
				for _, c := range append(append([]corev1.Container{}, updated.Spec.InitContainers...), updated.Spec.Containers...) {
					if c.Resources.Requests.Cpu().Equal(wantCPU) {
						resized[c.Name] = true
					}
				}
			}
			assert.True(t, resized["sidecar"], "native sidecar %q must be resized to %s", "sidecar", tt.wantCPU)
			assert.True(t, resized["app"], "regular container %q must be resized to %s", "app", tt.wantCPU)
		})
	}
}

func TestApplyStartupBoosts_SkipsStaleRecommendation(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Stale:    true,
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("200m"),
					},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("startup boost must not resize from a stale recommendation")
		}
	}

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "my-app-abc", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.Empty(t, updated.Annotations[annotationStartupBoostAt],
		"stale rec must not persist a startup-boost annotation")
}

func TestApplyStartupBoosts_SkipsDuringCanaryInProgress(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeCanary},
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Canary: &attunev1alpha1.CanaryStatus{
				Phase: attunev1alpha1.CanaryPhaseInProgress,
				Workloads: []attunev1alpha1.CanaryWorkloadStatus{
					{Workload: "my-app", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"canary-only"}},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-new",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, logr.Discard())
	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
		}},
	}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*pod}}, recs, resizer, nil)

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("startup boost must not resize a non-canary pod during CanaryInProgress")
		}
	}
}

func TestApplyStartupBoosts_PromotedSiblingStillBoosts(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeCanary},
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Canary: &attunev1alpha1.CanaryStatus{
				Phase: attunev1alpha1.CanaryPhaseInProgress,
				Workloads: []attunev1alpha1.CanaryWorkloadStatus{
					{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"app-a-canary"}},
					{Workload: "app-b", Phase: attunev1alpha1.CanaryPhaseFullRollout},
				},
			},
		},
	}
	newPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "main",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
						},
					},
				},
			},
		}
	}
	podA := newPod("app-a-new")
	podB := newPod("app-b-new")
	clientset := kubefake.NewSimpleClientset(podA, podB)
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(podA, podB).Build()
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, logr.Discard())
	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "app-a", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
		}},
		{Workload: "app-b", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
		}},
	}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{
		"app-a": {*podA},
		"app-b": {*podB},
	}, recs, resizer, nil)

	boosted := map[string]bool{}
	for _, a := range clientset.Actions() {
		if a.GetVerb() != "update" || a.GetSubresource() != "resize" {
			continue
		}
		updated := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
		boosted[updated.Name] = true
	}
	assert.False(t, boosted["app-a-new"], "unpromoted app-a must not boost a new pod")
	assert.True(t, boosted["app-b-new"], "promoted app-b must still get startup boost")
}

func TestApplyStartupBoosts_UnpromotedCanarySliceStillBoosts(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeCanary},
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Canary: &attunev1alpha1.CanaryStatus{
				Phase: attunev1alpha1.CanaryPhaseInProgress,
				Workloads: []attunev1alpha1.CanaryWorkloadStatus{
					{Workload: "app-a", Phase: attunev1alpha1.CanaryPhaseInProgress, Pods: []string{"app-a-canary"}},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "app-a-canary",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })
	resizer := resize.NewPodResizer(clientset, logr.Discard())
	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "app-a", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
		}},
	}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"app-a": {*pod}}, recs, resizer, nil)

	found := false
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			found = true
		}
	}
	assert.True(t, found, "unpromoted canary-slice pod must still get startup boost")
}

func TestApplyStartupBoosts_NaNMultiplierSkipped(t *testing.T) {
	// NaN multiplier must be treated as invalid and skip the boost entirely.
	// Before the fix, NaN <= 1 evaluated to false, so NaN passed the guard.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "NaN",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// NaN multiplier should be skipped -- no resize action should occur.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("expected no resize action for NaN multiplier")
		}
	}
}

func TestApplyStartupBoosts_SkipsPodOutsideWindow(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-old",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)), // 5 min old, outside 2m window
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify no resize action was taken.
	actions := clientset.Actions()
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Error("should not resize pod outside boost window")
		}
	}
}

func TestApplyStartupBoosts_ExpiresBoostAfterDuration(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC) // 5 minutes after boost
	boostTime := now.Add(-3 * time.Minute)             // boosted 3 min ago
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute}, // 2 min duration, expired
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-xyz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(boostTime),
			Annotations: map[string]string{
				annotationStartupBoostAt: boostTime.UTC().Format(time.RFC3339), // boost was applied
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"), // boosted
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify a resize action was taken (reducing back to steady-state).
	actions := clientset.Actions()
	var foundResize bool
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for boost expiry")
}

func TestApplyStartupBoosts_ExpiryRestoresRequestsAndLimitsDest(t *testing.T) {
	t.Parallel()
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	boostTime := now.Add(-3 * time.Minute)
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				ControlledValues: &both,
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "2.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "qos-app-xyz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(boostTime),
			Annotations: map[string]string{
				annotationStartupBoostAt: boostTime.UTC().Format(time.RFC3339),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSGuaranteed},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "qos-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"),
						CPULimit:   resource.MustParse("500m"),
					},
				},
			},
		},
	}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"qos-app": {*pod}}, recs, resizer, nil)

	got, err := clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"expiry must restore CPU request to rec dest")
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Limits.Cpu().MilliValue(),
		"expiry must restore CPU dest to rec dest")
}

func TestApplyStartupBoosts_MalformedAnnotationSkipsGracefully(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
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
	// Pod has a malformed boost timestamp annotation.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-xyz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
			Annotations: map[string]string{
				annotationStartupBoostAt: "not-a-valid-timestamp",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	logger := ctrl.Log.WithName("test")
	resizer := resize.NewPodResizer(clientset, logger)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	// Should not panic and should not attempt any resize.
	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	actions := clientset.Actions()
	for _, a := range actions {
		assert.NotEqual(t, "resize", a.GetSubresource(),
			"no resize should be attempted for a pod with malformed boost annotation")
	}
}

func TestApplyStartupBoosts_SkipsWhenExceedsNodeAllocatable(t *testing.T) {
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	// Node with only 1 CPU allocatable.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	// Pod consuming 500m, boost multiplier 3x = 1500m which exceeds node's 1 CPU.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-app-1", Namespace: "default",
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Second)),
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()
	clientset := kubefake.NewSimpleClientset(pod)
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

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
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("500m")}},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"my-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify no resize action was taken (boost would exceed node allocatable).
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("expected no resize action when boost exceeds node allocatable")
		}
	}
}

func TestApplyStartupBoosts_CapsAtCPULimit(t *testing.T) {
	// When the boosted CPU (3x 200m = 600m) exceeds the container's CPU limit
	// (500m), the boost should be capped at the limit to avoid API rejection
	// from requests > limits.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "limited-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "limited-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:        "main",
					Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"limited-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify resize was attempted and CPU was capped at the limit (500m).
	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpuReq := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			cpuLim := resource.MustParse("500m")
			assert.True(t, cpuReq.Cmp(cpuLim) <= 0,
				"boosted CPU request (%s) should not exceed limit (500m)", cpuReq.String())
			assert.True(t, cpuReq.Cmp(resource.MustParse("100m")) > 0,
				"CPU should be boosted above the original 100m")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for capped startup boost")
}

func TestApplyStartupBoosts_ExpiryKeepsAnnotationOnFailure(t *testing.T) {
	// When steady-state resize fails during boost expiry, the boost annotation
	// should be kept so the next reconciliation retries. Without this,
	// a transient failure leaves the pod permanently at boosted CPU.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	boostAt := now.Add(-3 * time.Minute) // boost applied 3min ago, duration 2min => expired
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boost-expire-pod",
			Namespace: "default",
			Annotations: map[string]string{
				annotationStartupBoostAt: boostAt.UTC().Format(time.RFC3339),
			},
			Labels:            map[string]string{labelTracked: "true"},
			CreationTimestamp: metav1.NewTime(boostAt.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("600m"), // boosted
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	// Make UpdateResize fail to simulate transient error.
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "resize" {
			return true, nil, fmt.Errorf("simulated resize failure")
		}
		return false, nil, nil
	})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

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
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "boost-expire",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")}},
			},
		},
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	podsByWorkload := map[string][]corev1.Pod{"boost-expire": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	// Verify the boost annotation is still present (not removed after failure).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "boost-expire-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations[annotationStartupBoostAt]
	assert.True(t, has, "boost annotation should be kept when resize fails so next reconcile retries")
}

func TestApplyStartupBoosts_ExpiryKeepsAnnotationOnQoSSkip(t *testing.T) {
	t.Parallel()
	// Guaranteed + request-only expiry fails PreservesQoS. A blocking skip
	// must keep startup-boost-at so the next reconcile can retry.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	boostAt := now.Add(-3 * time.Minute)
	only := attunev1alpha1.ControlledRequestsOnly
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boost-expire-qos-pod",
			Namespace: "default",
			Annotations: map[string]string{
				annotationStartupBoostAt: boostAt.UTC().Format(time.RFC3339),
			},
			Labels:            map[string]string{labelTracked: "true"},
			CreationTimestamp: metav1.NewTime(boostAt.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSGuaranteed},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				ControlledValues: &only,
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "2.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "boost-expire-qos",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("500m")}},
			},
		},
	}
	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	podsByWorkload := map[string][]corev1.Pod{"boost-expire-qos": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "boost-expire-qos-pod", Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	_, has := updated.Annotations[annotationStartupBoostAt]
	assert.True(t, has, "boost annotation should be kept when expiry is blocked by QoS skip")
}

func TestApplyStartupBoosts_QoSSkipDoesNotEmitResizeSkipped(t *testing.T) {
	// Guaranteed QoS with request-only boost target fails PreservesQoS.
	// shouldSkipResize must stay a predicate so boost does not inherit a
	// "Skipping resize ... QoS" event.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-app-qos",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSGuaranteed},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })
	recorder := events.NewFakeRecorder(10)
	r.Recorder = recorder

	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "my-app",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")},
		}},
	}}
	r.applyStartupBoosts(context.Background(), policy, map[string][]corev1.Pod{"my-app": {*pod}}, recs, resize.NewPodResizer(clientset, ctrl.Log.WithName("test")), nil)

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			t.Fatal("Guaranteed request-only boost must be skipped by PreservesQoS")
		}
	}
	select {
	case ev := <-recorder.Events:
		t.Fatalf("startup boost QoS skip must not emit resize events, got %s", ev)
	default:
	}
}

// --- Issue #440: startup boost expiry memory regression test ---

func TestApplyStartupBoosts_ExpiryPassesShouldSkipResizeWithMemory(t *testing.T) {
	// Regression test: startup boost expiry must populate memory values in
	// the skip-check target so that LimitRange/node allocatable checks
	// don't fail due to zero-valued memory fields.
	boostApplied := time.Now().Add(-10 * time.Minute) // expired
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-abc-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "api-server"},
			Annotations: map[string]string{
				annotationStartupBoostAt: boostApplied.UTC().Format(time.RFC3339),
				annotationPolicy:         "test-policy",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"), // boosted
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4000m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	// LimitRange requires minimum memory of 64Mi.
	limitRange := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lr", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			},
		},
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod, node, limitRange).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())

	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	reconciler.Clientset = clientset

	policy := newTestPolicy("test-policy", "default")
	dur := metav1.Duration{Duration: 5 * time.Minute}
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   dur,
	}

	// Steady-state recommendation: 500m CPU (half the boosted value).
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest: resource.MustParse("500m"),
					},
				},
			},
		},
	}

	resizer := resize.NewPodResizer(clientset, logr.Discard())
	checks := reconciler.buildResizePreChecks(context.Background(), policy)

	reconciler.applyStartupBoosts(context.Background(), policy, podMap("api-server", pod), recommendations, resizer, checks)

	// Verify that UpdateResize was called (boost expired, resize to steady-state).
	var resizeCalls int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			resizeCalls++
		}
	}
	assert.GreaterOrEqual(t, resizeCalls, 1,
		"boost expiry should call UpdateResize to reduce CPU to steady-state")

	// Verify the boost annotation was removed (expiry completed successfully).
	var updated corev1.Pod
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated)
	require.NoError(t, err)
	_, hasBoostAt := updated.Annotations[annotationStartupBoostAt]
	assert.False(t, hasBoostAt, "startup boost annotation should be removed after expiry")
}

func TestApplyStartupBoosts_CapsAtMaxAllowed(t *testing.T) {
	// When the boosted CPU (3x 200m = 600m) exceeds the policy's
	// maxAllowed (400m), the boost should be capped at maxAllowed
	// to respect admin-configured ceilings.
	scheme := testScheme()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	maxAllowed := resource.MustParse("400m")
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				MaxAllowed: &maxAllowed,
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "maxallowed-app-abc",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(pod)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.SetNowFunc(func() time.Time { return now })

	resizer := resize.NewPodResizer(clientset, ctrl.Log)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "maxallowed-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:        "main",
					Recommended: attunev1alpha1.ResourceValues{CPURequest: resource.MustParse("200m")},
				},
			},
		},
	}
	podsByWorkload := map[string][]corev1.Pod{"maxallowed-app": {*pod}}

	r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

	var foundResize bool
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
			updatedPod := a.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
			cpuReq := updatedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			assert.True(t, cpuReq.Cmp(maxAllowed) <= 0,
				"boosted CPU (%s) should not exceed maxAllowed (400m)", cpuReq.String())
			assert.True(t, cpuReq.Cmp(resource.MustParse("100m")) > 0,
				"CPU should be boosted above original 100m")
			break
		}
	}
	assert.True(t, foundResize, "expected a resize action for maxAllowed-capped startup boost")
}

func TestApplyStartupBoosts_DestCap(t *testing.T) {
	// Live reconcile must dest-cap like CREATE after rec dest is written:
	// RequestsOnly keeps leftover dest; RequestsAndLimits dest-caps rec dest
	// and raises dest so a dest==request leftover is not a silent no-op.
	only := attunev1alpha1.ControlledRequestsOnly
	both := attunev1alpha1.ControlledRequestsAndLimits

	tests := []struct {
		name     string
		cv       *string
		leftover string
		recReq   string
		recDest  string
		wantReq  string
		wantDest string
	}{
		{
			name:     "RequestsOnly leftover dest dest-caps request and does not raise dest",
			cv:       &only,
			leftover: "200m",
			recReq:   "500m",
			wantReq:  "200m",
			wantDest: "200m",
		},
		{
			name:     "RequestsAndLimits leftover dest dest-caps rec dest and raises dest",
			cv:       &both,
			leftover: "200m",
			recReq:   "500m",
			recDest:  "1",
			wantReq:  "1",
			wantDest: "1",
		},
		{
			name:     "RequestsAndLimits Guaranteed rec dest equals rec request raises dest with boost",
			cv:       &both,
			leftover: "500m",
			recReq:   "500m",
			recDest:  "500m",
			wantReq:  "1",
			wantDest: "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			leftover, err := resource.ParseQuantity(tt.leftover)
			require.NoError(t, err)
			recReq, err := resource.ParseQuantity(tt.recReq)
			require.NoError(t, err)
			wantReq, err := resource.ParseQuantity(tt.wantReq)
			require.NoError(t, err)
			wantDest, err := resource.ParseQuantity(tt.wantDest)
			require.NoError(t, err)
			memReq, err := resource.ParseQuantity("128Mi")
			require.NoError(t, err)

			policy := &attunev1alpha1.AttunePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
				Spec: attunev1alpha1.AttunePolicySpec{
					CPU: attunev1alpha1.ResourceConfig{
						ControlledValues: tt.cv,
						StartupBoost: &attunev1alpha1.StartupBoost{
							Multiplier: "2.0",
							Duration:   metav1.Duration{Duration: 2 * time.Minute},
						},
					},
				},
			}
			recVals := attunev1alpha1.ResourceValues{CPURequest: recReq}
			if tt.recDest != "" {
				recDest, destErr := resource.ParseQuantity(tt.recDest)
				require.NoError(t, destErr)
				recVals.CPULimit = recDest
			}

			// dest==request leftover is Guaranteed. PreservesQoS needs memory dest.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "dest-app-abc",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSGuaranteed},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "main",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    leftover.DeepCopy(),
									corev1.ResourceMemory: memReq,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    leftover.DeepCopy(),
									corev1.ResourceMemory: memReq.DeepCopy(),
								},
							},
						},
					},
				},
			}
			clientset := kubefake.NewSimpleClientset(pod)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
			r := NewAttunePolicyReconciler()
			r.Client = fakeClient
			r.Scheme = scheme
			r.Clientset = clientset
			r.SetNowFunc(func() time.Time { return now })

			resizer := resize.NewPodResizer(clientset, ctrl.Log)
			recs := []attunev1alpha1.WorkloadRecommendation{
				{
					Workload: "dest-app",
					Kind:     "Deployment",
					Containers: []attunev1alpha1.ContainerRecommendation{
						{Name: "main", Recommended: recVals},
					},
				},
			}
			podsByWorkload := map[string][]corev1.Pod{"dest-app": {*pod}}

			r.applyStartupBoosts(context.Background(), policy, podsByWorkload, recs, resizer, nil)

			got, getErr := clientset.CoreV1().Pods(pod.Namespace).Get(
				context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, getErr)
			gotReq := got.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			gotDest := got.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
			assert.Equal(t, wantReq.MilliValue(), gotReq.MilliValue(),
				"live startup boost CPU request dest-clamp")
			assert.Equal(t, wantDest.MilliValue(), gotDest.MilliValue(),
				"live startup boost CPU dest")
		})
	}
}
