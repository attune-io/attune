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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"pgregory.net/rapid"
)

var extraResourceNames = []corev1.ResourceName{
	"nvidia.com/gpu",
	"hugepages-2Mi",
	corev1.ResourceEphemeralStorage,
}

func genQuantity(t *rapid.T) resource.Quantity {
	n := rapid.Int64Range(1, 1<<30).Draw(t, "bytes")
	return *resource.NewQuantity(n, resource.BinarySI)
}

func genCPUMemPair(t *rapid.T) (req, lim resource.Quantity) {
	reqMilli := rapid.Int64Range(1, 8000).Draw(t, "cpuReqMilli")
	limMilli := rapid.Int64Range(reqMilli, 16000).Draw(t, "cpuLimMilli")
	return *resource.NewMilliQuantity(reqMilli, resource.DecimalSI),
		*resource.NewMilliQuantity(limMilli, resource.DecimalSI)
}

func TestReplaceCPUMemoryResources_PreservesNonCPUMemory(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		cpuReq, cpuLim := genCPUMemPair(rt)
		memReq := genQuantity(rt)
		memLim := genQuantity(rt)
		current := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    cpuReq,
				corev1.ResourceMemory: memReq,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpuLim,
				corev1.ResourceMemory: memLim,
			},
		}
		for _, name := range extraResourceNames {
			if rapid.Bool().Draw(rt, string(name)+"present") {
				q := genQuantity(rt)
				current.Requests[name] = q
				current.Limits[name] = q
			}
		}
		wantCPU, _ := genCPUMemPair(rt)
		wantMem := genQuantity(rt)
		want := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    wantCPU,
				corev1.ResourceMemory: wantMem,
			},
		}
		got := replaceCPUMemoryResources(current, want)
		for _, name := range extraResourceNames {
			_, had := current.Requests[name]
			_, still := got.Requests[name]
			if had != still {
				rt.Fatalf("request %s present=%v after replace (was %v)", name, still, had)
			}
			if had && !got.Requests[name].Equal(current.Requests[name]) {
				was, now := current.Requests[name], got.Requests[name]
				rt.Fatalf("request %s changed from %s to %s", name, was.String(), now.String())
			}
			_, hadL := current.Limits[name]
			_, stillL := got.Limits[name]
			if hadL != stillL {
				rt.Fatalf("limit %s present=%v after replace (was %v)", name, stillL, hadL)
			}
			if hadL && !got.Limits[name].Equal(current.Limits[name]) {
				was, now := current.Limits[name], got.Limits[name]
				rt.Fatalf("limit %s changed from %s to %s", name, was.String(), now.String())
			}
		}
		if _, ok := got.Limits[corev1.ResourceCPU]; ok {
			rt.Fatalf("omitted cpu limit must be deleted")
		}
		if _, ok := got.Limits[corev1.ResourceMemory]; ok {
			rt.Fatalf("omitted memory limit must be deleted")
		}
	})
}
