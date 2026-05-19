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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

// ---------- Resizing condition ----------

func TestSetResizingCondition_InProgress(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{Mode: rightsizev1alpha1.UpdateModeAuto},
		},
		Status: rightsizev1alpha1.RightSizePolicyStatus{
			Workloads: rightsizev1alpha1.WorkloadStatus{Resized: 2},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonInProgress, cond.Reason)
}

func TestSetResizingCondition_CooldownActive(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{Mode: rightsizev1alpha1.UpdateModeAuto},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setResizingCondition(policy, true)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonCooldownActive, cond.Reason)
}

func TestSetResizingCondition_Idle(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{Mode: rightsizev1alpha1.UpdateModeAuto},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionResizing)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonIdle, cond.Reason)
}

func TestSetResizingCondition_ObserveMode_NoCondition(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{Mode: rightsizev1alpha1.UpdateModeObserve},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setResizingCondition(policy, false)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionResizing)
	assert.Nil(t, cond)
}

// ---------- Degraded condition ----------

func TestSetDegradedCondition_HighRevertRate(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Status: rightsizev1alpha1.RightSizePolicyStatus{
			ResizeHistory: []rightsizev1alpha1.ResizeHistoryEntry{
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultSuccess},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
			},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, rightsizev1alpha1.ReasonHighRevertRate, cond.Reason)
}

func TestSetDegradedCondition_LowRevertRate(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{
		Status: rightsizev1alpha1.RightSizePolicyStatus{
			ResizeHistory: []rightsizev1alpha1.ResizeHistoryEntry{
				{Result: rightsizev1alpha1.ResizeResultSuccess},
				{Result: rightsizev1alpha1.ResizeResultSuccess},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultSuccess},
				{Result: rightsizev1alpha1.ResizeResultSuccess},
			},
		},
	}
	r := &RightSizePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionDegraded)
	assert.Nil(t, cond)
}

func TestSetDegradedCondition_EmptyHistory(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{}
	r := &RightSizePolicyReconciler{}
	r.setDegradedCondition(policy)

	cond := meta.FindStatusCondition(policy.Status.Conditions, rightsizev1alpha1.ConditionDegraded)
	assert.Nil(t, cond)
}

// ---------- Consecutive reverts ----------

func TestConsecutiveReverts(t *testing.T) {
	tests := []struct {
		name    string
		history []rightsizev1alpha1.ResizeHistoryEntry
		want    int
	}{
		{"empty", nil, 0},
		{"no reverts", []rightsizev1alpha1.ResizeHistoryEntry{{Result: rightsizev1alpha1.ResizeResultSuccess}}, 0},
		{"one revert", []rightsizev1alpha1.ResizeHistoryEntry{{Result: rightsizev1alpha1.ResizeResultReverted}}, 1},
		{"three consecutive", []rightsizev1alpha1.ResizeHistoryEntry{
			{Result: rightsizev1alpha1.ResizeResultSuccess}, {Result: rightsizev1alpha1.ResizeResultReverted}, {Result: rightsizev1alpha1.ResizeResultReverted}, {Result: rightsizev1alpha1.ResizeResultReverted},
		}, 3},
		{"broken by success", []rightsizev1alpha1.ResizeHistoryEntry{
			{Result: rightsizev1alpha1.ResizeResultReverted}, {Result: rightsizev1alpha1.ResizeResultSuccess}, {Result: rightsizev1alpha1.ResizeResultReverted},
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
	r := &RightSizePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
	}
	assert.Equal(t, cooldown, r.getEffectiveCooldown(policy))
}

func TestGetEffectiveCooldown_TwoReverts(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
		Status: rightsizev1alpha1.RightSizePolicyStatus{
			ResizeHistory: []rightsizev1alpha1.ResizeHistoryEntry{
				{Result: rightsizev1alpha1.ResizeResultReverted}, {Result: rightsizev1alpha1.ResizeResultReverted},
			},
		},
	}
	// 2 reverts -> 2^2 = 4x base cooldown
	assert.Equal(t, 4*cooldown, r.getEffectiveCooldown(policy))
}

func TestGetEffectiveCooldown_CappedAt16x(t *testing.T) {
	r := &RightSizePolicyReconciler{}
	cooldown := 1 * time.Hour
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Cooldown: &metav1.Duration{Duration: cooldown},
			},
		},
		Status: rightsizev1alpha1.RightSizePolicyStatus{
			ResizeHistory: []rightsizev1alpha1.ResizeHistoryEntry{
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
				{Result: rightsizev1alpha1.ResizeResultReverted},
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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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
	r := &RightSizePolicyReconciler{Client: c}

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

// ---------- EstimatedMonthlySavings ----------

func TestComputeSavings_EstimatedMonthlySavings(t *testing.T) {
	scheme := testScheme()
	r := &RightSizePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
	}
	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "test",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: rightsizev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("1000m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings := r.computeSavings("default", recommendations, nil)
	assert.NotEmpty(t, savings.EstimatedMonthlySavings)
	// 0.5 cores * $0.031/hr * 730 hrs + 0.5 GiB * $0.004/hr * 730 hrs
	// = $11.315 + $1.46 = $12.78
	assert.Equal(t, "$12.78", savings.EstimatedMonthlySavings)
}

// ---------- Custom CostPricing ----------

func TestComputeSavings_CustomCostPricing(t *testing.T) {
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
				CPUPerCoreHour:   "0.10",
				MemoryPerGiBHour: "0.01",
			},
		},
	}
	scheme := testScheme()
	r := &RightSizePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaults).Build(),
	}
	recommendations := []rightsizev1alpha1.WorkloadRecommendation{
		{
			Workload: "test",
			Kind:     "Deployment",
			Containers: []rightsizev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: rightsizev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("1000m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: rightsizev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings := r.computeSavings("default", recommendations, defaults)
	// 0.5 cores * $0.10/hr * 730 hrs + 0.5 GiB * $0.01/hr * 730 hrs
	// = $36.50 + $3.65 = $40.15
	assert.Equal(t, "$40.15", savings.EstimatedMonthlySavings)
}
