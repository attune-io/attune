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

package resize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestClampRequestsToLimits_CPURequestExceedsLimit(t *testing.T) {
	t.Parallel()
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
	}
	clamped := ClampRequestsToLimits(&target)
	assert.Equal(t, []string{"cpu"}, clamped)
	assert.True(t, target.Requests[corev1.ResourceCPU].Equal(resource.MustParse("200m")))
}

func TestClampRequestsToLimits_MemoryRequestExceedsLimit(t *testing.T) {
	t.Parallel()
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	clamped := ClampRequestsToLimits(&target)
	assert.Equal(t, []string{"memory"}, clamped)
	assert.True(t, target.Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
}

func TestClampRequestsToLimits_NoLimitsIsNoOp(t *testing.T) {
	t.Parallel()
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	clamped := ClampRequestsToLimits(&target)
	assert.Nil(t, clamped)
	assert.True(t, target.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
	assert.True(t, target.Requests[corev1.ResourceMemory].Equal(resource.MustParse("512Mi")))
}

func TestClampRequestsToLimits_RequestAtOrBelowLimitIsNoOp(t *testing.T) {
	t.Parallel()
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	clamped := ClampRequestsToLimits(&target)
	assert.Nil(t, clamped)
	assert.True(t, target.Requests[corev1.ResourceCPU].Equal(resource.MustParse("200m")))
	assert.True(t, target.Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
}
