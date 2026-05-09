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

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

func TestIsCooldownActive_NoAnnotation(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_RecentTime(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// 5 minutes ago with 1-hour cooldown: still active.
	assert.True(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_OldTime(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			},
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// 2 hours ago with 1-hour cooldown: expired.
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_InvalidAnnotation(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: "not-a-valid-timestamp",
			},
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 1 * time.Hour},
			},
		},
	}
	// Invalid annotation value is treated as no previous resize.
	assert.False(t, r.isCooldownActive(policy))
}

func TestIsCooldownActive_CustomCooldownDuration(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				lastResizeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
			},
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 30 * time.Minute},
			},
		},
	}
	// 20 minutes ago with 30-minute cooldown: still active.
	assert.True(t, r.isCooldownActive(policy))
}
