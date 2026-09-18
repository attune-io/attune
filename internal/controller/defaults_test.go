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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/metrics"
)

func TestMergeDefaults_NoDefaults(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
			},
			Memory: attunev1alpha1.ResourceConfig{
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
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
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
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "20",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
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
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "50",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
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
	clusterDefaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	nsDefaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "production-defaults", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 99},
		},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterDefaults, nsDefaults).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	// Namespace with an AttuneNamespaceDefaults should use it.
	result, err := r.fetchDefaults(context.Background(), "production")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, int32(99), result.Spec.CPU.Percentile)

	// Namespace without AttuneNamespaceDefaults falls back to cluster.
	result, err = r.fetchDefaults(context.Background(), "staging")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, int32(90), result.Spec.CPU.Percentile)
}

func TestFetchDefaults_NamespaceMergesWithClusterDefaults(t *testing.T) {
	// Issue #394: 3-tier merge — namespace wins for set fields; cluster fills gaps.
	clusterDefaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}
	nsDefaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "production-defaults", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			// Override percentile only; cooldown should come from cluster.
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterDefaults, nsDefaults).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	defaults, err := r.fetchDefaults(context.Background(), "production")
	require.NoError(t, err)
	require.NotNil(t, defaults)
	require.NotNil(t, defaults.Spec.CPU)
	assert.Equal(t, int32(90), defaults.Spec.CPU.Percentile, "namespace percentile wins")
	assert.Equal(t, "20", defaults.Spec.CPU.Overhead, "cluster overhead fills namespace gap")
	require.NotNil(t, defaults.Spec.UpdateStrategy)
	require.NotNil(t, defaults.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 10*time.Minute, defaults.Spec.UpdateStrategy.Cooldown.Duration,
		"cluster cooldown fills namespace gap")

	policy := &attunev1alpha1.AttunePolicy{}
	r.mergeDefaults(policy, defaults)
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	require.NotNil(t, policy.Spec.UpdateStrategy)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 10*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
}

func TestMergeDefaults_NamespaceDefaultsUseBuiltInFallbackForOmittedMemory(t *testing.T) {
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "20"},
		},
	}
	r := NewAttunePolicyReconciler()

	policy := &attunev1alpha1.AttunePolicy{}
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
	r := NewAttunePolicyReconciler()
	r.Client = errClient
	r.Scheme = scheme

	// Both namespace and cluster List calls fail; fetchDefaults should return an error.
	result, err := r.fetchDefaults(context.Background(), "default")
	assert.Nil(t, result, "fetchDefaults should not return defaults when List fails")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing AttuneNamespaceDefaults")
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
	r := NewAttunePolicyReconciler()
	r.Client = errClient
	r.Scheme = scheme

	result, err := r.fetchDefaults(context.Background(), "default")
	assert.Nil(t, result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing AttuneDefaults")
}

func TestFetchDefaults_SelectsLexicographicallySmallestClusterDefault(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			&attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "zeta-defaults"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{Percentile: 99},
				},
			},
			&attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha-defaults"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
				},
			},
		).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

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
			&attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
				},
			},
			&attunev1alpha1.AttuneNamespaceDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "zeta-defaults", Namespace: "production"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{Percentile: 99},
				},
			},
			&attunev1alpha1.AttuneNamespaceDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha-defaults", Namespace: "production"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{Percentile: 95},
				},
			},
		).
		Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

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
		objects = append(objects, &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU: &attunev1alpha1.ResourceConfig{Percentile: int32(90 + idx)},
			},
		})
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	result, err := r.fetchDefaults(context.Background(), "default")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "alpha-defaults", result.Name)
}

// ---------- mergeDefaults (more paths) ----------

