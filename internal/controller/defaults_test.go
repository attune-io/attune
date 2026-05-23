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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
)

func TestMergeDefaults_NoDefaults(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
			},
		},
	}

	defaults, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	r.mergeDefaults(policy, defaults)

	// Nothing should change when no defaults exist.
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "30", policy.Spec.Memory.Overhead)
}

func TestMergeDefaults_CPUPercentileMerged(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 0, // zero: should be filled from defaults
				Overhead:   "50",
			},
		},
	}

	fetchedDefaults, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	r.mergeDefaults(policy, fetchedDefaults)

	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	// Overhead was already set on the policy, so it stays.
	assert.Equal(t, "50", policy.Spec.CPU.Overhead)
}

func TestMergeDefaults_OverheadMerged(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "20",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "", // empty: should be filled from defaults
			},
		},
	}

	fetchedDefaults, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	r.mergeDefaults(policy, fetchedDefaults)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
}

func TestMergeDefaults_PolicyTakesPrecedence(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "50",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "30",
			},
		},
	}

	fetchedDefaults, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	r.mergeDefaults(policy, fetchedDefaults)

	// Policy values take precedence over defaults.
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "30", policy.Spec.CPU.Overhead)
}

func TestFetchDefaults_NamespaceScopedOverridesCluster(t *testing.T) {
	clusterDefaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	nsDefaults := &rightsizev1alpha1.RightSizeNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "production-defaults", Namespace: "production"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99},
		},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterDefaults, nsDefaults).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	// Namespace with a RightSizeNamespaceDefaults should use it.
	result, err := r.fetchDefaults(context.Background(), "production")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, int32(99), result.Spec.CPU.Percentile)

	// Namespace without RightSizeNamespaceDefaults falls back to cluster.
	result, err = r.fetchDefaults(context.Background(), "staging")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, int32(90), result.Spec.CPU.Percentile)
}

func TestFetchDefaults_NamespaceDefaultsDoNotMergeWithClusterDefaults(t *testing.T) {
	clusterDefaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU:    &rightsizev1alpha1.ResourceConfig{Percentile: 90, Overhead: "20"},
			Memory: &rightsizev1alpha1.ResourceConfig{Percentile: 95, Overhead: "40"},
		},
	}
	nsDefaults := &rightsizev1alpha1.RightSizeNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "production-defaults", Namespace: "production"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99, Overhead: "20"},
			// Memory intentionally omitted: namespace defaults should replace,
			// not merge with, cluster defaults for this namespace.
		},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterDefaults, nsDefaults).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	defaults, err := r.fetchDefaults(context.Background(), "production")
	require.NoError(t, err)
	require.NotNil(t, defaults)
	assert.Equal(t, int32(99), defaults.Spec.CPU.Percentile)
	assert.Equal(t, "20", defaults.Spec.CPU.Overhead)
	assert.Nil(t, defaults.Spec.Memory)

	policy := &rightsizev1alpha1.RightSizePolicy{}
	r.mergeDefaults(policy, defaults)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	assert.Zero(t, policy.Spec.Memory.Percentile)
	assert.Empty(t, policy.Spec.Memory.Overhead)
}

