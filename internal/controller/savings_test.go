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

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func newSavingsReconciler() *AttunePolicyReconciler {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	return r
}

func TestComputeSavings_Empty(t *testing.T) {
	r := newSavingsReconciler()
	savings, _ := r.computeSavings(nil, nil)
	assert.Empty(t, savings.CPURequestReduction)
	assert.Empty(t, savings.MemoryRequestReduction)
}

func TestComputeSavings_CPUReduction(t *testing.T) {
	r := newSavingsReconciler()
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("150m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	// 500m - 150m = 350m saved.
	assert.Equal(t, "350m", savings.CPURequestReduction)
	// Memory unchanged.
	assert.Empty(t, savings.MemoryRequestReduction)
	// EstimatedMonthlySavings: 0.35 cores * $0.031/hr * 730 hrs = $7.92
	assert.Equal(t, "$7.92", savings.EstimatedMonthlySavings)
}

func TestComputeSavings_MemoryReduction(t *testing.T) {
	r := newSavingsReconciler()
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("280Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	// CPU unchanged.
	assert.Empty(t, savings.CPURequestReduction)
	// 512Mi - 280Mi = 232Mi saved.
	assert.Equal(t, "232Mi", savings.MemoryRequestReduction)
}

func TestComputeSavings_MultipleWorkloads(t *testing.T) {
	r := newSavingsReconciler()
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("300m"),
						MemoryRequest: resource.MustParse("384Mi"),
					},
				},
			},
		},
		{
			Workload: "worker",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("1000m"),
						MemoryRequest: resource.MustParse("1Gi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("900m"),
						MemoryRequest: resource.MustParse("768Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	// CPU: (500-300) + (1000-900) = 200 + 100 = 300m.
	assert.Equal(t, "300m", savings.CPURequestReduction)
	// Memory: (512-384)Mi + (1024-768)Mi = 128Mi + 256Mi = 384Mi.
	assert.Equal(t, "384Mi", savings.MemoryRequestReduction)
}

func TestComputeSavings_NoReduction(t *testing.T) {
	r := newSavingsReconciler()
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "main",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("200m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("300m"),
						MemoryRequest: resource.MustParse("512Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	// Recommended >= current for both resources: no savings.
	assert.Empty(t, savings.CPURequestReduction)
	assert.Empty(t, savings.MemoryRequestReduction)

	// All containers are under-provisioned: increase fields should be set.
	assert.Equal(t, "100m", savings.CPURequestIncrease)
	assert.Equal(t, "256Mi", savings.MemoryRequestIncrease)
	assert.Equal(t, "$2.99", savings.EstimatedMonthlyCostIncrease)
}

func TestComputeSavings_Mixed(t *testing.T) {
	r := newSavingsReconciler()
	recommendations := []attunev1alpha1.WorkloadRecommendation{
		{
			Workload: "api-server",
			Kind:     "Deployment",
			Containers: []attunev1alpha1.ContainerRecommendation{
				{
					Name: "reduce-cpu",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("500m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("150m"),
						MemoryRequest: resource.MustParse("256Mi"),
					},
				},
				{
					Name: "increase-cpu",
					Current: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("100m"),
						MemoryRequest: resource.MustParse("128Mi"),
					},
					Recommended: attunev1alpha1.ResourceValues{
						CPURequest:    resource.MustParse("300m"),
						MemoryRequest: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	savings, _ := r.computeSavings(recommendations, nil)
	// Only the first container saves CPU: 500m - 150m = 350m.
	// The second container increases CPU, which is not counted as savings.
	assert.Equal(t, "350m", savings.CPURequestReduction)
	// No memory savings.
	assert.Empty(t, savings.MemoryRequestReduction)

	// The second container increases CPU: 300m - 100m = 200m.
	assert.Equal(t, "200m", savings.CPURequestIncrease)
	// No memory increase.
	assert.Empty(t, savings.MemoryRequestIncrease)
	// Cost increase: 0.2 cores * $0.031/hr * 730 hrs = $4.53
	assert.Equal(t, "$4.53", savings.EstimatedMonthlyCostIncrease)
}

// --- Defense-in-depth parsing helpers ---

func TestParseFloat64_ValidValue(t *testing.T) {
	assert.Equal(t, 3.5, parseFloat64("3.5", 1.0))
}

func TestParseFloat64_EmptyReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("", 1.0))
}

func TestParseFloat64_NaNReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("NaN", 1.0))
}

func TestParseFloat64_InfReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("Inf", 1.0))
}

func TestParseFloat64_NegativeInfReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("-Inf", 1.0))
}

func TestParseFloat64_NegativeReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("-0.5", 1.0))
}

func TestParseFloat64_ZeroReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("0", 1.0))
}

func TestParseFloat64_ExceedsMaxReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("11.0", 1.0))
}

func TestParseFloat64_BoundaryMaxAccepted(t *testing.T) {
	assert.Equal(t, 10.0, parseFloat64("10.0", 1.0))
}

