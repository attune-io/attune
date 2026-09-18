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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------- appendResizedContainer ----------

func TestAppendResizedContainer(t *testing.T) {
	tests := []struct {
		name      string
		existing  string
		container string
		want      string
	}{
		{"first container on empty", "", "main", "main"},
		{"append second container", "main", "sidecar", "main,sidecar"},
		{"dedup existing container", "main", "main", "main"},
		{"dedup in multi-container list", "main,sidecar", "main", "main,sidecar"},
		{"append third container", "main,sidecar", "worker", "main,sidecar,worker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			}}
			if tt.existing != "" {
				pod.Annotations[annotationResizedContainers] = tt.existing
			}
			appendResizedContainer(pod, tt.container)
			assert.Equal(t, tt.want, pod.Annotations[annotationResizedContainers])
		})
	}
}

// ---------- parseResizeRecords ----------

func TestParseResizeRecords_MultiContainer(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                              resizedAt,
				annotationResizedContainers:                      "app,sidecar",
				annotationOriginalCPUPrefix + "app":              "500m",
				annotationOriginalMemoryPrefix + "app":           "512Mi",
				annotationOriginalRestartCountPrefix + "app":     "0",
				annotationOriginalCPUPrefix + "sidecar":          "100m",
				annotationOriginalMemoryPrefix + "sidecar":       "128Mi",
				annotationOriginalRestartCountPrefix + "sidecar": "2",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			}},
			{Name: "sidecar", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			}},
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "app", records[0].Container)
	assert.Equal(t, "sidecar", records[1].Container)
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("500m")))
	assert.True(t, records[1].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.Equal(t, int32(2), records[1].RestartCount)
}

func TestParseResizeRecords_RestoresLimits(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "limits-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "100m",
				annotationOriginalMemoryPrefix + "app":       "64Mi",
				annotationOriginalCPULimitPrefix + "app":     "200m",
				annotationOriginalMemoryLimitPrefix + "app":  "128Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("150m"),
					corev1.ResourceMemory: resource.MustParse("96Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("192Mi"),
				},
			}},
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 1)
	// Requests restored from annotations.
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.True(t, records[0].OriginalResources.Requests.Memory().Equal(resource.MustParse("64Mi")))
	// Limits restored from limit annotations.
	require.NotNil(t, records[0].OriginalResources.Limits, "Limits should be populated from limit annotations")
	assert.True(t, records[0].OriginalResources.Limits.Cpu().Equal(resource.MustParse("200m")))
	assert.True(t, records[0].OriginalResources.Limits.Memory().Equal(resource.MustParse("128Mi")))
}

func TestParseResizeRecords_NoLimitAnnotations(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "no-limits-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "100m",
				annotationOriginalMemoryPrefix + "app":       "64Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("150m"),
					corev1.ResourceMemory: resource.MustParse("96Mi"),
				},
			}},
		}},
	}

	records, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.True(t, records[0].OriginalResources.Requests.Cpu().Equal(resource.MustParse("100m")))
	// Limits should be nil when no limit annotations exist (backwards compat).
	assert.Nil(t, records[0].OriginalResources.Limits, "Limits should be nil when limit annotations are absent")
}

func TestParseResizeRecords_MissingCPUAnnotation(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                    resizedAt,
				annotationResizedContainers:            "app",
				annotationOriginalMemoryPrefix + "app": "512Mi",
			},
		},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing original CPU for app")
}

func TestParseResizeRecords_InvalidTimestamp(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt: "not-a-timestamp",
			},
		},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing resized-at annotation")
}

func TestParseResizeRecords_MalformedRestartCount(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-pod", Namespace: "default",
			Annotations: map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "500m",
				annotationOriginalMemoryPrefix + "app":       "512Mi",
				annotationOriginalRestartCountPrefix + "app": "not-a-number",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
		}},
	}

	_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing original restart count for app")
}

