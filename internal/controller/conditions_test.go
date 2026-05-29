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
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// ---------- Resizing condition ----------

func TestSetResizingCondition_InProgress(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Workloads: attunev1alpha1.WorkloadStatus{Resized: 2},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInProgress, cond.Reason)
}

func TestSetResizingCondition_CooldownActive(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setResizingCondition(policy, true)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonCooldownActive, cond.Reason)
}

func TestSetResizingCondition_Idle(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonIdle, cond.Reason)
}

func TestSetResizingCondition_ObserveMode_NoCondition(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeObserve},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionResizing)
	assert.Nil(t, cond)
}

// ---------- Degraded condition ----------

func TestSetDegradedCondition_HighRevertRate(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultSuccess},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
			},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonHighRevertRate, cond.Reason)
}

func TestSetDegradedCondition_LowRevertRate(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				{Result: attunev1alpha1.ResizeResultSuccess},
				{Result: attunev1alpha1.ResizeResultSuccess},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultSuccess},
				{Result: attunev1alpha1.ResizeResultSuccess},
			},
		},
	}
	r := &AttunePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionDegraded)
	assert.Nil(t, cond)
}

func TestSetDegradedCondition_EmptyHistory(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	r := &AttunePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionDegraded)
	assert.Nil(t, cond)
}

// ---------- Consecutive reverts ----------

func TestConsecutiveReverts(t *testing.T) {
	tests := []struct {
		name    string
		history []attunev1alpha1.ResizeHistoryEntry
		want    int
	}{
		{"empty", nil, 0},
		{"no reverts", []attunev1alpha1.ResizeHistoryEntry{{Result: attunev1alpha1.ResizeResultSuccess}}, 0},
		{"one revert", []attunev1alpha1.ResizeHistoryEntry{{Result: attunev1alpha1.ResizeResultReverted}}, 1},
		{"three consecutive", []attunev1alpha1.ResizeHistoryEntry{
			{Result: attunev1alpha1.ResizeResultSuccess}, {Result: attunev1alpha1.ResizeResultReverted}, {Result: attunev1alpha1.ResizeResultReverted}, {Result: attunev1alpha1.ResizeResultReverted},
		}, 3},
		{"broken by success", []attunev1alpha1.ResizeHistoryEntry{
			{Result: attunev1alpha1.ResizeResultReverted}, {Result: attunev1alpha1.ResizeResultSuccess}, {Result: attunev1alpha1.ResizeResultReverted},
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, consecutiveReverts(tt.history))
		})
	}
}

// ---------- Exponential backoff ----------

func TestGetEffectiveCooldown_NoReverts(t *testing.T) {
	r := &AttunePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
	}
	assert.Equal(t, cooldown, r.getEffectiveCooldown(policy))
}

func TestGetEffectiveCooldown_TwoReverts(t *testing.T) {
	r := &AttunePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				{Result: attunev1alpha1.ResizeResultReverted}, {Result: attunev1alpha1.ResizeResultReverted},
			},
		},
	}
	// 2 reverts -> 2^2 = 4x base cooldown
	assert.Equal(t, 4*cooldown, r.getEffectiveCooldown(policy))
}

func TestGetEffectiveCooldown_CappedAt16x(t *testing.T) {
	r := &AttunePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
			},
		},
	}
	// 6 reverts but capped at 4 -> 2^4 = 16x
	assert.Equal(t, 16*cooldown, r.getEffectiveCooldown(policy))
}

// ---------- LimitRange compatibility ----------

var zeroCurrent = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("0"),
		corev1.ResourceMemory: resource.MustParse("0"),
	},
}

func TestCheckQuotaCompatibility_NoLimitRange(t *testing.T) {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &AttunePolicyReconciler{Client: c}

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.NoError(t, err)
}

func TestCheckQuotaCompatibility_BelowMinimum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "below LimitRange minimum")
}

func TestCheckQuotaCompatibility_AboveMaximum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Max: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds LimitRange maximum")
}

func TestCheckQuotaCompatibility_MemoryBelowMinimum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Min: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory request")
	assert.Contains(t, err.Error(), "below LimitRange minimum")
}

func TestCheckQuotaCompatibility_CPUAboveMaximum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Max: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CPU request")
	assert.Contains(t, err.Error(), "exceeds LimitRange maximum")
}

// ---------- ResourceQuota ----------

