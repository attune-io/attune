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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SebTardifLabs/kube-rightsize/internal/metrics"
)

// confidenceEstimator widens the recommendation when data confidence is low.
// Used only in unit tests; the production path inlines this logic in
// RecommendWithExplanation.
//
// Formula: result = inner * (1 + multiplier / max(confidence, 0.1)) ^ exponent
type confidenceEstimator struct {
	multiplier float64
	exponent   float64
	inner      estimator
}

// Estimate delegates to the inner estimator and then applies the confidence
// adjustment formula. Low confidence values are floored at 0.1 to prevent
// division by zero or extreme inflation.
func (e *confidenceEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.inner.Estimate(profile, current)

	multiplier := e.multiplier
	if multiplier == 0 {
		multiplier = 1.0
	}
	exponent := e.exponent
	if exponent == 0 {
		exponent = 2.0
	}

	confidence := math.Max(profile.Confidence, 0.1)
	factor := math.Pow(1+multiplier/confidence, exponent)

	return scaleQuantity(inner, factor)
}