func TestParseResizeRecords_MalformedLimitAnnotations(t *testing.T) {
	resizedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	testCases := []struct {
		name          string
		podName       string
		annotations   map[string]string
		errorContains string
	}{
		{
			name:    "cpu limit",
			podName: "bad-cpu-limit-pod",
			annotations: map[string]string{
				annotationOriginalCPULimitPrefix + "app": "not-a-quantity",
			},
			errorContains: "parsing original CPU limit for app",
		},
		{
			name:    "memory limit",
			podName: "bad-memory-limit-pod",
			annotations: map[string]string{
				annotationOriginalMemoryLimitPrefix + "app": "not-a-quantity",
			},
			errorContains: "parsing original memory limit for app",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{
				annotationResizedAt:                          resizedAt,
				annotationResizedContainers:                  "app",
				annotationOriginalCPUPrefix + "app":          "500m",
				annotationOriginalMemoryPrefix + "app":       "512Mi",
				annotationOriginalRestartCountPrefix + "app": "0",
			}
			for key, value := range tc.annotations {
				annotations[key] = value
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        tc.podName,
					Namespace:   "default",
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			}

			_, err := parseResizeRecords(pod, 5*time.Minute, time.Now())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errorContains)
		})
	}
}

func TestRemoveTrackingAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":               "test",
				"attune.io/tracked": "true",
				"unrelated":         "keep",
			},
			Annotations: map[string]string{
				"attune.io/resized-at":                      "2026-01-01T00:00:00Z",
				"attune.io/resized-workload":                "api-server",
				"attune.io/resized-containers":              "main,sidecar",
				"attune.io/original-cpu-request.main":       "500m",
				"attune.io/original-memory-request.main":    "512Mi",
				"attune.io/original-cpu-limit.main":         "1000m",
				"attune.io/original-memory-limit.main":      "1Gi",
				"attune.io/original-restart-count.main":     "0",
				"attune.io/original-cpu-request.sidecar":    "100m",
				"attune.io/original-memory-request.sidecar": "64Mi",
				"attune.io/original-cpu-limit.sidecar":      "200m",
				"attune.io/original-memory-limit.sidecar":   "128Mi",
				"attune.io/original-restart-count.sidecar":  "2",
				"unrelated-annotation":                      "keep",
			},
		},
	}

	removeTrackingAnnotations(pod)

	// Tracking label should be removed.
	_, hasTracked := pod.Labels["attune.io/tracked"]
	assert.False(t, hasTracked, "tracked label should be removed")
	assert.Equal(t, "keep", pod.Labels["unrelated"], "unrelated labels should be preserved")

	// All tracking annotations should be removed.
	for key := range pod.Annotations {
		assert.False(t, strings.HasPrefix(key, "attune.io/"),
			"tracking annotation %q should be removed", key)
	}
	assert.Equal(t, "keep", pod.Annotations["unrelated-annotation"],
		"unrelated annotations should be preserved")
}

// safetyTestDeploy is the deployment used by safety observation tests.
// Declared at package level so tests can pass it as the workloads arg
// to checkPendingSafetyObservations.
var safetyTestDeploy = newTestDeployment("api-server", "default", nil)

func TestTrackingAnnotationsApplied(t *testing.T) {
	intended := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{labelTracked: "true"},
			Annotations: map[string]string{
				annotationResizedAt:                           "2026-03-01T12:00:00Z",
				annotationResizedWorkload:                     "api-server",
				annotationPolicy:                              "test-policy",
				annotationResizedContainers:                   "main",
				annotationOriginalCPUPrefix + "main":          "500m",
				annotationOriginalMemoryPrefix + "main":       "512Mi",
				annotationOriginalCPULimitPrefix + "main":     "1000m",
				annotationOriginalMemoryLimitPrefix + "main":  "1Gi",
				annotationOriginalRestartCountPrefix + "main": "3",
			},
		},
	}
	clone := func() *corev1.Pod { return intended.DeepCopy() }

	t.Run("match", func(t *testing.T) {
		assert.True(t, trackingAnnotationsApplied(clone(), intended, "main"))
	})
	t.Run("stale resized-at is not this persist", func(t *testing.T) {
		got := clone()
		got.Annotations[annotationResizedAt] = "2026-01-01T00:00:00Z"
		assert.False(t, trackingAnnotationsApplied(got, intended, "main"))
	})
	t.Run("missing container", func(t *testing.T) {
		got := clone()
		got.Annotations[annotationResizedContainers] = "sidecar"
		assert.False(t, trackingAnnotationsApplied(got, intended, "main"))
	})
	t.Run("container listed among others", func(t *testing.T) {
		got := clone()
		got.Annotations[annotationResizedContainers] = "sidecar,main"
		assert.True(t, trackingAnnotationsApplied(got, intended, "main"))
	})
}

type failOnNamedPodUpdateClient struct {
	client.Client
	failPodName string
}
