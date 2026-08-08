/*
Copyright 2026.
*/

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestPodCacheFilter_KeepsMatchingAndTracked(t *testing.T) {
	static, err := labels.Parse("team=payments")
	require.NoError(t, err)
	f := NewPodCacheFilter(static)
	f.UpdateDynamic([]labels.Selector{SelectorFromMap(map[string]string{"app": "api"})})

	keepAPI := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Labels: map[string]string{"app": "api"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c", Image: "x",
			Resources: corev1.ResourceRequirements{},
		}}},
	}
	assert.True(t, f.Keep(keepAPI))
	out, err := f.Transform(keepAPI)
	require.NoError(t, err)
	got := out.(*corev1.Pod)
	require.Len(t, got.Spec.Containers, 1)
	assert.Empty(t, got.Spec.Containers[0].Image) // stripped, Spec retained

	keepStatic := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p2", Labels: map[string]string{"team": "payments"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "y"}}},
	}
	assert.True(t, f.Keep(keepStatic))
	out, err = f.Transform(keepStatic)
	require.NoError(t, err)
	assert.Len(t, out.(*corev1.Pod).Spec.Containers, 1)

	tracked := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p3", Labels: map[string]string{LabelTracked: "true", "app": "other"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "z"}}},
	}
	assert.True(t, f.Keep(tracked))

	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p4", Labels: map[string]string{"app": "other"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "z", Env: []corev1.EnvVar{{Name: "A"}}}}},
	}
	assert.False(t, f.Keep(other))
	// Transform never stubs: Spec containers remain (env stripped).
	out, err = f.Transform(other)
	require.NoError(t, err)
	stripped := out.(*corev1.Pod)
	require.Len(t, stripped.Spec.Containers, 1)
	assert.Empty(t, stripped.Spec.Containers[0].Env)
	assert.Empty(t, stripped.Spec.Containers[0].Image)
}

func TestPodCacheFilter_DisabledKeepsAll(t *testing.T) {
	f := NewPodCacheFilter(nil)
	// enabled false until UpdateDynamic
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{"app": "x"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
	}
	assert.True(t, f.Keep(pod))
	out, err := f.Transform(pod)
	require.NoError(t, err)
	assert.Empty(t, out.(*corev1.Pod).Spec.Containers[0].Image)
	assert.Len(t, out.(*corev1.Pod).Spec.Containers, 1)
}

func TestPodCacheFilter_SetEnabled(t *testing.T) {
	f := NewPodCacheFilter(nil)
	f.UpdateDynamic([]labels.Selector{SelectorFromMap(map[string]string{"app": "api"})})
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{"app": "other"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}
	assert.False(t, f.Keep(other), "enabled after UpdateDynamic should drop non-matches")
	f.SetEnabled(false)
	assert.True(t, f.Keep(other), "SetEnabled(false) keeps all for diagnostics")
	f.SetEnabled(true)
	assert.False(t, f.Keep(other))
	// nil receiver is a no-op (must not panic).
	var nilF *PodCacheFilter
	nilF.SetEnabled(true)
	assert.True(t, nilF.Keep(other))
}

func TestSelectorFromMap_Empty(t *testing.T) {
	assert.Nil(t, SelectorFromMap(nil))
	assert.Nil(t, SelectorFromMap(map[string]string{}))
	sel := SelectorFromMap(map[string]string{"app": "x"})
	require.NotNil(t, sel)
	assert.True(t, sel.Matches(labels.Set{"app": "x"}))
}
