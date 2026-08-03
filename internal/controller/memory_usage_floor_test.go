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
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestApplyMemoryUsageFloor_RaisesUnsafeLimit(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	margin := int32(10)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			Memory: attunev1alpha1.ResourceConfig{
				DecreaseUsageMarginPercent: &margin,
			},
		},
	}
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryLimit: resource.MustParse("1Gi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("500Mi"),
			},
		},
	}
	target := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("200Mi")},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("200Mi")},
	}

	before := promtestutil.ToFloat64(operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
		"default", "p", "clamped_usage"))
	got := r.applyMemoryUsageFloor(context.Background(), policy, pod, rec, target)
	// 500Mi * 1.1 = 550Mi
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("550Mi")),
		"got %s", got.Limits.Memory().String())
	assert.Equal(t, before+1, promtestutil.ToFloat64(operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
		"default", "p", "clamped_usage")))
}

func TestApplyMemoryUsageFloor_SkippedUnsafeIncrementsMetric(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Usage is above the current limit: floor clamps back to current → skipped_unsafe.
	margin := int32(10)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p-skip", Namespace: "ns-skip"},
		Spec: attunev1alpha1.AttunePolicySpec{
			Memory: attunev1alpha1.ResourceConfig{
				DecreaseUsageMarginPercent: &margin,
			},
		},
	}
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns-skip"}}
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryLimit: resource.MustParse("200Mi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("300Mi"),
			},
		},
	}
	target := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
	}

	before := promtestutil.ToFloat64(operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
		"ns-skip", "p-skip", "skipped_unsafe"))
	got := r.applyMemoryUsageFloor(context.Background(), policy, pod, rec, target)
	// Floor = max(target, usage*(1+m/100)) but never above current → stays 200Mi.
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("200Mi")),
		"got %s", got.Limits.Memory().String())
	assert.Equal(t, before+1, promtestutil.ToFloat64(operatormetrics.MemoryLimitDecreaseTotal.WithLabelValues(
		"ns-skip", "p-skip", "skipped_unsafe")))
}

func TestApplyMemoryUsageFloor_SafeDecreaseUnchanged(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	margin := int32(10)
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			Memory: attunev1alpha1.ResourceConfig{
				DecreaseUsageMarginPercent: &margin,
			},
		},
	}
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	rec := attunev1alpha1.ContainerRecommendation{
		Name: "app",
		Current: attunev1alpha1.ResourceValues{
			MemoryLimit: resource.MustParse("1Gi"),
		},
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("200Mi"),
			},
		},
	}
	target := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("800Mi")},
	}

	got := r.applyMemoryUsageFloor(context.Background(), policy, pod, rec, target)
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("800Mi")))
}

func TestRecentMemoryUsage(t *testing.T) {
	t.Parallel()
	_, ok := recentMemoryUsage(attunev1alpha1.ContainerRecommendation{})
	assert.False(t, ok)

	u, ok := recentMemoryUsage(attunev1alpha1.ContainerRecommendation{
		Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
			Memory: &attunev1alpha1.ResourceRecommendationExplanation{
				RawPercentile: resource.MustParse("256Mi"),
			},
		},
	})
	assert.True(t, ok)
	assert.True(t, u.Equal(resource.MustParse("256Mi")))
}
