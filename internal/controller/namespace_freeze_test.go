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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/conflict"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func newTestNamespace(name string, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}

func TestNamespaceApplyFrozen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ns         *corev1.Namespace
		getErr     error
		wantFrozen bool
		wantErr    bool
	}{
		{
			name:       "freeze=true skips apply",
			ns:         newTestNamespace("default", map[string]string{conflict.AnnotationFreeze: "true"}),
			wantFrozen: true,
		},
		{
			name:       "freeze=false does not skip apply",
			ns:         newTestNamespace("default", map[string]string{conflict.AnnotationFreeze: "false"}),
			wantFrozen: false,
		},
		{
			name:       "freeze absent does not skip apply",
			ns:         newTestNamespace("default", nil),
			wantFrozen: false,
		},
		{
			name:       "True is not freeze (same parser as skip)",
			ns:         newTestNamespace("default", map[string]string{conflict.AnnotationFreeze: "True"}),
			wantFrozen: false,
		},
		{
			name:       "Get error fail-closed",
			ns:         newTestNamespace("default", nil),
			getErr:     fmt.Errorf("simulated namespace get failure"),
			wantFrozen: true,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := testScheme()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.ns != nil && tt.getErr == nil {
				builder = builder.WithObjects(tt.ns)
			}
			if tt.getErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Namespace); ok {
							return tt.getErr
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			r := NewAttunePolicyReconciler()
			r.Client = builder.Build()
			r.Scheme = scheme

			frozen, err := r.namespaceApplyFrozen(context.Background(), "default")
			assert.Equal(t, tt.wantFrozen, frozen)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestReconcile_NamespaceFreeze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		freezeValue     string // empty means omit the annotation
		skipWorkload    bool
		getErr          bool
		wantRecs        bool
		wantResized     bool
		wantFrozenCond  bool
		wantFrozenEvent bool
	}{
		{
			name:            "freeze=true skips resize and still computes recommendations",
			freezeValue:     "true",
			wantRecs:        true,
			wantResized:     false,
			wantFrozenCond:  true,
			wantFrozenEvent: true,
		},
		{
			name:        "freeze=false does not skip resize",
			freezeValue: "false",
			wantRecs:    true,
			wantResized: true,
		},
		{
			name:        "freeze absent does not skip resize",
			wantRecs:    true,
			wantResized: true,
		},
		{
			name:            "Get error fail-closed skips resize",
			getErr:          true,
			wantRecs:        true,
			wantResized:     false,
			wantFrozenCond:  true,
			wantFrozenEvent: true,
		},
		{
			name:            "workload skip still works independently of freeze",
			freezeValue:     "true",
			skipWorkload:    true,
			wantRecs:        false,
			wantResized:     false,
			wantFrozenCond:  true,
			wantFrozenEvent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
			policy.Spec.CPU.MaxChangePercent = int32Ptr(100)

			deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
			if tt.skipWorkload {
				deploy.Annotations = map[string]string{conflict.AnnotationSkip: "true"}
			}
			pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")

			nsAnns := map[string]string{}
			if tt.freezeValue != "" {
				nsAnns[conflict.AnnotationFreeze] = tt.freezeValue
			}
			ns := newTestNamespace("default", nsAnns)

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}

			scheme := testScheme()
			builder := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(policy, deploy, pod, ns).
				WithStatusSubresource(&attunev1alpha1.AttunePolicy{})
			if tt.getErr {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Namespace); ok {
							return fmt.Errorf("simulated namespace get failure")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			fakeClient := builder.Build()
			reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
			reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())

			var eventReason, eventNote string
			reconciler.Recorder = &capturingEventRecorder{reason: &eventReason, note: &eventNote}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
			})
			require.NoError(t, err)

			var updated attunev1alpha1.AttunePolicy
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
				Name: "test-policy", Namespace: "default",
			}, &updated))

			assert.Equal(t, int32(1), updated.Status.Workloads.Discovered)
			if tt.wantRecs {
				assert.Greater(t, updated.Status.Workloads.WithRecommendations, int32(0))
				require.NotEmpty(t, updated.Status.Recommendations)
			} else {
				assert.Equal(t, int32(0), updated.Status.Workloads.WithRecommendations)
				assert.Empty(t, updated.Status.Recommendations)
			}

			if tt.wantResized {
				assert.Greater(t, updated.Status.Workloads.Resized, int32(0), "expected a live resize")
			} else {
				assert.Equal(t, int32(0), updated.Status.Workloads.Resized)
			}

			cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
			if tt.wantFrozenCond {
				require.NotNil(t, cond)
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
				assert.Contains(t, cond.Message, "resizes skipped")
			} else if cond != nil {
				assert.NotEqual(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
			}

			if tt.wantFrozenEvent {
				assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, eventReason)
				assert.Contains(t, eventNote, "resizes skipped")
			} else {
				assert.NotEqual(t, attunev1alpha1.ReasonNamespaceFrozen, eventReason)
			}
		})
	}
}

// capturingEventRecorder records the last Eventf reason and formatted note.
type capturingEventRecorder struct {
	reason *string
	note   *string
}

func (c *capturingEventRecorder) Eventf(_, _ runtime.Object, _, reason, _, note string, args ...interface{}) {
	if reason != attunev1alpha1.ReasonNamespaceFrozen {
		return
	}
	if c.reason != nil {
		*c.reason = reason
	}
	if c.note != nil {
		*c.note = fmt.Sprintf(note, args...)
	}
}
