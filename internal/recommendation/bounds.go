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

package recommendation

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/attune-io/attune/internal/metrics"
)

// boundsEstimator clamps the result from the inner estimator to
// user-defined minimum and maximum values. Values below min are raised
// to min; values above max are lowered to max. Used only in unit tests;
// production calls applyBounds from RecommendWithExplanation.
type boundsEstimator struct {
	min   resource.Quantity
	max   resource.Quantity
	inner estimator
}

// Estimate delegates to the inner estimator and clamps the result to the
// configured [Min, Max] range.
func (e *boundsEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.inner.Estimate(profile, current)
	clamped, _ := applyBounds(inner, e.min, e.max)
	return clamped
}

// applyBounds clamps q into [min, max]. The second return is "min", "max",
// or empty when the value was already inside the range.
func applyBounds(q, min, max resource.Quantity) (resource.Quantity, string) {
	if q.Cmp(min) < 0 {
		return min.DeepCopy(), "min"
	}
	if q.Cmp(max) > 0 {
		return max.DeepCopy(), "max"
	}
	return q, ""
}

func validCRDPercentile(p int) bool {
	switch p {
	case 0, 50, 90, 95, 99:
		return true
	default:
		return false
	}
}
