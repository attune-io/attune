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
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
	}
	out, err := f.Transform(keepAPI)
	require.NoError(t, err)
	got := out.(*corev1.Pod)
	require.Len(t, got.Spec.Containers, 1)
	assert.Empty(t, got.Spec.Containers[0].Image) // stripped

	keepStatic := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p2", Labels: map[string]string{"team": "payments"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "y"}}},
	}
	out, err = f.Transform(keepStatic)
	require.NoError(t, err)
	assert.NotEmpty(t, out.(*corev1.Pod).Spec.Containers)

	tracked := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p3", Labels: map[string]string{LabelTracked: "true", "app": "other"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "z"}}},
	}
	out, err = f.Transform(tracked)
	require.NoError(t, err)
	assert.NotEmpty(t, out.(*corev1.Pod).Spec.Containers)

	drop := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p4", Labels: map[string]string{"app": "other"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "z", Env: []corev1.EnvVar{{Name: "A"}}}}},
	}
	out, err = f.Transform(drop)
	require.NoError(t, err)
	stub := out.(*corev1.Pod)
	assert.Empty(t, stub.Spec.Containers)
	assert.Equal(t, "p4", stub.Name)
	assert.Equal(t, "other", stub.Labels["app"])
}

func TestPodCacheFilter_DisabledKeepsAll(t *testing.T) {
	f := NewPodCacheFilter(nil)
	// enabled false until UpdateDynamic
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{"app": "x"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
	}
	out, err := f.Transform(pod)
	require.NoError(t, err)
	assert.Empty(t, out.(*corev1.Pod).Spec.Containers[0].Image)
}
