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
	"math"

	"github.com/SebTardif/kube-rightsize/internal/metrics"
	"k8s.io/apimachinery/pkg/api/resource"
)

// MarginEstimator wraps another estimator and multiplies the result by a
// safety factor to provide headroom above the estimated usage.
type MarginEstimator struct {
	// Factor is the safety multiplier (e.g. 1.2 for 20% headroom).
	Factor float64
	// Inner is the wrapped estimator whose result is scaled.
	Inner Estimator
}

// Estimate delegates to the inner estimator and multiplies the result by
// the configured safety factor.
func (e *MarginEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.Inner.Estimate(profile, current)
	millis := inner.MilliValue()

	adjusted := int64(math.Ceil(float64(millis) * e.Factor))
	return *resource.NewMilliQuantity(adjusted, resource.DecimalSI)
}
