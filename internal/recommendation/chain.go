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

// RecommendationEngine composes a chain of estimators to produce a final
// resource recommendation. The chain is built from:
// percentile -> margin -> confidence -> bounds -> change_filter.
type RecommendationEngine struct {
	chain Estimator
}

// NewEngine creates a new RecommendationEngine with the specified parameters.
// The estimator chain is: PercentileEstimator -> MarginEstimator ->
// ConfidenceEstimator -> BoundsEstimator -> ChangeFilter.
func NewEngine(percentile int, safetyMargin float64, minBound, maxBound resource.Quantity,
	maxChangePercent float64, isCPU ...bool,
) *RecommendationEngine {
	// Build the chain from innermost to outermost.
	cpu := len(isCPU) > 0 && isCPU[0]
	base := &PercentileEstimator{Percentile: percentile, IsCPU: cpu}

	margin := &MarginEstimator{
		Factor: safetyMargin,
		Inner:  base,
	}

	confidence := &ConfidenceEstimator{
		Multiplier: 1.0,
		Exponent:   2.0,
		Inner:      margin,
	}

	bounds := &BoundsEstimator{
		Min:   minBound,
		Max:   maxBound,
		Inner: confidence,
	}

	filter := &ChangeFilter{
		MinChangePercent: 10,
		MaxChangePercent: maxChangePercent,
		Inner:            bounds,
	}

	return &RecommendationEngine{chain: filter}
}

// Recommend produces a resource recommendation for the given usage profile
// and current allocation. It returns the recommended quantity and whether
// the recommendation differs from the current value.
func (e *RecommendationEngine) Recommend(profile metrics.UsageProfile, current resource.Quantity) (recommended resource.Quantity, changed bool) {
	recommended = e.chain.Estimate(profile, current)
	changed = recommended.Cmp(current) != 0
	return recommended, changed
}
