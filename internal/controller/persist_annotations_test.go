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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPersistResizeAnnotations_TimeoutAfterCommitTreatsAsSuccess(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	wrapped := &commitThenTimeoutPodClient{Client: fakeClient, cs: clientset, timeoutsLeft: 1}

	r := NewAttunePolicyReconciler()
	r.Client = wrapped
	r.Scheme = scheme
	r.Clientset = clientset

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(context.Background(), working, persistMainRec(t),
		"test-policy", "api-server", now, 3)
	require.NoError(t, err, "committed persist plus client timeout must be treated as success")
	assert.Empty(t, reason)
	assert.Equal(t, 1, wrapped.timeoutsSeen)

	var stored corev1.Pod
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}, &stored))
	assert.Equal(t, "main", stored.Annotations[annotationResizedContainers])
	assert.Equal(t, now.UTC().Format(time.RFC3339), stored.Annotations[annotationResizedAt])
	assert.Equal(t, "test-policy", stored.Annotations[annotationPolicy])
	assert.Equal(t, "true", stored.Labels[labelTracked])
	assert.Equal(t, now.UTC().Format(time.RFC3339), working.Annotations[annotationResizedAt],
		"in-memory pod must receive the confirmed annotations")
}

func TestPersistResizeAnnotations_ConfirmGetRetryThenSuccess(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	wrapped := &commitThenTimeoutPodClient{Client: fakeClient, cs: clientset, timeoutsLeft: 1}

	var confirmGets atomic.Int32
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		n := confirmGets.Add(1)
		// Get 1 is persist re-fetch; Get 2 is the first confirm.
		if n == 2 {
			return true, nil, apierrors.NewTimeoutError("injected confirm Get timeout", 0)
		}
		return false, nil, nil
	})

	r := NewAttunePolicyReconciler()
	r.Client = wrapped
	r.Scheme = scheme
	r.Clientset = clientset

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(context.Background(), working, persistMainRec(t),
		"test-policy", "api-server", now, 3)
	require.NoError(t, err, "confirm Get timeout then success must not revert")
	assert.Empty(t, reason)
	assert.GreaterOrEqual(t, confirmGets.Load(), int32(3), "re-fetch plus failed confirm plus retry")
	assert.Equal(t, now.UTC().Format(time.RFC3339), working.Annotations[annotationResizedAt])
}

func TestPersistResizeAnnotations_ConfirmUsesDetachedContext(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	inner := kubefake.NewSimpleClientset(pod.DeepCopy())
	clientset := &cancelAwareGetClientset{Interface: inner}
	wrapped := &commitThenTimeoutPodClient{Client: fakeClient, cs: inner, timeoutsLeft: 1}

	r := NewAttunePolicyReconciler()
	r.Client = wrapped
	r.Scheme = scheme
	r.Clientset = clientset

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(ctx, working, persistMainRec(t),
		"test-policy", "api-server", now, 3)
	require.NoError(t, err, "cancelled parent ctx must not skip confirm after a committed write")
	assert.Empty(t, reason)
	assert.Equal(t, now.UTC().Format(time.RFC3339), working.Annotations[annotationResizedAt])
	assert.GreaterOrEqual(t, clientset.gets.Load(), int32(2), "re-fetch plus at least one confirm Get")
}

func TestPersistResizeAnnotations_ConfirmGetAlwaysErrorsReverts(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	clientset := kubefake.NewSimpleClientset(pod.DeepCopy())
	wrapped := &commitThenTimeoutPodClient{Client: fakeClient, cs: clientset, timeoutsLeft: 1}

	var confirmGets atomic.Int32
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		// Allow the persist re-fetch; fail every confirm Get.
		obj, err := clientset.Tracker().Get(ga.GetResource(), ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		live, ok := obj.(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		if live.Annotations[annotationResizedAt] != "" {
			confirmGets.Add(1)
			return true, nil, apierrors.NewInternalError(fmt.Errorf("injected confirm Get 500"))
		}
		return false, nil, nil
	})

	r := NewAttunePolicyReconciler()
	r.Client = wrapped
	r.Scheme = scheme
	r.Clientset = clientset

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(context.Background(), working, persistMainRec(t),
		"test-policy", "api-server", now, 3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected timeout after committed annotation persist",
		"must return the write error, not the confirm Get error")
	assert.Equal(t, "annotation-persist-failed", reason)
	assert.Equal(t, int32(confirmTrackingAttempts), confirmGets.Load(),
		"confirm must retry before reverting")
}