func TestMergeDefaults_MergesAllFields(t *testing.T) {
	queryStep := metav1.Duration{Duration: 30 * time.Second}
	historyWindow := metav1.Duration{Duration: 48 * time.Hour}
	cooldown := metav1.Duration{Duration: 30 * time.Minute}
	autoRevert := true
	controlledValues := "RequestsAndLimits"
	burstSensitivity := "0.2"
	allowDecrease := true
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile:       90,
				Overhead:         "50",
				ControlledValues: &controlledValues,
				BurstSensitivity: &burstSensitivity,
				MinAllowed:       quantityPtr("100m"),
				MaxAllowed:       quantityPtr("8"),
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "3.0",
					Duration:   metav1.Duration{Duration: 2 * time.Minute},
				},
				MaxChangePercent: int32Ptr(80),
			},
			Memory: &attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "40",
				AllowDecrease:    &allowDecrease,
				MaxChangePercent: int32Ptr(60),
			},
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep:         &queryStep,
				HistoryWindow:     &historyWindow,
				MinimumDataPoints: int32Ptr(24),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                   attunev1alpha1.UpdateTypeAuto,
				Cooldown:               &cooldown,
				AutoRevert:             &autoRevert,
				ResizeMethod:           attunev1alpha1.ResizeMethodInPlaceOrRecreate,
				MaxConcurrentResizes:   5,
				MaxTotalCPUIncrease:    quantityPtr("2000m"),
				MaxTotalMemoryIncrease: quantityPtr("4Gi"),
				Schedule: &attunev1alpha1.ResizeSchedule{
					DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
					Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
				},
				Canary: &attunev1alpha1.CanaryConfig{
					Percentage:        10,
					AutoPromote:       true,
					ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
				},
				SafetyObservationPeriod: &metav1.Duration{Duration: 3 * time.Minute},
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with all zeros/empty (should inherit from defaults).
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	r.mergeDefaults(policy, defaults)

	// CPU
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "50", policy.Spec.CPU.Overhead)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, "RequestsAndLimits", *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.CPU.BurstSensitivity)
	assert.Equal(t, "0.2", *policy.Spec.CPU.BurstSensitivity)
	require.NotNil(t, policy.Spec.CPU.MinAllowed)
	assert.Equal(t, resource.MustParse("100m"), *policy.Spec.CPU.MinAllowed)
	require.NotNil(t, policy.Spec.CPU.StartupBoost)
	assert.Equal(t, "3.0", policy.Spec.CPU.StartupBoost.Multiplier)
	assert.Equal(t, 2*time.Minute, policy.Spec.CPU.StartupBoost.Duration.Duration)

	// Memory
	assert.Equal(t, int32(95), policy.Spec.Memory.Percentile)
	assert.Equal(t, "40", policy.Spec.Memory.Overhead)
	require.NotNil(t, policy.Spec.Memory.AllowDecrease)
	assert.True(t, *policy.Spec.Memory.AllowDecrease)

	// MetricsSource
	require.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, 30*time.Second, policy.Spec.MetricsSource.QueryStep.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Equal(t, 48*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, int32(24), *policy.Spec.MetricsSource.MinimumDataPoints)

	// UpdateStrategy
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, int32(5), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxTotalCPUIncrease)
	assert.Equal(t, resource.MustParse("2000m"), *policy.Spec.UpdateStrategy.MaxTotalCPUIncrease)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease)
	assert.Equal(t, resource.MustParse("4Gi"), *policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease)
	require.NotNil(t, policy.Spec.UpdateStrategy.Schedule)
	assert.Equal(t, []string{"Monday", "Wednesday", "Friday"}, policy.Spec.UpdateStrategy.Schedule.DaysOfWeek)
	require.NotNil(t, policy.Spec.UpdateStrategy.Canary)
	assert.Equal(t, int32(10), policy.Spec.UpdateStrategy.Canary.Percentage)
	assert.True(t, policy.Spec.UpdateStrategy.Canary.AutoPromote)
	assert.Equal(t, 5*time.Minute, policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.SafetyObservationPeriod)
	assert.Equal(t, 3*time.Minute, policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration)
}

func TestApplyBuiltInDefaults_FillsAllFields(t *testing.T) {
	r := newReconcilerWithClient()
	// Create a policy with ALL fields unset (no webhook defaults, no cluster defaults).
	policy := &attunev1alpha1.AttunePolicy{}

	r.applyBuiltInDefaults(policy)

	// Every field should now have a built-in default value.
	assert.Equal(t, attunev1alpha1.DefaultUpdateType, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultCPUMaxChangePercent, *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultMemoryMaxChangePercent, *policy.Spec.Memory.MaxChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, time.Hour, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, attunev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
	require.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Equal(t, 168*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, 5*time.Minute, policy.Spec.MetricsSource.QueryStep.Duration)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.True(t, *policy.Spec.ExcludeKnownSidecars)
	assert.Equal(t, attunev1alpha1.DefaultMaxConcurrentResizes, policy.Spec.UpdateStrategy.MaxConcurrentResizes)
}

func TestApplyBuiltInDefaults_PreservesUserValues(t *testing.T) {
	r := newReconcilerWithClient()
	// Create a policy with explicit user values.
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.CPU.MaxChangePercent = int32Ptr(80)
	policy.Spec.Memory.MaxChangePercent = int32Ptr(60)
	autoRevert := false
	policy.Spec.UpdateStrategy.AutoRevert = &autoRevert
	policy.Spec.UpdateStrategy.ResizeMethod = attunev1alpha1.ResizeMethodInPlaceOrRecreate
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(24)
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 30 * time.Minute}
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 48 * time.Hour}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 30 * time.Second}
	cv := attunev1alpha1.ControlledRequestsAndLimits
	policy.Spec.CPU.ControlledValues = &cv
	policy.Spec.Memory.ControlledValues = &cv
	excludeKnown := false
	policy.Spec.ExcludeKnownSidecars = &excludeKnown

	r.applyBuiltInDefaults(policy)

	// User values should be preserved, not overwritten.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.Equal(t, int32(24), *policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	assert.Equal(t, 48*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	assert.Equal(t, 30*time.Second, policy.Spec.MetricsSource.QueryStep.Duration)
	assert.Equal(t, attunev1alpha1.ControlledRequestsAndLimits, *policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.ControlledRequestsAndLimits, *policy.Spec.Memory.ControlledValues)
	require.NotNil(t, policy.Spec.ExcludeKnownSidecars)
	assert.False(t, *policy.Spec.ExcludeKnownSidecars)
}

