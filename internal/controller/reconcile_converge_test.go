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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// writeCounter counts client writes (Update, Patch, status subresource).
type writeCounter struct {
	n atomic.Int32
}

func (w *writeCounter) take() int32 {
	return w.n.Swap(0)
}

func (w *writeCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			w.n.Add(1)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			w.n.Add(1)
			return c.Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			w.n.Add(1)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			w.n.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}

func TestReconcile_NoWorkloads_SecondPassNoWrites(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Finalizers = []string{finalizerName}
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	writes := &writeCounter{}
	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ensureTestNamespaces([]client.Object{policy})...).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(writes.funcs()).
		Build()
	r := newReconcilerForReconcileWithClient(&mockCollector{}, c, scheme)
	r.SetNowFunc(func() time.Time { return now })
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"}}

	first, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.NotZero(t, first.RequeueAfter)
	firstWrites := writes.take()
	require.Greater(t, firstWrites, int32(0), "first reconcile must persist Ready=False")

	second, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(0), writes.take(),
		"second reconcile with a frozen clock must not rewrite status")
}

func TestReconcile_Paused_SecondPassNoWrites(t *testing.T) {
	paused := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-policy",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			Paused: &paused,
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: func() *string { s := "my-app"; return &s }(),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	writes := &writeCounter{}
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		WithInterceptorFuncs(writes.funcs()).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return now })
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "paused-policy", Namespace: "default"}}

	first, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, first)
	require.Greater(t, writes.take(), int32(0))

	second, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(0), writes.take(),
		"second paused reconcile must not rewrite Ready=False")
}

func TestReconcile_Paused_UnpauseWritesAgain(t *testing.T) {
	paused := true
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-policy",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			Paused: &paused,
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: func() *string { s := "my-app"; return &s }(),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}
	writes := &writeCounter{}
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		WithInterceptorFuncs(writes.funcs()).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "paused-policy", Namespace: "default"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	writes.take()

	var stored attunev1alpha1.AttunePolicy
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &stored))
	unpaused := false
	stored.Spec.Paused = &unpaused
	require.NoError(t, c.Update(context.Background(), &stored))
	writes.take()

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Greater(t, writes.take(), int32(0),
		"leaving Paused must write a new Ready reason")
}
