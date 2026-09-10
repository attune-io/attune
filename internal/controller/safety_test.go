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
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/safety"
)

// ---------- findContainerStatusByName ----------

func TestFindContainerStatusByName(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: 0},
				{Name: "sidecar", Ready: true, RestartCount: 1},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init-db", Ready: false, RestartCount: 0},
			},
		},
	}

	tests := []struct {
		name      string
		container string
		wantName  string
		wantNil   bool
	}{
		{
			name:      "finds regular container",
			container: "main",
			wantName:  "main",
		},
		{
			name:      "finds second regular container",
			container: "sidecar",
			wantName:  "sidecar",
		},
		{
			name:      "finds init container",
			container: "init-db",
			wantName:  "init-db",
		},
		{
			name:      "returns nil for missing container",
			container: "nonexistent",
			wantNil:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findContainerStatusByName(pod, tt.container)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantName, got.Name)
			}
		})
	}
}

func TestFindContainerStatusByName_EmptyPod(t *testing.T) {
	pod := &corev1.Pod{}
	got := findContainerStatusByName(pod, "any")
	assert.Nil(t, got)
}

// ---------- runImmediateSafetyCheck ----------

func TestRunImmediateSafetyCheck_AutoRevertDisabled(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				// AutoRevert defaults to nil, but set explicitly to false.
				AutoRevert: boolPtr(false),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(
		context.Background(),
		policy,
		safety.ResizeRecord{},
	)
	assert.NoError(t, err)
	assert.Empty(t, reason)
}

func TestRunImmediateSafetyCheck_CheckPodError(t *testing.T) {
	// Create a fake clientset that returns an error for pod Get.
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())

	record := safety.ResizeRecord{
		PodName:   "test-pod",
		Namespace: "default",
		Container: "main",
		ResizedAt: time.Now(),
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, record)
	assert.Error(t, err, "should propagate Get error")
	assert.Empty(t, reason, "no revert reason when the check itself fails")
}

func TestRunImmediateSafetyCheck_UnsafePod(t *testing.T) {
	// Create a pod that has restarted 5 times (safety violation).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					RestartCount: 5, // increased by 5 since resize (record has 0)
				},
			},
		},
	}

	cs := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())

	record := safety.ResizeRecord{
		PodName:      "test-pod",
		Namespace:    "default",
		Container:    "main",
		ResizedAt:    time.Now().Add(-10 * time.Minute),
		RestartCount: 0, // original restart count before resize
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, record)
	assert.NoError(t, err)
	assert.NotEmpty(t, reason, "should return a revert reason for unsafe pod")
	assert.Contains(t, reason, "restart", "reason should mention restart increase")
}

func TestRunImmediateSafetyCheck_SafePod(t *testing.T) {
	// Create a healthy pod that passes all safety checks.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "healthy-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					Ready:        true,
					RestartCount: 0,
				},
			},
		},
	}

	cs := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())

	record := safety.ResizeRecord{
		PodName:      "healthy-pod",
		Namespace:    "default",
		Container:    "main",
		ResizedAt:    time.Now().Add(-10 * time.Minute),
		RestartCount: 0,
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, record)
	assert.NoError(t, err)
	assert.Empty(t, reason, "healthy pod should not trigger revert")
}

func TestRunImmediateSafetyCheck_NotReadyDoesNotRevert(t *testing.T) {
	// Ready=False with no OOM and no extra restarts is transient during
	// adjustment. Immediate check must not revert; full CheckPod would.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notready-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					Ready:        false,
					RestartCount: 0,
				},
			},
		},
	}

	cs := kubefake.NewSimpleClientset(pod)
	r := NewAttunePolicyReconciler()
	r.Clientset = cs

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				AutoRevert: boolPtr(true),
			},
		},
	}

	ctx := log.IntoContext(context.Background(), logr.Discard())

	record := safety.ResizeRecord{
		PodName:      "notready-pod",
		Namespace:    "default",
		Container:    "main",
		ResizedAt:    time.Now(),
		RestartCount: 0,
		OriginalResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}

	reason, err := r.runImmediateSafetyCheck(ctx, policy, record)
	assert.NoError(t, err)
	assert.Empty(t, reason, "transient not-ready must not trigger immediate revert")
	assert.NotEqual(t, "notready", reason, "immediate check must not use full CheckPod")
}

func TestRevertAndRestoreAfterSafety_RecordsRevertWhenRestoreFails(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := persistAtRec64MiDeployment()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("admission webhook denied template restore")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{{
		Timestamp: metav1.NewTime(time.Now()),
		Workload:  "api",
		Container: "app",
		Resource:  "memory",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-abc", Namespace: "default"}}
	reverted := false
	before := promtestutil.ToFloat64(operatormetrics.RevertsTotal.WithLabelValues("default", "api", "oomkill"))

	err := r.revertAndRestoreAfterSafety(
		log.IntoContext(context.Background(), logr.Discard()),
		func(safety.ResizeRecord) error {
			reverted = true
			return nil
		},
		policy, []client.Object{deploy}, original256MiRecord(), pod,
		"api", "oomkill", "OOMKilled",
		"Failed to revert pod during safety observation",
		"Safety observation reverted resize on pod %s/%s: %s",
	)
	require.Error(t, err, "restore patch failure must still return an error")
	assert.True(t, reverted, "live revert must run before restore")
	assert.Equal(t, attunev1alpha1.ResizeResultReverted, policy.Status.ResizeHistory[0].Result,
		"history must mark Reverted even when template restore fails")
	assert.Equal(t, before+1, promtestutil.ToFloat64(operatormetrics.RevertsTotal.WithLabelValues("default", "api", "oomkill")),
		"RevertsTotal must increment after a successful live revert")
}

func TestRetryTemplateRestoreIfAlreadyReverted_UsesCallerPod(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := persistAtRec64MiDeployment()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("live Get must not run inside retry")
	})
	r.Clientset = cs

	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	record := original256MiRecord()
	live := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: record.OriginalResources,
			}},
		},
	}

	err := r.retryTemplateRestoreIfAlreadyReverted(context.Background(), policy, []client.Object{deploy}, live, record)
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deploy), &got))
	assert.True(t, got.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"restore must use the caller-supplied live pod, not a Clientset Get")
}

func TestAcquireReleaseEvictionLock_DeletesWhenIdle(t *testing.T) {
	r := NewAttunePolicyReconciler()
	key := "default/api"
	mu := r.acquireEvictionLock(key)
	_, ok := r.evictionLocks.Load(key)
	assert.True(t, ok, "held lock must be in the map")
	assert.False(t, mu.TryLock(), "acquired lock must be held")
	r.releaseEvictionLock(key, mu)
	_, ok = r.evictionLocks.Load(key)
	assert.False(t, ok, "idle eviction lock must be removed")
}

func TestAcquireEvictionLock_SerializesSameKey(t *testing.T) {
	r := NewAttunePolicyReconciler()
	key := "default/api"
	mu := r.acquireEvictionLock(key)
	done := make(chan struct{})
	go func() {
		mu2 := r.acquireEvictionLock(key)
		close(done)
		r.releaseEvictionLock(key, mu2)
	}()
	select {
	case <-done:
		t.Fatal("second acquire must wait while the first holder is active")
	case <-time.After(30 * time.Millisecond):
	}
	r.releaseEvictionLock(key, mu)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second acquire must proceed after release")
	}
	_, ok := r.evictionLocks.Load(key)
	assert.False(t, ok, "map must be empty after both holders release")
}