func TestCheckQuotaCompatibility_QuotaExceeded(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("3800m"),
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := &AttunePolicyReconciler{Client: c}

	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Increase is 400m, but headroom is only 200m (4000m - 3800m).
	err := r.checkQuotaCompatibility(context.Background(), "default", current, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceed ResourceQuota")
}

func TestCheckQuotaCompatibility_QuotaWithHeadroom(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("2"),
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := &AttunePolicyReconciler{Client: c}

	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Increase is 400m, headroom is 2000m. Should pass.
	err := r.checkQuotaCompatibility(context.Background(), "default", current, target)
	assert.NoError(t, err)
}

func TestCheckQuotaCompatibility_DecreaseAlwaysPasses(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("1"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("1"),
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := &AttunePolicyReconciler{Client: c}

	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Decrease never exceeds quota.
	err := r.checkQuotaCompatibility(context.Background(), "default", current, target)
	assert.NoError(t, err)
}

func TestCheckQuotaCompatibility_MemoryQuotaExceeded(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "mem-quota", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("2Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("1900Mi"),
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := &AttunePolicyReconciler{Client: c}

	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	// Increase is 256Mi, but headroom is only ~148Mi (2Gi - 1900Mi).
	err := r.checkQuotaCompatibility(context.Background(), "default", current, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceed ResourceQuota")
}

func TestCheckQuotaCompatibility_LimitsAboveMaximum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	// Requests are within bounds, but limits exceed the LimitRange max.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("3"),
			corev1.ResourceMemory: resource.MustParse("3Gi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
	assert.Contains(t, err.Error(), "exceeds LimitRange maximum")
}

func TestCheckQuotaCompatibility_MemoryLimitAboveMaximum(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lr).Build()
	r := &AttunePolicyReconciler{Client: c}

	// CPU limit is within bounds, but memory limit exceeds the LimitRange max.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	err := r.checkQuotaCompatibility(context.Background(), "default", zeroCurrent, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory limit")
	assert.Contains(t, err.Error(), "exceeds LimitRange maximum")
}

// ---------- EstimatedMonthlySavings ----------

func TestComputeSavings_EstimatedMonthlySavings(t *testing.T) {
	scheme := testScheme()
	r := &AttunePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
	}
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "test",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("1000m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	assert.NotEmpty(t, savings.EstimatedMonthlySavings)
	// 0.5 cores * $0.031/hr * 730 hrs + 0.5 GiB * $0.004/hr * 730 hrs
	// = $11.315 + $1.46 = $12.78
	assert.Equal(t, "$12.78", savings.EstimatedMonthlySavings)
}

// ---------- Custom CostPricing ----------

func TestComputeSavings_CustomCostPricing(t *testing.T) {
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour:   "0.10",
				MemoryPerGiBHour: "0.01",
			},
		},
	}
	scheme := testScheme()
	r := &AttunePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaults).Build(),
	}
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "test",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("1000m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, defaults)
	// 0.5 cores * $0.10/hr * 730 hrs + 0.5 GiB * $0.01/hr * 730 hrs
	// = $36.50 + $3.65 = $40.15
	assert.Equal(t, "$40.15", savings.EstimatedMonthlySavings)
}

// ---------- setFailedCondition conflict retry ----------

func TestSetFailedCondition_SuccessOnFirstAttempt(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()
	r := &AttunePolicyReconciler{Client: c, Scheme: scheme}

	r.setFailedCondition(context.Background(), policy, attunev1alpha1.ReasonInvalidConfig, "bad config")

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
	assert.Equal(t, "bad config", cond.Message)
}

func TestSetFailedCondition_ConflictRetrySucceeds(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	var updateCalls atomic.Int32
	wrappedClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				call := updateCalls.Add(1)
				if call == 1 {
					return apierrors.NewConflict(schema.GroupResource{Group: "attune.io", Resource: "attunepolicies"}, "test-policy", fmt.Errorf("conflict"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &AttunePolicyReconciler{Client: wrappedClient, Scheme: scheme}
	r.setFailedCondition(context.Background(), policy, attunev1alpha1.ReasonInvalidConfig, "retry test")

	assert.Equal(t, int32(2), updateCalls.Load(), "expected 2 update calls (1 conflict + 1 success)")

	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInvalidConfig, cond.Reason)
}

func TestSetFailedCondition_ExhaustedRetries(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		Build()

	var updateCalls atomic.Int32
	wrappedClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				updateCalls.Add(1)
				return apierrors.NewConflict(schema.GroupResource{Group: "attune.io", Resource: "attunepolicies"}, "test-policy", fmt.Errorf("conflict"))
			},
			Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &AttunePolicyReconciler{Client: wrappedClient, Scheme: scheme}
	r.setFailedCondition(context.Background(), policy, attunev1alpha1.ReasonInvalidConfig, "exhausted test")

	// 3 attempts in the for loop, all return conflict
	assert.Equal(t, int32(3), updateCalls.Load(), "expected 3 update attempts before exhaustion")
}

func TestSetFailedCondition_NonConflictError(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}

	var updateCalls atomic.Int32
	wrappedClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				updateCalls.Add(1)
				return fmt.Errorf("connection refused")
			},
		}).
		Build()

	r := &AttunePolicyReconciler{Client: wrappedClient, Scheme: scheme}
	r.setFailedCondition(context.Background(), policy, attunev1alpha1.ReasonInvalidConfig, "non-conflict test")

	// Non-conflict error should not retry
	assert.Equal(t, int32(1), updateCalls.Load(), "expected exactly 1 update call for non-conflict error")
}