func TestMergeDefaults_NamespaceDefaultsUseBuiltInFallbackForOmittedMemory(t *testing.T) {
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99, Overhead: "20"},
		},
	}
	r := &RightSizePolicyReconciler{}

	policy := &rightsizev1alpha1.RightSizePolicy{}
	r.mergeDefaults(policy, defaults)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	assert.Zero(t, policy.Spec.Memory.Percentile)
	assert.Empty(t, policy.Spec.Memory.Overhead)

	cpuEngine, memEngine := buildRecommendationEngines(policy)
	require.NotNil(t, cpuEngine)
	require.NotNil(t, memEngine)

	profile := metrics.UsageProfile{
		OverallPercentiles: metrics.PercentileSet{
			P50: 256 * 1024 * 1024,
			P90: 384 * 1024 * 1024,
			P95: 512 * 1024 * 1024,
			P99: 1024 * 1024 * 1024,
			Max: 1024 * 1024 * 1024,
		},
		Confidence: 1.0,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = profile.OverallPercentiles
	}

	recommended, explanation, changed := memEngine.RecommendWithExplanation(profile, resource.MustParse("512Mi"))
	assert.True(t, changed)
	assert.Equal(t, int64(1024*1024*1024), explanation.RawPercentile.Value(), "omitted memory should fall back to the built-in p99 percentile")
	assert.Equal(t, 30.0, explanation.Overhead, "omitted memory should fall back to the built-in overhead")
	assert.Equal(t, int64(1395864372), explanation.AfterOverhead.Value(), "built-in memory overhead should widen the raw percentile result")
	assert.Equal(t, recommended.String(), explanation.Final.String())
}

func TestFetchDefaults_ListError(t *testing.T) {
	scheme := testScheme()
	errClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()
	r := &RightSizePolicyReconciler{Client: errClient, Scheme: scheme}

	// Both namespace and cluster List calls fail; fetchDefaults should return an error.
	result, err := r.fetchDefaults(context.Background(), "default")
	assert.Nil(t, result, "fetchDefaults should not return defaults when List fails")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing RightSizeNamespaceDefaults")
}

func TestFetchDefaults_ClusterListError(t *testing.T) {
	// Namespace list succeeds (returns 0 items) but cluster list fails.
	scheme := testScheme()
	callCount := 0
	errClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, client client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				callCount++
				if callCount == 1 {
					// First call (namespace-scoped): succeed with empty list.
					return nil
				}
				// Second call (cluster-scoped): fail.
				return fmt.Errorf("simulated cluster list error")
			},
		}).Build()
	r := &RightSizePolicyReconciler{Client: errClient, Scheme: scheme}

	result, err := r.fetchDefaults(context.Background(), "default")
	assert.Nil(t, result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing RightSizeDefaults")
}

func TestFetchDefaults_SelectsLexicographicallySmallestClusterDefault(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			&rightsizev1alpha1.RightSizeDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "zeta-defaults"},
				Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
					CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99},
				},
			},
			&rightsizev1alpha1.RightSizeDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha-defaults"},
				Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
					CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 90},
				},
			},
		).
		Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	for i := 0; i < 10; i++ {
		result, err := r.fetchDefaults(context.Background(), "default")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "alpha-defaults", result.Name)
		assert.Equal(t, int32(90), result.Spec.CPU.Percentile)
	}
}

func TestFetchDefaults_SelectsLexicographicallySmallestNamespaceDefault(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			&rightsizev1alpha1.RightSizeDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
				Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
					CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 80},
				},
			},
			&rightsizev1alpha1.RightSizeNamespaceDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "zeta-defaults", Namespace: "production"},
				Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
					CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99},
				},
			},
			&rightsizev1alpha1.RightSizeNamespaceDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha-defaults", Namespace: "production"},
				Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
					CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 95},
				},
			},
		).
		Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	for i := 0; i < 10; i++ {
		result, err := r.fetchDefaults(context.Background(), "production")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "alpha-defaults", result.Name)
		assert.Equal(t, int32(95), result.Spec.CPU.Percentile)
	}
}

func TestFetchDefaults_DoesNotDependOnListOrder(t *testing.T) {
	scheme := testScheme()
	listOrder := []string{"gamma-defaults", "alpha-defaults", "beta-defaults"}
	sort.SliceStable(listOrder, func(i, j int) bool { return i > j })

	objects := make([]client.Object, 0, len(listOrder))
	for idx, name := range listOrder {
		objects = append(objects, &rightsizev1alpha1.RightSizeDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
				CPU: &rightsizev1alpha1.ResourceConfig{Percentile: int32(90 + idx)},
			},
		})
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	result, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "alpha-defaults", result.Name)
}
