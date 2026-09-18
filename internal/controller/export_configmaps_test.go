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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestExportRecommendationConfigMaps_CreatesConfigMap(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.95,
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("250m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
				},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-policy-my-app-recommendations",
	}, &cm)
	require.NoError(t, err)
	assert.Equal(t, RecommendationExportSchemaVersion, cm.Data["schema-version"])
	assert.Equal(t, "test-policy", cm.Data["policy"])
	assert.Equal(t, "default", cm.Data["namespace"])
	assert.Equal(t, "my-app", cm.Data["workload"])
	assert.Equal(t, "Deployment", cm.Data["kind"])
	assert.Equal(t, "250m", cm.Data["main.cpu-request"])
	assert.Equal(t, "256Mi", cm.Data["main.memory-request"])
	assert.Equal(t, "0.95", cm.Data["main.confidence"])
	assert.NotEmpty(t, cm.Data["last-updated"])
	assert.Equal(t, cm.Data["last-updated"], cm.Data["generated-at"])
	assert.Equal(t, "test-policy", cm.Labels["attune.io/policy"])
	assert.Equal(t, RecommendationExportSchemaVersion, cm.Labels["attune.io/export-schema"])
}

func TestExportRecommendationConfigMaps_UpdatesExisting(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-my-app-recommendations",
			Namespace: "default",
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.99,
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-policy-my-app-recommendations",
	}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "500m", cm.Data["main.cpu-request"])
	assert.Equal(t, "0.99", cm.Data["main.confidence"])
}

func TestExportRecommendationConfigMaps_CreateFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return fmt.Errorf("simulated create failure")
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.95, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("250m"), MemoryRequest: resource.MustParse("256Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "ConfigMap should not exist after create failure")
}

func TestExportRecommendationConfigMaps_GetFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("simulated API server error")
				}
				return nil
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.90, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("100m"), MemoryRequest: resource.MustParse("128Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)
}

func TestExportRecommendationConfigMaps_UpdateFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy-my-app-recommendations", Namespace: "default"},
		Data:       map[string]string{"old-key": "old-value"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("simulated patch failure")
				}
				return nil
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.85, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("300m"), MemoryRequest: resource.MustParse("384Mi"),
			}},
		}},
	}

	// Should not panic; the error is logged and the function continues.
	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// The existing ConfigMap should still have old data since the update failed.
	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "old-value", cm.Data["old-key"])
}

func TestExportRecommendationConfigMaps_PreservesExistingLabels(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default", UID: "abc-123"},
	}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-policy-my-app-recommendations", Namespace: "default",
			Labels: map[string]string{"custom-label": "keep-me"},
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{Workload: "my-app", Kind: "Deployment", Containers: []attunev1alpha1.ContainerRecommendation{
			{Name: "main", Confidence: 0.92, Recommended: attunev1alpha1.ResourceValues{
				CPURequest: resource.MustParse("400m"), MemoryRequest: resource.MustParse("512Mi"),
			}},
		}},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &cm)
	require.NoError(t, err)
	assert.Equal(t, "400m", cm.Data["main.cpu-request"], "data should be updated")
	assert.Equal(t, "keep-me", cm.Labels["custom-label"], "existing labels should be preserved")
	assert.Equal(t, "test-policy", cm.Labels["attune.io/policy"], "operator labels should be set")
}

func TestExportRecommendationConfigMaps_OrphanCleanup(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}

	// Pre-create ConfigMaps for two workloads under this policy
	activeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-my-app-recommendations",
			Namespace: "default",
			Labels: map[string]string{
				"attune.io/policy":   "test-policy",
				"attune.io/workload": "my-app",
			},
		},
		Data: map[string]string{"main.cpu-request": "100m"},
	}
	orphanCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-old-workload-recommendations",
			Namespace: "default",
			Labels: map[string]string{
				"attune.io/policy":   "test-policy",
				"attune.io/workload": "old-workload",
			},
		},
		Data: map[string]string{"main.cpu-request": "50m"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, activeCM, orphanCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Confidence: 0.9, Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("200m"),
					MemoryRequest: resource.MustParse("256Mi"),
				}},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// Active workload ConfigMap should still exist and be updated
	var active corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-my-app-recommendations"}, &active)
	require.NoError(t, err)
	assert.Equal(t, "200m", active.Data["main.cpu-request"])

	// Orphaned workload ConfigMap should have been deleted
	var orphan corev1.ConfigMap
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-policy-old-workload-recommendations"}, &orphan)
	assert.True(t, apierrors.IsNotFound(err), "orphaned ConfigMap should be deleted")
}

