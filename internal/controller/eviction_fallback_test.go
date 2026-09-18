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
	"sync"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/resize"
)

func TestTryEvictionFallback_EvictsWhenMultipleReplicas(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "success"))
	resizeBefore := promtestutil.ToFloat64(operatormetrics.ResizeTotal.WithLabelValues("default", "api-server", "eviction", "success"))

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.True(t, evicted, "should evict when multiple replicas exist")
	assert.Empty(t, reason, "successful eviction has no failure reason")

	// Verify eviction was called.
	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
	}
	assert.Equal(t, 1, evictions)
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "success")))
	assert.Equal(t, resizeBefore, promtestutil.ToFloat64(operatormetrics.ResizeTotal.WithLabelValues("default", "api-server", "eviction", "success")),
		"eviction fallback should not increment in-place resize metrics")
}

func TestTryEvictionFallback_ConcurrentTwoReplicasEvictsAtMostOne(t *testing.T) {
	// Two executeResizes goroutines can both List running==2 and both Evict
	// unless List+count+Evict is serialized per workload. Fake clientset
	// does not remove a pod on Evict, so the reactor deletes it so the
	// second List sees running==1.
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		meta, ok := create.GetObject().(metav1.Object)
		if !ok {
			return true, nil, fmt.Errorf("eviction object is not metav1.Object")
		}
		ns := meta.GetNamespace()
		if ns == "" {
			ns = action.GetNamespace()
		}
		if err := clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ns, meta.GetName()); err != nil {
			return true, nil, err
		}
		return true, create.GetObject(), nil
	})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	var (
		start, done     sync.WaitGroup
		mu              sync.Mutex
		evictedCount    int
		lastReplicaHits int
	)
	start.Add(2)
	done.Add(2)
	run := func(p *corev1.Pod) {
		defer done.Done()
		start.Done()
		start.Wait()
		evicted, reason := r.tryEvictionFallback(context.Background(), policy, p, deploy,
			"api-server", "app", resizer)
		mu.Lock()
		defer mu.Unlock()
		if evicted {
			evictedCount++
		}
		if reason == reasonEvictionLastReplica {
			lastReplicaHits++
		}
	}
	go run(pod1)
	go run(pod2)
	done.Wait()

	var evictions int
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			evictions++
		}
		if a.GetVerb() == "list" {
			if lo, ok := a.(interface{ GetListOptions() metav1.ListOptions }); ok {
				opts := lo.GetListOptions()
				assert.Equal(t, evictionReplicaPageSize, opts.Limit, "last-replica List must paginate")
				assert.Empty(t, opts.ResourceVersion, "last-replica List must not use ResourceVersion 0")
			}
		}
	}
	assert.LessOrEqual(t, evictions, 1, "at most one eviction create")
	assert.LessOrEqual(t, evictedCount, 1, "at most one successful eviction")
	assert.True(t, lastReplicaHits >= 1 || evictedCount == 1,
		"at least one call must return last_replica or the second must see running<=1 after the first evicts")
}

func TestTryEvictionFallback_SkipsLastReplica(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "last_replica"))

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should NOT evict the last replica")
	assert.Equal(t, reasonEvictionLastReplica, reason)
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "last_replica")))

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EvictionBlocked")
		assert.Contains(t, event, "1 live Running replica")
		assert.Contains(t, event, "spec.replicas")
		assert.Contains(t, event, "NotReady")
	default:
		t.Error("expected EvictionBlocked event but none was emitted")
	}
}

func TestTryEvictionFallback_EvictionDeniedByPDB(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod1, pod2)
	// Make eviction fail (simulates PDB denial).
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "eviction" {
			return true, nil, fmt.Errorf("Cannot evict pod as it would violate the pod's disruption budget")
		}
		return false, nil, nil
	})

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should return false when eviction is denied by PDB")
	assert.Equal(t, reasonEvictionDenied, reason)
}

func TestTryEvictionFallback_StaleCacheDoesNotEvictLastLiveReplica(t *testing.T) {
	pod1 := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-2", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	// Informer cache still has two Running pods; the live API has only one.
	clientset := kubefake.NewSimpleClientset(pod1)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod1, pod2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod1, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "must not evict when live Clientset has only one running replica")
	assert.Equal(t, reasonEvictionLastReplica, reason)

	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			t.Error("eviction should not be attempted when live replica count is 1")
		}
	}
}

func TestTryEvictionFallback_ListErrorSkipsEviction(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	clientset.PrependReactor("list", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "list_failed"))

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should skip eviction when pod list fails")
	assert.Equal(t, reasonEvictionListFailed, reason)
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "list_failed")))
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EvictionBlocked")
		assert.Contains(t, event, "cannot list live Running pods")
	default:
		t.Error("expected EvictionBlocked event on list failure")
	}

	// Verify no eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			t.Error("eviction should not be attempted when List fails")
		}
	}
}

func TestTryEvictionFallback_NilSelectorSkipsEviction(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	// Create a deployment with nil Selector to exercise the nil-guard.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: nil,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
				},
			},
		},
	}
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Clientset = clientset
	r.Recorder = recorder
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evictionBefore := promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "no_selector"))

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "main", resizer)
	assert.False(t, evicted, "should skip eviction when workload has nil selector")
	assert.Equal(t, reasonEvictionNoSelector, reason)
	assert.Equal(t, evictionBefore+1, promtestutil.ToFloat64(operatormetrics.EvictionTotal.WithLabelValues("default", "api-server", "no_selector")))
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EvictionBlocked")
		assert.Contains(t, event, "no pod selector")
	default:
		t.Error("expected EvictionBlocked event when selector is nil")
	}

	// Verify no eviction was attempted.
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" && a.GetSubresource() == "eviction" {
			t.Error("eviction should not be attempted when selector is nil")
		}
	}
}

func TestTryEvictionFallback_NilClientsetSkipsEviction(t *testing.T) {
	pod := newTestPod("api-server-abc-1", "default", map[string]string{"app": "api-server"})
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate

	clientset := kubefake.NewSimpleClientset(pod)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, deploy, pod).Build()
	recorder := events.NewFakeRecorder(10)
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.Recorder = recorder
	resizer := resize.NewPodResizer(clientset, ctrl.Log)

	evicted, reason := r.tryEvictionFallback(context.Background(), policy, pod, deploy,
		"api-server", "app", resizer)
	assert.False(t, evicted, "should skip eviction when Clientset is nil")
	assert.Equal(t, reasonEvictionListFailed, reason)
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EvictionBlocked")
		assert.Contains(t, event, "clientset unavailable")
	default:
		t.Error("expected EvictionBlocked event when Clientset is nil")
	}
}
