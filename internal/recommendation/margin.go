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

// marginEstimator wraps another estimator and multiplies the result by a
// safety factor to provide headroom above the estimated usage. Used only in
// unit tests; the production path inlines this logic in RecommendWithExplanation.
type marginEstimator struct {
	factor float64
	inner  estimator
}

// Estimate delegates to the inner estimator and multiplies the result by
// the configured safety factor.
func (e *marginEstimator) Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity {
	inner := e.inner.Estimate(profile, current)

	return scaleQuantity(inner, e.factor)
}