func TestMergeDefaults_ClusterDefaultsTakeEffect(t *testing.T) {
	// This is the #267 regression test: verify that cluster defaults actually
	// override built-in defaults when the webhook does not pre-fill fields.
	cooldown := metav1.Duration{Duration: 30 * time.Minute}
	autoRevert := false
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(80),
			},
			Memory: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(60),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                 attunev1alpha1.UpdateTypeAuto,
				Cooldown:             &cooldown,
				AutoRevert:           &autoRevert,
				ResizeMethod:         attunev1alpha1.ResizeMethodInPlaceOrRecreate,
				MaxConcurrentResizes: 5,
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with ALL fields unset (as if no webhook defaulting occurred).
	policy := &attunev1alpha1.AttunePolicy{}

	r.mergeDefaults(policy, defaults)
	r.applyBuiltInDefaults(policy)

	// Cluster defaults should take effect.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	assert.Equal(t, int32(5), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(60), *policy.Spec.Memory.MaxChangePercent)
}

func TestMergeAndApplyDefaults_PartialClusterDefaults(t *testing.T) {
	// Admin sets only Mode and CPU MaxChangePercent; everything else nil.
	// After mergeDefaults + applyBuiltInDefaults, the inherited fields must
	// be preserved and the rest must get built-in defaults.
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				MaxChangePercent: int32Ptr(80),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeAuto,
			},
		},
	}
	r := newReconcilerWithClient(defaults)
	policy := &attunev1alpha1.AttunePolicy{}

	r.mergeDefaults(policy, defaults)
	// Verify partial state before applyBuiltInDefaults.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	assert.Nil(t, policy.Spec.Memory.MaxChangePercent,
		"should still be nil before applyBuiltInDefaults")
	assert.Nil(t, policy.Spec.UpdateStrategy.AutoRevert)

	r.applyBuiltInDefaults(policy)
	// Inherited values preserved.
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	// Built-in defaults fill the rest.
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, attunev1alpha1.DefaultMemoryMaxChangePercent, *policy.Spec.Memory.MaxChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.True(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.DefaultResizeMethod, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, attunev1alpha1.DefaultMinimumDataPoints, *policy.Spec.MetricsSource.MinimumDataPoints)
}

func TestMergeDefaults_QueryStepPolicyOverrides(t *testing.T) {
	defaultStep := metav1.Duration{Duration: 30 * time.Second}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep: &defaultStep,
			},
		},
	}
	r := newReconcilerWithClient(defaults)

	policyStep := metav1.Duration{Duration: 1 * time.Minute}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	policy.Spec.MetricsSource.QueryStep = &policyStep

	r.mergeDefaults(policy, defaults)

	// Policy-level value should NOT be overwritten.
	assert.Equal(t, 1*time.Minute, policy.Spec.MetricsSource.QueryStep.Duration)
}

func TestMergeDefaults_PolicyOverridesDefaults(t *testing.T) {
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU:    &attunev1alpha1.ResourceConfig{Percentile: 90, Overhead: "50"},
			Memory: &attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "40"},
		},
	}
	r := newReconcilerWithClient(defaults)

	// Policy with explicit values (should NOT be overwritten).
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU:    attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "10"},
			Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "20"},
		},
	}

	r.mergeDefaults(policy, nil)

	assert.Equal(t, int32(99), policy.Spec.CPU.Percentile)
	assert.Equal(t, "10", policy.Spec.CPU.Overhead)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "20", policy.Spec.Memory.Overhead)
}
