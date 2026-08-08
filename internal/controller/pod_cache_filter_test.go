/*
Copyright 2026.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/transform"
)

func TestRefreshPodCacheFilter_NilFilterNoOp(t *testing.T) {
	r := NewAttunePolicyReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(testScheme()).Build()
	// Must not panic when PodCacheFilter is unset.
	r.refreshPodCacheFilter(context.Background())
}

func TestRefreshPodCacheFilter_UpdateDynamicFromPolicy(t *testing.T) {
	scheme := testScheme()
	deployName := "api"
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &deployName,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, policy).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.PodCacheFilter = transform.NewPodCacheFilter(nil)

	fixed := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return fixed })

	r.refreshPodCacheFilter(context.Background())

	keep := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "match", Labels: map[string]string{"app": "api"}},
	}
	drop := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"app": "other"}},
	}
	assert.True(t, r.PodCacheFilter.Keep(keep), "pod matching policy target selector should be kept")
	assert.False(t, r.PodCacheFilter.Keep(drop), "unrelated pod should not match keep diagnostics")
}

func TestRefreshPodCacheFilter_ThrottleSkipsWithinWindow(t *testing.T) {
	scheme := testScheme()
	nameA := "app-a"
	nameB := "app-b"
	deployA := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: nameA, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
		},
	}
	deployB := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: nameB, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
		},
	}
	policyA := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pa", Namespace: "ns"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &nameA},
		},
	}
	policyB := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pb", Namespace: "ns"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &nameB},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployA, deployB, policyA).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.PodCacheFilter = transform.NewPodCacheFilter(nil)

	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return now })

	// First refresh installs selector for app=a.
	r.refreshPodCacheFilter(context.Background())
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}}}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "b"}}}
	assert.True(t, r.PodCacheFilter.Keep(podA))
	assert.False(t, r.PodCacheFilter.Keep(podB))

	// Replace policy with B; within throttle window selectors must not change.
	require.NoError(t, c.Delete(context.Background(), policyA))
	require.NoError(t, c.Create(context.Background(), policyB))
	now = now.Add(10 * time.Second)
	r.refreshPodCacheFilter(context.Background())
	assert.True(t, r.PodCacheFilter.Keep(podA), "throttled refresh must keep prior selectors")
	assert.False(t, r.PodCacheFilter.Keep(podB), "throttled refresh must not install new selectors yet")

	// After throttle elapses, rebuild picks up policy B.
	now = now.Add(refreshPodCacheFilterThrottle)
	r.refreshPodCacheFilter(context.Background())
	assert.False(t, r.PodCacheFilter.Keep(podA), "after throttle, old selector should drop")
	assert.True(t, r.PodCacheFilter.Keep(podB), "after throttle, new selector should apply")
}

func TestRefreshPodCacheFilter_ListPoliciesErrorLeavesFilter(t *testing.T) {
	scheme := testScheme()
	// Seed known selectors so we can prove List failure does not wipe them.
	f := transform.NewPodCacheFilter(nil)
	f.UpdateDynamic([]labels.Selector{transform.SelectorFromMap(map[string]string{"app": "seeded"})})
	seeded := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "seeded"}}}
	require.True(t, f.Keep(seeded))

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*attunev1alpha1.AttunePolicyList); ok {
					return fmt.Errorf("simulated list failure")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.PodCacheFilter = f
	fixed := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return fixed })

	// Must not panic; filter remains seeded.
	r.refreshPodCacheFilter(context.Background())
	assert.True(t, r.PodCacheFilter.Keep(seeded), "list error must leave existing dynamic selectors unchanged")
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}}}
	assert.False(t, r.PodCacheFilter.Keep(other))
}

func TestRefreshPodCacheFilter_TwoPoliciesSameTarget(t *testing.T) {
	scheme := testScheme()
	name := "shared"
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared"}},
		},
	}
	// Two policies targeting the same deployment should not break refresh
	// (selector map dedupes by sel.String(); Keep still matches once).
	p1 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &name},
		},
	}
	p2 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &name},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, p1, p2).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.PodCacheFilter = transform.NewPodCacheFilter(nil)
	r.SetNowFunc(func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) })

	r.refreshPodCacheFilter(context.Background())
	assert.True(t, r.PodCacheFilter.Keep(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "shared"}},
	}))
	assert.False(t, r.PodCacheFilter.Keep(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}},
	}))
}
