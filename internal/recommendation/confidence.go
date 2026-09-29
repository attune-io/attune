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

	"github.com/attune-io/attune/internal/metrics"
)

// confidenceEstimator widens the recommendation when data confidence is low.
// Used by the benchmark and its unit test. RecommendWithExplanation inlines
// the same factor.
//
// factor = 1 + multiplier * (1 - confidence) ^ exponent
type confidenceEstimator struct {
	multiplier float64
	exponent   float64
	inner      estimator
}

// Estimate delegates to the inner estimator and then applies the confidence
// factor. Confidence is clamped to [0, 1]. A zero multiplier or exponent
// uses 1 and 2 on this helper only. RecommendWithExplanation skips the
// factor when either value is 0.
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

	confidence := profile.Confidence
	if confidence > 1 {
		confidence = 1
	}
	if confidence < 0 {
		confidence = 0
	}
	factor := 1 + multiplier*math.Pow(1-confidence, exponent)

	return scaleQuantity(inner, factor)
}
