/*
Copyright 2026.
*/

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStripDeploymentFields_PreservesSelectorAndContainers(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "app",
			Namespace:     "ns",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "c",
						Image:   "busybox",
						Command: []string{"sleep"},
						Env:     []corev1.EnvVar{{Name: "A", Value: "1"}},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			UpdatedReplicas:   2,
			AvailableReplicas: 2,
			Conditions:        []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable}},
		},
	}
	out, err := StripDeploymentFields(d)
	require.NoError(t, err)
	stripped := out.(*appsv1.Deployment)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, map[string]string{"app": "x"}, stripped.Spec.Selector.MatchLabels)
	require.Len(t, stripped.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "c", stripped.Spec.Template.Spec.Containers[0].Name)
	assert.Empty(t, stripped.Spec.Template.Spec.Containers[0].Image)
	assert.Nil(t, stripped.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, int32(2), stripped.Status.UpdatedReplicas)
	assert.Nil(t, stripped.Status.Conditions)
}

func TestStripHPAFields_PreservesSpec(t *testing.T) {
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "h",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
			Annotations:   map[string]string{"a": "b"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "app"},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive}},
		},
	}
	out, err := StripHPAFields(h)
	require.NoError(t, err)
	stripped := out.(*autoscalingv2.HorizontalPodAutoscaler)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, "app", stripped.Spec.ScaleTargetRef.Name)
	assert.Equal(t, "b", stripped.Annotations["a"])
	assert.Nil(t, stripped.Status.Conditions)
}
