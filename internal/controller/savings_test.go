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