func TestParseFloat64_InvalidStringReturnsFallback(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64("abc", 1.0))
}

func TestParseFloat64Ratio_ValidValue(t *testing.T) {
	assert.Equal(t, 2.5, parseFloat64Ratio("2.5"))
}

func TestParseFloat64Ratio_EmptyReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64Ratio(""))
}

func TestParseFloat64Ratio_NaNReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64Ratio("NaN"))
}

func TestParseFloat64Ratio_InfReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64Ratio("Inf"))
}

func TestParseFloat64Ratio_NegativeReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64Ratio("-1.0"))
}

func TestParseFloat64Ratio_ExceedsMaxReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64Ratio("1001"))
}

func TestParseFloat64Ratio_BoundaryMaxAccepted(t *testing.T) {
	assert.Equal(t, 1000.0, parseFloat64Ratio("1000"))
}

func TestParseOverheadPercent_ValidValue(t *testing.T) {
	assert.Equal(t, 20.0, parseOverheadPercent("20", 15.0))
}

func TestParseOverheadPercent_EmptyReturnsFallback(t *testing.T) {
	assert.Equal(t, 15.0, parseOverheadPercent("", 15.0))
}

func TestParseOverheadPercent_NaNReturnsFallback(t *testing.T) {
	assert.Equal(t, 15.0, parseOverheadPercent("NaN", 15.0))
}

func TestParseOverheadPercent_InfReturnsFallback(t *testing.T) {
	assert.Equal(t, 15.0, parseOverheadPercent("Inf", 15.0))
}

func TestParseOverheadPercent_NegativeReturnsFallback(t *testing.T) {
	assert.Equal(t, 15.0, parseOverheadPercent("-5", 15.0))
}

func TestParseOverheadPercent_ZeroIsValid(t *testing.T) {
	assert.Equal(t, 0.0, parseOverheadPercent("0", 15.0))
}

func TestParseOverheadPercent_ExceedsMaxReturnsFallback(t *testing.T) {
	assert.Equal(t, 15.0, parseOverheadPercent("901", 15.0))
}

func TestParseFloat64NonNeg_ValidValue(t *testing.T) {
	assert.Equal(t, 0.5, parseFloat64NonNeg("0.5", 0.1))
}

func TestParseFloat64NonNeg_EmptyReturnsFallback(t *testing.T) {
	assert.Equal(t, 0.1, parseFloat64NonNeg("", 0.1))
}

func TestParseFloat64NonNeg_NaNReturnsFallback(t *testing.T) {
	assert.Equal(t, 0.1, parseFloat64NonNeg("NaN", 0.1))
}

func TestParseFloat64NonNeg_InfReturnsFallback(t *testing.T) {
	assert.Equal(t, 0.1, parseFloat64NonNeg("Inf", 0.1))
}

func TestParseFloat64NonNeg_NegativeReturnsFallback(t *testing.T) {
	assert.Equal(t, 0.1, parseFloat64NonNeg("-0.5", 0.1))
}

func TestParseFloat64NonNeg_ZeroIsValid(t *testing.T) {
	assert.Equal(t, 0.0, parseFloat64NonNeg("0", 0.1))
}

func TestParseFloat64NonNeg_ExceedsOneCapsAtOne(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64NonNeg("5.0", 0.1))
}

func TestParseFloat64NonNeg_ExactlyOneIsValid(t *testing.T) {
	assert.Equal(t, 1.0, parseFloat64NonNeg("1.0", 0.1))
}

func TestScaleLimits_NormalCase(t *testing.T) {
	// Current limit 1000m with request 500m gives ratio 2.0.
	// New request 250m * 2.0 = 500m new limit.
	result := scaleLimits(
		resource.MustParse("500m"),
		resource.MustParse("1000m"),
		resource.MustParse("250m"),
	)
	expected := resource.MustParse("500m")
	assert.True(t, result.Cmp(expected) == 0, "expected %s, got %s", expected.String(), result.String())
}

func TestScaleLimits_ZeroRequestReturnsZero(t *testing.T) {
	result := scaleLimits(
		resource.MustParse("0"),
		resource.MustParse("1000m"),
		resource.MustParse("250m"),
	)
	assert.True(t, result.IsZero())
}

func TestScaleLimits_ZeroLimitReturnsZero(t *testing.T) {
	result := scaleLimits(
		resource.MustParse("500m"),
		resource.MustParse("0"),
		resource.MustParse("250m"),
	)
	assert.True(t, result.IsZero())
}

func TestScaleLimits_EqualRequestAndLimit(t *testing.T) {
	// Ratio is 1.0 so new limit equals new request.
	result := scaleLimits(
		resource.MustParse("500m"),
		resource.MustParse("500m"),
		resource.MustParse("300m"),
	)
	expected := resource.MustParse("300m")
	assert.True(t, result.Cmp(expected) == 0, "expected %s, got %s", expected.String(), result.String())
}
