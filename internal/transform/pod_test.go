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

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStripPodFields(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test", "rightsize.io/tracked": "true"},
			Annotations: map[string]string{
				"rightsize.io/resized-at": "2026-01-01T00:00:00Z",
				"other-annotation":        "keep",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{
				{
					Name:    "app",
					Image:   "nginx:latest",
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", "sleep infinity"},
					Env: []corev1.EnvVar{
						{Name: "FOO", Value: "bar"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/data"},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
					ResizePolicy: []corev1.ContainerResizePolicy{
						{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
					},
					LivenessProbe:  &corev1.Probe{InitialDelaySeconds: 10},
					ReadinessProbe: &corev1.Probe{InitialDelaySeconds: 5},
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name:  "init",
					Image: "busybox",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0, Ready: true},
			},
			QOSClass: corev1.PodQOSBurstable,
		},
	}

	result, err := StripPodFields(pod)
	require.NoError(t, err)
	stripped := result.(*corev1.Pod)

	// Preserved fields.
	assert.Equal(t, "test-pod", stripped.Name)
	assert.Equal(t, "default", stripped.Namespace)
	assert.Equal(t, "node-1", stripped.Spec.NodeName)
	assert.Equal(t, map[string]string{"app": "test", "rightsize.io/tracked": "true"}, stripped.Labels)
	assert.Contains(t, stripped.Annotations, "rightsize.io/resized-at")
	assert.Contains(t, stripped.Annotations, "other-annotation")
	assert.Equal(t, corev1.PodRunning, stripped.Status.Phase)
	assert.Len(t, stripped.Status.Conditions, 1)
	assert.Len(t, stripped.Status.ContainerStatuses, 1)
	assert.Equal(t, corev1.PodQOSBurstable, stripped.Status.QOSClass)

	// Container preserved fields.
	require.Len(t, stripped.Spec.Containers, 1)
	c := stripped.Spec.Containers[0]
	assert.Equal(t, "app", c.Name)
	assert.Equal(t, resource.MustParse("500m"), c.Resources.Requests[corev1.ResourceCPU])
	assert.Len(t, c.ResizePolicy, 1)

	// Container stripped fields.
	assert.Empty(t, c.Image)
	assert.Nil(t, c.Command)
	assert.Nil(t, c.Args)
	assert.Nil(t, c.Env)
	assert.Nil(t, c.VolumeMounts)
	assert.Nil(t, c.LivenessProbe)
	assert.Nil(t, c.ReadinessProbe)
	assert.Nil(t, c.Ports)

	// Init container preserved fields.
	require.Len(t, stripped.Spec.InitContainers, 1)
	ic := stripped.Spec.InitContainers[0]
	assert.Equal(t, "init", ic.Name)
	assert.Equal(t, resource.MustParse("100m"), ic.Resources.Requests[corev1.ResourceCPU])

	// Init container stripped fields.
	assert.Empty(t, ic.Image)

	// Pod-level stripped fields.
	assert.Nil(t, stripped.ManagedFields)
	assert.Nil(t, stripped.Spec.Volumes)
	assert.Nil(t, stripped.Spec.EphemeralContainers)
}

func TestStripPodFields_NonPodObject(t *testing.T) {
	obj := "not a pod"
	result, err := StripPodFields(obj)
	require.NoError(t, err)
	assert.Equal(t, "not a pod", result)
}

func TestStripPodFields_NilContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-pod"},
	}
	result, err := StripPodFields(pod)
	require.NoError(t, err)
	stripped := result.(*corev1.Pod)
	assert.Equal(t, "empty-pod", stripped.Name)
}
