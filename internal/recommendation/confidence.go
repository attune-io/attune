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

// ConfidenceEstimator widens the recommendation when data confidence is low.
// When confidence is high (near 1.0), the adjustment is small. When confidence
// is low, recommendations are inflated to be conservative, avoiding under-provisioning.
//
// Formula: result = inner * (1 + multiplier / max(confidence, 0.1)) ^ exponent
type ConfidenceEstimator struct {
	// Multiplier controls how much low confidence inflates the result.
	// Default: 1.0.
	Multiplier float64
	// Exponent controls the steepness of the confidence curve.
	// Default: 2.0.
	Exponent float64
	// Inner is the wrapped estimator whose result is adjusted.
	Inner Estimator
}

// Estimate delegates to the inner estimator and then applies the confidence
// adjustment formula. Low confidence values are floored at 0.1 to prevent
// division by zero or extreme inflation.
func (e *ConfidenceEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.Inner.Estimate(profile, current)
	millis := float64(inner.MilliValue())

	multiplier := e.Multiplier
	if multiplier == 0 {
		multiplier = 1.0
	}
	exponent := e.Exponent
	if exponent == 0 {
		exponent = 2.0
	}

	confidence := math.Max(profile.Confidence, 0.1)
	factor := math.Pow(1+multiplier/confidence, exponent)

	adjusted := int64(math.Ceil(millis * factor))
	return *resource.NewMilliQuantity(adjusted, resource.DecimalSI)
}
