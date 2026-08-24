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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestIsCooldownActive_NoAnnotation(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_RecentTime(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// 5 minutes ago with 1-hour cooldown: still active.
	assert.True(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_OldTime(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// 2 hours ago with 1-hour cooldown: expired.
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_InvalidAnnotation(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: "not-a-valid-timestamp",
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// Invalid annotation value is treated as no previous resize.
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_CustomCooldownDuration(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 30 * time.Minute},
			},
		},
	}
	// 20 minutes ago with 30-minute cooldown: still active.
	assert.True(t, r.isCooldownActive(policy))
}

func TestIsWorkloadCooldownActive_IsolatesApps(t *testing.T) {
	r := NewAttunePolicyReconciler()
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return now })
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotationKey("app-a"): now.Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	assert.True(t, r.isWorkloadCooldownActive(policy, "app-a"))
	assert.False(t, r.isWorkloadCooldownActive(policy, "app-b"), "unrelated app must not inherit A's cooldown")
	assert.False(t, r.allWorkloadsCooling(policy, []string{"app-a", "app-b"}))
	assert.True(t, r.allWorkloadsCooling(policy, []string{"app-a"}))
}

func TestIsWorkloadCooldownActive_FallsBackToPolicyWide(t *testing.T) {
	r := NewAttunePolicyReconciler()
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return now })
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: now.Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	assert.True(t, r.isWorkloadCooldownActive(policy, "app-a"), "missing per-app key falls back to policy-wide")
	assert.True(t, r.isCooldownActive(policy))
}

func TestIsWorkloadCooldownActive_MarkResizeTimeDoesNotLockSiblings(t *testing.T) {
	r := NewAttunePolicyReconciler()
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	r.SetNowFunc(func() time.Time { return now })
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation:             now.Format(time.RFC3339),
				lastResizeAnnotationKey("app-a"): now.Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	assert.True(t, r.isWorkloadCooldownActive(policy, "app-a"))
	assert.False(t, r.isWorkloadCooldownActive(policy, "app-b"), "B must not inherit the policy-wide stamp once any per-app key exists")
}

func TestIsWorkloadCooldownActive_MalformedPerWorkload(t *testing.T) {
	r := NewAttunePolicyReconciler()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotationKey("app-a"): "not-a-valid-timestamp",
				lastResizeAnnotation:             time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// Malformed per-workload value is treated as no previous resize for that app
	// (do not silently fall back to policy-wide after a bad per-app stamp).
	assert.False(t, r.isWorkloadCooldownActive(policy, "app-a"))
}

func TestGetEffectiveCooldown_PerWorkloadBackoff(t *testing.T) {
	r := NewAttunePolicyReconciler()
	cooldown := 1 * time.Hour
	ts := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				flippedSuccessRevert("app-a", "InPlace", ts, "oomkill"),
				flippedSuccessRevert("app-a", "InPlace", ts.Add(time.Minute), "oomkill"),
				{Workload: "app-b", Method: "InPlace", Result: attunev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(ts)},
			},
		},
	}
	assert.Equal(t, 4*cooldown, r.getEffectiveCooldown(policy, "app-a"), "two reverts on A → 4x")
	assert.Equal(t, cooldown, r.getEffectiveCooldown(policy, "app-b"), "B has no revert streak")
}