func TestExportRecommendationConfigMaps_SkipsLongName(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Build a workload name that makes the ConfigMap name exceed 253 chars.
	// Format: "test-policy-<workload>-recommendations" = 12 + len(workload) + 16 = 28 + len(workload)
	// Need 28 + len(workload) > 253, so len(workload) >= 226
	longWorkload := strings.Repeat("x", 226)
	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: longWorkload,
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{Name: "main", Confidence: 0.9, Recommended: attunev1alpha1.ResourceValues{
					CPURequest:    resource.MustParse("100m"),
					MemoryRequest: resource.MustParse("128Mi"),
				}},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	// ConfigMap should NOT have been created because the name exceeds 253 chars.
	cmName := fmt.Sprintf("test-policy-%s-recommendations", longWorkload)
	assert.Greater(t, len(cmName), 253)
	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      cmName,
	}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "ConfigMap with name >253 chars should not be created")
}

func TestExportRecommendationConfigMaps_SkipsStale(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	const t0 = "2026-01-01T00:00:00Z"
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy-my-app-recommendations",
			Namespace: "default",
			Labels: map[string]string{
				"attune.io/policy":   "test-policy",
				"attune.io/workload": "my-app",
			},
		},
		Data: map[string]string{
			"main.cpu-request": "100m",
			"last-updated":     t0,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, existingCM).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) })

	recs := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "my-app",
			Kind:     "Deployment",
			Stale:    true,
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name:       "main",
					Confidence: 0.95,
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	r.exportRecommendationConfigMaps(context.Background(), policy, recs)

	var cm corev1.ConfigMap
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-policy-my-app-recommendations",
	}, &cm)
	require.NoError(t, err, "stale rec must remain in currentWorkloads so the CM is not deleted")
	assert.Equal(t, "100m", cm.Data["main.cpu-request"], "stale rec must not overwrite existing cpu-request")
	assert.Equal(t, t0, cm.Data["last-updated"], "stale rec must not restamp last-updated")

	// No existing CM: a stale-only rec must not create one.
	staleOnlyPolicy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-only",
			Namespace: "default",
			UID:       "def-456",
		},
	}
	require.NoError(t, fakeClient.Create(context.Background(), staleOnlyPolicy))
	r.exportRecommendationConfigMaps(context.Background(), staleOnlyPolicy, recs)
	err = fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "stale-only-my-app-recommendations",
	}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "stale-only rec must not create a ConfigMap")
}

func TestExportRecommendationConfigMaps_NaNInfConfidenceWritesZero(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
			UID:       "abc-123",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme
	r.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	tests := []struct {
		name       string
		confidence float64
	}{
		{"NaN", math.NaN()},
		{"positive Inf", math.Inf(1)},
		{"negative Inf", math.Inf(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := []attunev1alpha1.WorkloadRecommendation{
				{
					Workload: "my-app",
					Kind:     "Deployment",
					Containers: []attunev1alpha1.ContainerRecommendation{
						{
							Name:       "main",
							Confidence: tt.confidence,
							Recommended: attunev1alpha1.ResourceValues{
								CPURequest:    resource.MustParse("250m"),
								MemoryRequest: resource.MustParse("256Mi"),
							},
						},
					},
				},
			}

			r.exportRecommendationConfigMaps(context.Background(), policy, recs)

			var cm corev1.ConfigMap
			err := fakeClient.Get(context.Background(), client.ObjectKey{
				Namespace: "default",
				Name:      "test-policy-my-app-recommendations",
			}, &cm)
			require.NoError(t, err)
			assert.Equal(t, "0.00", cm.Data["main.confidence"],
				"%s confidence should be written as 0.00", tt.name)

			// Clean up for next subtest.
			_ = fakeClient.Delete(context.Background(), &cm)
		})
	}
}
