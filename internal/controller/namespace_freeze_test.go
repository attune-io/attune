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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
				assert.Contains(t, cond.Message, "new apply skipped")
			} else if cond != nil {
				assert.NotEqual(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
			}

			if tt.wantFrozenEvent {
				assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, eventReason)
				assert.Contains(t, eventNote, "new apply skipped")
			} else {
				assert.NotEqual(t, attunev1alpha1.ReasonNamespaceFrozen, eventReason)
			}
		})
	}
}

func TestReconcile_NamespaceFreeze_SkipsOnRecommendationPersist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		freeze     bool
		wantPatch  bool
		wantFrozen bool
	}{
		{
			name:       "freeze=true skips OnRecommendation persist",
			freeze:     true,
			wantPatch:  false,
			wantFrozen: true,
		},
		{
			name:      "freeze absent persists OnRecommendation",
			freeze:    false,
			wantPatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy := newTestPolicy("test-policy", "default")
			policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
			policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
				Enabled: boolPtr(true),
				When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
			}

			deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
			pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
			nsAnns := map[string]string{}
			if tt.freeze {
				nsAnns[conflict.AnnotationFreeze] = "true"
			}
			ns := newTestNamespace("default", nsAnns)

			mc := &mockCollector{
				queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
					return generateSamples(200, 0.1), nil
				},
			}

			scheme := testScheme()
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(policy, deploy, pod, ns).
				WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
				Build()
			reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
			})
			require.NoError(t, err)

			var updated attunev1alpha1.AttunePolicy
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
				Name: "test-policy", Namespace: "default",
			}, &updated))
			require.NotEmpty(t, updated.Status.Recommendations)

			var gotDeploy appsv1.Deployment
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
				Name: "api-server", Namespace: "default",
			}, &gotDeploy))
			cpuMilli := gotDeploy.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
			if tt.wantPatch {
				assert.NotEqual(t, int64(500), cpuMilli, "expected OnRecommendation persist to change template CPU")
			} else {
				assert.Equal(t, int64(500), cpuMilli, "frozen namespace must not persist template")
			}

			cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
			if tt.wantFrozen {
				require.NotNil(t, cond)
				assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
			} else if cond != nil {
				assert.NotEqual(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
			}
		})
	}
}

func TestReconcile_NamespaceFreeze_StillRevertsPendingSafety(t *testing.T) {
	t.Parallel()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	res := &deploy.Spec.Template.Spec.Containers[0].Resources
	res.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	res.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	res.Limits[corev1.ResourceCPU] = resource.MustParse("500m")
	res.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")

	resizedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	pod := newResizePod("api-server", "250m", "256Mi", "500m", "512Mi")
	pod.Labels[labelTracked] = "true"
	pod.Annotations = map[string]string{
		annotationResizedAt:                          resizedAt,
		annotationResizedWorkload:                    "api-server",
		annotationResizedContainers:                  "main",
		annotationOriginalCPUPrefix + "main":         "500m",
		annotationOriginalMemoryPrefix + "main":      "512Mi",
		annotationOriginalCPULimitPrefix + "main":    "1000m",
		annotationOriginalMemoryLimitPrefix + "main": "1Gi",
		annotationPolicy:                             "test-policy",
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "main",
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason:     "OOMKilled",
					FinishedAt: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	ns := newTestNamespace("default", map[string]string{conflict.AnnotationFreeze: "true"})
	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))

	assert.Greater(t, updated.Status.Workloads.WithRecommendations, int32(0),
		"freeze is apply-only; recommendations must still compute")
	require.NotEmpty(t, updated.Status.Recommendations)

	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)

	var foundResize bool
	for _, a := range reconciler.Clientset.(*kubefake.Clientset).Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			foundResize = true
		}
	}
	assert.True(t, foundResize, "frozen namespace must still revert pending safety observations")

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	gotRes := gotDeploy.Spec.Template.Spec.Containers[0].Resources
	assert.Equal(t, int64(500), gotRes.Requests.Cpu().MilliValue(),
		"safety revert must restore template CPU to original 500m")
	assert.True(t, gotRes.Requests.Memory().Equal(resource.MustParse("512Mi")),
		"safety revert must restore template memory to original 512Mi")

	assert.Equal(t, int32(0), updated.Status.Workloads.Resized, "freeze must skip new apply")
}

func TestReconcile_NamespaceFreeze_SkipsAfterSuccessfulResizePersist(t *testing.T) {
	t.Parallel()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	policy.Status.ResizeHistory = []attunev1alpha1.ResizeHistoryEntry{{
		Timestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
		Workload:  "api-server",
		Container: "main",
		Method:    "InPlace",
		Result:    attunev1alpha1.ResizeResultSuccess,
	}}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	ns := newTestNamespace("default", map[string]string{conflict.AnnotationFreeze: "true"})

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))
	require.NotEmpty(t, updated.Status.Recommendations)

	var gotDeploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "api-server", Namespace: "default",
	}, &gotDeploy))
	assert.Equal(t, int64(500), gotDeploy.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(),
		"frozen namespace must not persist AfterSuccessfulResize template")

	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
}

func TestReconcile_NamespaceFreeze_RecheckBeforeExecuteResizes(t *testing.T) {
	t.Parallel()

	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(100)

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	pod := newResizePod("api-server", "500m", "512Mi", "1000m", "1Gi")
	ns := newTestNamespace("default", nil)

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]rsmetrics.Sample, error) {
			return generateSamples(200, 0.1), nil
		},
	}

	scheme := testScheme()
	nsGets := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, deploy, pod, ns).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if nsObj, ok := obj.(*corev1.Namespace); ok {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					nsGets++
					if nsGets > 1 {
						if nsObj.Annotations == nil {
							nsObj.Annotations = map[string]string{}
						}
						nsObj.Annotations[conflict.AnnotationFreeze] = "true"
					}
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reconciler := newReconcilerForReconcileWithClient(mc, fakeClient, scheme)
	reconciler.Clientset = kubefake.NewSimpleClientset(pod.DeepCopy())

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated attunev1alpha1.AttunePolicy
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "test-policy", Namespace: "default",
	}, &updated))

	assert.Greater(t, updated.Status.Workloads.WithRecommendations, int32(0))
	assert.Equal(t, int32(0), updated.Status.Workloads.Resized, "second freeze check must skip executeResizes")
	assert.GreaterOrEqual(t, nsGets, 2, "expected a second namespace Get before apply")

	cond := meta.FindStatusCondition(updated.Status.Conditions, attunev1alpha1.ConditionResizeBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, attunev1alpha1.ReasonNamespaceFrozen, cond.Reason)
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
