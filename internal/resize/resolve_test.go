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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func guaranteedMemPod(name, currentLim string) *corev1.Pod {
	lim := resource.MustParse(currentLim)
	return &corev1.Pod{
		Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: name,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: lim.DeepCopy()},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: lim.DeepCopy()},
				},
			}},
		},
	}
}

func memPair(req, lim string) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req)},
	}
	if lim != "" {
		out.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)}
	}
	return out
}

func TestResolveAppliedTarget_PlatformClampRaisesGuaranteed(t *testing.T) {
	t.Parallel()
	pod := guaranteedMemPod("app", "1Gi")
	got, meta := ResolveAppliedTarget(ResolveInput{
		Target:                     memPair("64Mi", "64Mi"),
		Pod:                        pod,
		Container:                  "app",
		AllowInPlaceMemoryDecrease: false,
		ApplyUsageFloor:            true,
		CurrentMemoryLimit:         resource.MustParse("1Gi"),
		RecentUsage:                resource.MustParse("300Mi"),
	})
	require.True(t, meta.PlatformClamped, "NotRequired must clamp 64Mi up to live 1Gi")
	assert.False(t, meta.FloorApplied, "platform clamp skips the usage floor")
	assert.True(t, meta.GuaranteedRequestRaised)
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("1Gi")), "got limit %s", got.Limits.Memory())
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("1Gi")), "got request %s", got.Requests.Memory())
}

func TestResolveAppliedTarget_UsageFloorThenGuaranteedRaise(t *testing.T) {
	t.Parallel()
	pod := guaranteedMemPod("app", "1Gi")
	usage := resource.MustParse("300Mi")
	got, meta := ResolveAppliedTarget(ResolveInput{
		Target:                     memPair("64Mi", "64Mi"),
		Pod:                        pod,
		Container:                  "app",
		AllowInPlaceMemoryDecrease: true,
		ApplyUsageFloor:            true,
		CurrentMemoryLimit:         resource.MustParse("1Gi"),
		RecentUsage:                usage,
		UsageMarginPercent:         0,
	})
	assert.False(t, meta.PlatformClamped)
	require.True(t, meta.FloorApplied)
	assert.Equal(t, usage.Value()+1, got.Limits.Memory().Value(), "margin 0 is usage+1")
	assert.Equal(t, got.Limits.Memory().Value(), got.Requests.Memory().Value(),
		"Guaranteed request must match the floored limit")
}

func TestResolveAppliedTarget_RevertSkipsFloor(t *testing.T) {
	t.Parallel()
	pod := guaranteedMemPod("app", "1Gi")
	got, meta := ResolveAppliedTarget(ResolveInput{
		Target:                     memPair("256Mi", "256Mi"),
		Pod:                        pod,
		Container:                  "app",
		AllowInPlaceMemoryDecrease: false,
	})
	assert.True(t, meta.PlatformClamped, "revert still clamps a lower original limit")
	assert.False(t, meta.FloorApplied)
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("1Gi")))
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("1Gi")))
}

func TestResolveAppliedTarget_RevertDoesNotFloorWhenClampIsOff(t *testing.T) {
	t.Parallel()
	// allowInPlace=true so clamp is a no-op. Usage would raise 256Mi if
	// the floor ran. Revert must leave the original pair.
	pod := guaranteedMemPod("app", "1Gi")
	got, meta := ResolveAppliedTarget(ResolveInput{
		Target:                     memPair("256Mi", "256Mi"),
		Pod:                        pod,
		Container:                  "app",
		AllowInPlaceMemoryDecrease: true,
		ApplyUsageFloor:            false,
		CurrentMemoryLimit:         resource.MustParse("1Gi"),
		RecentUsage:                resource.MustParse("400Mi"),
		UsageMarginPercent:         0,
	})
	assert.False(t, meta.PlatformClamped)
	assert.False(t, meta.FloorApplied)
	assert.True(t, got.Limits.Memory().Equal(resource.MustParse("256Mi")), "got limit %s", got.Limits.Memory())
	assert.True(t, got.Requests.Memory().Equal(resource.MustParse("256Mi")), "got request %s", got.Requests.Memory())
}
