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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"pgregory.net/rapid"
)

func TestResolveAppliedTarget_RequestAtMostLimit(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		curLimMilli := rapid.Int64Range(100, 8000).Draw(rt, "curLim")
		tgtLimMilli := rapid.Int64Range(50, 8000).Draw(rt, "tgtLim")
		tgtReqMilli := rapid.Int64Range(1, tgtLimMilli).Draw(rt, "tgtReq")
		allow := rapid.Bool().Draw(rt, "allowDecrease")
		cur := *resource.NewMilliQuantity(curLimMilli, resource.DecimalSI)
		pod := &corev1.Pod{
			Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: cur},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: cur},
					},
				}},
			},
		}
		target := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: *resource.NewMilliQuantity(tgtReqMilli, resource.DecimalSI)},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: *resource.NewMilliQuantity(tgtLimMilli, resource.DecimalSI)},
		}
		got, _ := ResolveAppliedTarget(ResolveInput{
			Target:                     target,
			Pod:                        pod,
			Container:                  "app",
			AllowInPlaceMemoryDecrease: allow,
		})
		req, rok := got.Requests[corev1.ResourceMemory]
		lim, lok := got.Limits[corev1.ResourceMemory]
		if rok && lok && req.Cmp(lim) > 0 {
			rt.Fatalf("request %s exceeds limit %s", req.String(), lim.String())
		}
	})
}
