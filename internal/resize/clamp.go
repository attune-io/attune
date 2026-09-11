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
	corev1 "k8s.io/api/core/v1"
)

// ClampRequestsToLimits ensures requests do not exceed limits for each resource.
// When a limit is present and the request exceeds it, the request is capped
// at the limit value to prevent API server rejection.
func ClampRequestsToLimits(target *corev1.ResourceRequirements) []string {
	if target.Limits == nil {
		return nil
	}
	var clamped []string
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		lim, hasLim := target.Limits[res]
		req, hasReq := target.Requests[res]
		if hasLim && hasReq && req.Cmp(lim) > 0 {
			target.Requests[res] = lim.DeepCopy()
			clamped = append(clamped, string(res))
		}
	}
	return clamped
}
