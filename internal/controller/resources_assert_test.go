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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// resourceListMap is every key, not a cpu/memory subset. #699: stop
// choosing which resource fields to assert on.
func resourceListMap(rl corev1.ResourceList) map[string]string {
	out := map[string]string{}
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func assertFullResources(t *testing.T, got, want corev1.ResourceRequirements, msg string) {
	t.Helper()
	assert.Equal(t, resourceListMap(want.Requests), resourceListMap(got.Requests), msg+" requests")
	assert.Equal(t, resourceListMap(want.Limits), resourceListMap(got.Limits), msg+" limits")
}

func TestReplaceCPUMemoryResources_KeepsEveryNonCPUMemoryKey(t *testing.T) {
	t.Parallel()
	gpu := corev1.ResourceName("nvidia.com/gpu")
	huge := corev1.ResourceName("hugepages-2Mi")
	eph := corev1.ResourceEphemeralStorage
	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
			gpu:                   resource.MustParse("1"),
			huge:                  resource.MustParse("4Mi"),
			eph:                   resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
			gpu:                   resource.MustParse("1"),
			huge:                  resource.MustParse("4Mi"),
			eph:                   resource.MustParse("1Gi"),
		},
	}
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	got := replaceCPUMemoryResources(current, want)
	assertFullResources(t, got, corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
			gpu:                   resource.MustParse("1"),
			huge:                  resource.MustParse("4Mi"),
			eph:                   resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			gpu:  resource.MustParse("1"),
			huge: resource.MustParse("4Mi"),
			eph:  resource.MustParse("1Gi"),
		},
	}, "replace must keep every non-cpu/memory key and drop omitted cpu/memory limits")
}
