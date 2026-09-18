/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or applied to in writing, software
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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/lifecycle"
)

const smObservation = 5 * time.Minute

// safetyLifecycleSM drives Classify via the same annotation helpers the
// reconciler uses. Incomplete is first-class (tracking may remain).
type safetyLifecycleSM struct {
	now     time.Time
	pod     *corev1.Pod
	policy  *attunev1alpha1.AttunePolicy
	origCPU resource.Quantity
}

func (sm *safetyLifecycleSM) reset() {
	sm.now = time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	sm.origCPU = resource.MustParse("500m")
	persist := true
	sm.policy = &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				TemplatePersistence: &attunev1alpha1.TemplatePersistence{
					Enabled: &persist,
					When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
				},
			},
		},
	}
	sm.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sm-pod",
			Namespace:   "default",
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				},
			}},
		},
	}
}

func (sm *safetyLifecycleSM) Check(t *rapid.T) {
	t.Helper()
	in := safetyLifecycleInput(sm.policy, sm.pod, sm.now, smObservation)
	phase := lifecycle.Classify(in)
	switch phase {
	case lifecycle.Idle:
		if trackingKeysPresent(sm.pod) {
			t.Fatalf("Idle still has tracking keys: labels=%v annotations=%v",
				sm.pod.Labels, sm.pod.Annotations)
		}
	case lifecycle.Incomplete:
		if !in.Tracked && !in.HasResizedAt {
			t.Fatalf("Incomplete requires tracking or resized-at")
		}
		if in.HasResizedAt && !in.ResizedAt.IsZero() {
			t.Fatalf("Incomplete with a parsed resized-at")
		}
	case lifecycle.Observing:
		if sm.now.Sub(in.ResizedAt) >= smObservation {
			t.Fatalf("Observing after period elapsed")
		}
	case lifecycle.Evaluating:
		if sm.now.Sub(in.ResizedAt) < smObservation {
			t.Fatalf("Evaluating before period elapsed")
		}
	case lifecycle.RestorePending:
		if !in.PersistAfterSuccess || !in.LiveMatchesOriginal {
			t.Fatalf("RestorePending without persist+live match")
		}
	default:
		t.Fatalf("unexpected phase %q", phase)
	}
}

func (sm *safetyLifecycleSM) ApplyResize(t *rapid.T) {
	if sm.pod.Annotations[annotationResizedAt] != "" {
		t.Skip("already tracking")
	}
	if sm.pod.Annotations == nil {
		sm.pod.Annotations = map[string]string{}
	}
	if sm.pod.Labels == nil {
		sm.pod.Labels = map[string]string{}
	}
	sm.pod.Labels[labelTracked] = "true"
	sm.pod.Annotations[annotationResizedAt] = sm.now.UTC().Format(time.RFC3339)
	sm.pod.Annotations[annotationResizedContainers] = "app"
	sm.pod.Annotations[annotationResizedWorkload] = "api-server"
	sm.pod.Annotations[annotationPolicy] = "sm-policy"
	sm.pod.Annotations[annotationOriginalCPUPrefix+"app"] = sm.origCPU.String()
	sm.pod.Annotations[annotationOriginalMemoryPrefix+"app"] = "512Mi"
	sm.pod.Annotations[annotationOriginalRestartCountPrefix+"app"] = "0"
	sm.pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
}

func (sm *safetyLifecycleSM) AdvanceClock(t *rapid.T) {
	mins := rapid.IntRange(1, 20).Draw(t, "minutes")
	sm.now = sm.now.Add(time.Duration(mins) * time.Minute)
}

func (sm *safetyLifecycleSM) CorruptResizedAt(t *rapid.T) {
	if sm.pod.Annotations[annotationResizedAt] == "" && sm.pod.Labels[labelTracked] != "true" {
		t.Skip("no tracking to corrupt")
	}
	if sm.pod.Annotations == nil {
		sm.pod.Annotations = map[string]string{}
	}
	sm.pod.Annotations[annotationResizedAt] = "not-rfc3339"
}

func (sm *safetyLifecycleSM) RestoreLiveToOriginal(t *rapid.T) {
	if sm.pod.Annotations[annotationResizedAt] == "" {
		t.Skip("no resize to restore")
	}
	sm.pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = sm.origCPU.DeepCopy()
	sm.pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("512Mi")
}

func (sm *safetyLifecycleSM) ClearTracking(t *rapid.T) {
	if !trackingKeysPresent(sm.pod) {
		t.Skip("already idle")
	}
	removeTrackingAnnotations(sm.pod)
}

func trackingKeysPresent(pod *corev1.Pod) bool {
	if pod.Labels[labelTracked] != "" {
		return true
	}
	for k := range pod.Annotations {
		if k == annotationResizedAt || k == annotationResizedContainers ||
			k == annotationResizedWorkload || k == annotationPolicy {
			return true
		}
		if strings.HasPrefix(k, annotationOriginalCPUPrefix) ||
			strings.HasPrefix(k, annotationOriginalMemoryPrefix) ||
			strings.HasPrefix(k, annotationOriginalCPULimitPrefix) ||
			strings.HasPrefix(k, annotationOriginalMemoryLimitPrefix) ||
			strings.HasPrefix(k, annotationOriginalRestartCountPrefix) {
			return true
		}
	}
	return false
}

func TestSafetyLifecycleStateMachine(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		sm := &safetyLifecycleSM{}
		sm.reset()
		rt.Repeat(rapid.StateMachineActions(sm))
	})
}
