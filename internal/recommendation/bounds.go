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

	"github.com/SebTardif/kube-rightsize/internal/metrics"
)

// BoundsEstimator clamps the result from the inner estimator to
// user-defined minimum and maximum values. Values below Min are raised
// to Min; values above Max are lowered to Max.
type BoundsEstimator struct {
	// Min is the floor for recommended values.
	Min resource.Quantity
	// Max is the ceiling for recommended values.
	Max resource.Quantity
	// Inner is the wrapped estimator whose result is clamped.
	Inner Estimator
}

// Estimate delegates to the inner estimator and clamps the result to the
// configured [Min, Max] range.
func (e *BoundsEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.Inner.Estimate(profile, current)

	if inner.Cmp(e.Min) < 0 {
		return e.Min.DeepCopy()
	}
	if inner.Cmp(e.Max) > 0 {
		return e.Max.DeepCopy()
	}

	return inner
}
