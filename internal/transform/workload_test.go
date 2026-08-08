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
	batchv1 "k8s.io/api/batch/v1"
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

func TestStripStatefulSetFields_PreservesSelectorAndReplicas(t *testing.T) {
	replicas := int32(3)
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "sts",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "sts"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c",
						Image: "busybox",
						Env:   []corev1.EnvVar{{Name: "A", Value: "1"}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}},
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas: 3,
			Conditions:    []appsv1.StatefulSetCondition{{Type: "Ready"}},
		},
	}
	out, err := StripStatefulSetFields(s)
	require.NoError(t, err)
	stripped := out.(*appsv1.StatefulSet)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, int32(3), *stripped.Spec.Replicas)
	assert.Equal(t, map[string]string{"app": "sts"}, stripped.Spec.Selector.MatchLabels)
	require.Len(t, stripped.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "c", stripped.Spec.Template.Spec.Containers[0].Name)
	assert.Empty(t, stripped.Spec.Template.Spec.Containers[0].Image)
	assert.Nil(t, stripped.Spec.VolumeClaimTemplates)
	assert.Equal(t, int32(3), stripped.Status.ReadyReplicas)
	assert.Nil(t, stripped.Status.Conditions)
}

func TestStripDaemonSetFields_PreservesSelector(t *testing.T) {
	d := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "ds",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img", Env: []corev1.EnvVar{{Name: "E"}}}},
				},
			},
		},
		Status: appsv1.DaemonSetStatus{
			NumberReady: 2,
			Conditions:  []appsv1.DaemonSetCondition{{Type: "Available"}},
		},
	}
	out, err := StripDaemonSetFields(d)
	require.NoError(t, err)
	stripped := out.(*appsv1.DaemonSet)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, map[string]string{"app": "ds"}, stripped.Spec.Selector.MatchLabels)
	require.Len(t, stripped.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, stripped.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, int32(2), stripped.Status.NumberReady)
	assert.Nil(t, stripped.Status.Conditions)
}

func TestStripReplicaSetFields_PreservesSelector(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "rs",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "rs"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				},
			},
		},
		Status: appsv1.ReplicaSetStatus{
			ReadyReplicas: 1,
			Conditions:    []appsv1.ReplicaSetCondition{{Type: "ReplicaFailure"}},
		},
	}
	out, err := StripReplicaSetFields(rs)
	require.NoError(t, err)
	stripped := out.(*appsv1.ReplicaSet)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, map[string]string{"app": "rs"}, stripped.Spec.Selector.MatchLabels)
	require.Len(t, stripped.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, stripped.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, int32(1), stripped.Status.ReadyReplicas)
	assert.Nil(t, stripped.Status.Conditions)
}

func TestStripJobFields_PreservesSelector(t *testing.T) {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "job",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
		},
		Spec: batchv1.JobSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job": "j"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img", Env: []corev1.EnvVar{{Name: "E"}}}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete}},
		},
	}
	out, err := StripJobFields(j)
	require.NoError(t, err)
	stripped := out.(*batchv1.Job)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, map[string]string{"job": "j"}, stripped.Spec.Selector.MatchLabels)
	require.Len(t, stripped.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, stripped.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, int32(1), stripped.Status.Succeeded)
	assert.Nil(t, stripped.Status.Conditions)
}

func TestStripCronJobFields_PreservesSchedule(t *testing.T) {
	c := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "cj",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "c", Image: "img"}},
						},
					},
				},
			},
		},
		Status: batchv1.CronJobStatus{
			Active: []corev1.ObjectReference{{Name: "job-1"}},
		},
	}
	out, err := StripCronJobFields(c)
	require.NoError(t, err)
	stripped := out.(*batchv1.CronJob)
	assert.Nil(t, stripped.ManagedFields)
	assert.Equal(t, "0 * * * *", stripped.Spec.Schedule)
	require.Len(t, stripped.Spec.JobTemplate.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, stripped.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image)
	assert.Nil(t, stripped.Status.Active)
}

func TestStripWorkloadFields_WrongTypePassthrough(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	for _, fn := range []func(any) (any, error){
		StripStatefulSetFields,
		StripDaemonSetFields,
		StripReplicaSetFields,
		StripJobFields,
		StripCronJobFields,
		StripDeploymentFields,
		StripHPAFields,
	} {
		out, err := fn(pod)
		require.NoError(t, err)
		assert.Same(t, pod, out)
	}
}