func TestSetFailedCondition_RefetchFailure(t *testing.T) {
	scheme := testScheme()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}

	var updateCalls atomic.Int32
	wrappedClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				updateCalls.Add(1)
				return apierrors.NewConflict(schema.GroupResource{Group: "attune.io", Resource: "attunepolicies"}, "test-policy", fmt.Errorf("conflict"))
			},
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return fmt.Errorf("API server unreachable")
			},
		}).
		Build()

	r := &AttunePolicyReconciler{Client: wrappedClient, Scheme: scheme}
	r.setFailedCondition(context.Background(), policy, attunev1alpha1.ReasonInvalidConfig, "refetch failure test")

	// Conflict on first update, then re-fetch fails -> returns immediately
	assert.Equal(t, int32(1), updateCalls.Load(), "expected 1 update call when re-fetch fails")
}

// ---------- setCooldownStatus ----------

func TestSetCooldownStatus_NoReverts(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}
	r.setCooldownStatus(policy)
	require.NotNil(t, policy.Status.Cooldown)
	assert.Equal(t, 10*time.Minute, policy.Status.Cooldown.EffectiveCooldown.Duration)
	assert.Equal(t, int32(1), policy.Status.Cooldown.BackoffMultiplier)
	assert.Equal(t, int32(0), policy.Status.Cooldown.ConsecutiveReverts)
}

func TestSetCooldownStatus_WithReverts(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: 10 * time.Minute},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
				{Result: attunev1alpha1.ResizeResultReverted},
			},
		},
	}
	r.setCooldownStatus(policy)
	require.NotNil(t, policy.Status.Cooldown)
	// 3 reverts = 2^3 = 8x multiplier
	assert.Equal(t, int32(8), policy.Status.Cooldown.BackoffMultiplier)
	assert.Equal(t, 80*time.Minute, policy.Status.Cooldown.EffectiveCooldown.Duration)
	assert.Equal(t, int32(3), policy.Status.Cooldown.ConsecutiveReverts)
}

func TestSetCooldownStatus_CappedAt16x(t *testing.T) {
	r := &AttunePolicyReconciler{}
	entries := make([]attunev1alpha1.ResizeHistoryEntry, 10)
	for i := range entries {
		entries[i].Result = attunev1alpha1.ResizeResultReverted
	}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: time.Hour},
			},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			ResizeHistory: entries,
		},
	}
	r.setCooldownStatus(policy)
	require.NotNil(t, policy.Status.Cooldown)
	// Capped at 2^4 = 16x
	assert.Equal(t, int32(16), policy.Status.Cooldown.BackoffMultiplier)
	assert.Equal(t, 16*time.Hour, policy.Status.Cooldown.EffectiveCooldown.Duration)
}

// ---------- setScheduleBlockedCondition ----------

func TestSetScheduleBlockedCondition_NoSchedule(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{}
	r.setScheduleBlockedCondition(policy, true)
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionScheduleBlocked)
	assert.Nil(t, cond, "should not set condition when no schedule")
}

func TestSetScheduleBlockedCondition_OutsideWindow(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					Windows: []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
				},
			},
		},
	}
	r.setScheduleBlockedCondition(policy, false)
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionScheduleBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonOutsideWindow, cond.Reason)
}

func TestSetScheduleBlockedCondition_InsideWindow(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					Windows: []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
				},
			},
		},
	}
	r.setScheduleBlockedCondition(policy, true)
	cond := meta.FindStatusCondition(policy.Status.Conditions, attunev1alpha1.ConditionScheduleBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, attunev1alpha1.ReasonInsideWindow, cond.Reason)
}

// ---------- getRateWindow ----------

func TestGetRateWindow_DefaultsFallbackToQueryStep(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				QueryStep: &metav1.Duration{Duration: 15 * time.Minute},
			},
		},
	}
	got := r.getRateWindow(policy)
	assert.Equal(t, 15*time.Minute, got)
}

func TestGetRateWindow_ExplicitValue(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 2 * time.Minute},
			},
		},
	}
	got := r.getRateWindow(policy)
	assert.Equal(t, 2*time.Minute, got)
}

func TestGetRateWindow_ClampedMin(t *testing.T) {
	r := &AttunePolicyReconciler{}
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 5 * time.Second},
			},
		},
	}
	got := r.getRateWindow(policy)
	assert.Equal(t, 30*time.Second, got, "should clamp to 30s minimum")
}
