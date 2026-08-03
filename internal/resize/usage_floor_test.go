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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestFloorMemoryLimitForUsage(t *testing.T) {
	t.Parallel()
	must := resource.MustParse
	targetWith := func(lim, req string) corev1.ResourceRequirements {
		rr := corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: must(lim)},
		}
		if req != "" {
			rr.Requests = corev1.ResourceList{corev1.ResourceMemory: must(req)}
		}
		return rr
	}

	tests := []struct {
		name         string
		target       corev1.ResourceRequirements
		currentLimit string
		usage        string
		margin       float64
		wantFloored  bool
		wantLimit    string
		wantRequest  string // empty = leave unset
	}{
		{
			name:         "safe decrease above usage+margin",
			target:       targetWith("800Mi", "800Mi"),
			currentLimit: "1Gi",
			usage:        "400Mi",
			margin:       10,
			wantFloored:  false,
			wantLimit:    "800Mi",
		},
		{
			name:         "unsafe decrease floored to usage+10%",
			target:       targetWith("200Mi", "200Mi"),
			currentLimit: "1Gi",
			usage:        "500Mi",
			margin:       10,
			wantFloored:  true,
			// 500Mi * 1.1 = 550Mi
			wantLimit:   "550Mi",
			wantRequest: "200Mi", // request left alone if still under floor... wait request 200 < 550, OK
		},
		{
			name:         "at or below usage floored",
			target:       targetWith("400Mi", ""),
			currentLimit: "1Gi",
			usage:        "500Mi",
			margin:       0,
			wantFloored:  true,
			wantLimit:    "500Mi", // margin 0: usage+1 byte, but BinarySI may show 500Mi+1
		},
		{
			name:         "increase not floored",
			target:       targetWith("2Gi", ""),
			currentLimit: "1Gi",
			usage:        "500Mi",
			margin:       10,
			wantFloored:  false,
			wantLimit:    "2Gi",
		},
		{
			name:         "zero usage no-op",
			target:       targetWith("200Mi", ""),
			currentLimit: "1Gi",
			usage:        "0",
			margin:       10,
			wantFloored:  false,
			wantLimit:    "200Mi",
		},
		{
			name:         "no limit on target",
			target:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: must("200Mi")}},
			currentLimit: "1Gi",
			usage:        "500Mi",
			margin:       10,
			wantFloored:  false,
		},
		{
			name:         "floor never exceeds current limit",
			target:       targetWith("100Mi", "100Mi"),
			currentLimit: "200Mi",
			usage:        "300Mi", // usage already above current limit (pathological)
			margin:       50,
			wantFloored:  true,
			wantLimit:    "200Mi", // clamped to current
			wantRequest:  "100Mi",
		},
		{
			name:         "request clamped down when above floored limit",
			target:       targetWith("100Mi", "900Mi"),
			currentLimit: "1Gi",
			usage:        "500Mi",
			margin:       10,
			wantFloored:  true,
			wantLimit:    "550Mi",
			wantRequest:  "550Mi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, floored := FloorMemoryLimitForUsage(tt.target, must(tt.currentLimit), must(tt.usage), tt.margin)
			assert.Equal(t, tt.wantFloored, floored)
			if tt.wantLimit != "" {
				require.Contains(t, got.Limits, corev1.ResourceMemory)
				// Compare byte values to avoid BinarySI formatting quirks for +1 byte cases.
				if tt.name == "at or below usage floored" {
					u := must(tt.usage)
					cur := must(tt.currentLimit)
					assert.Greater(t, got.Limits.Memory().Value(), u.Value())
					assert.LessOrEqual(t, got.Limits.Memory().Value(), cur.Value())
				} else {
					assert.True(t, got.Limits.Memory().Equal(must(tt.wantLimit)),
						"got limit %s want %s", got.Limits.Memory().String(), tt.wantLimit)
				}
			}
			if tt.wantRequest != "" {
				assert.True(t, got.Requests.Memory().Equal(must(tt.wantRequest)),
					"got request %s want %s", got.Requests.Memory().String(), tt.wantRequest)
			}
		})
	}
}

func TestFloorMemoryLimitForUsage_NaNMargin(t *testing.T) {
	t.Parallel()
	target := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
	}
	usage := resource.MustParse("200Mi")
	got, floored := FloorMemoryLimitForUsage(target, resource.MustParse("1Gi"), usage, math.NaN())
	assert.True(t, floored)
	// margin treated as 0 → limit strictly above usage
	assert.Greater(t, got.Limits.Memory().Value(), usage.Value())
}
