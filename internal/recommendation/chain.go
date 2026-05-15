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

// RecommendationEngine produces resource recommendations by applying a
// pipeline: percentile -> margin -> confidence -> bounds -> change_filter.
// Each step is configured via the fields below and executed inline in
// RecommendWithExplanation.
type RecommendationEngine struct {
	percentile           int
	safetyMargin         float64
	minBound             resource.Quantity
	maxBound             resource.Quantity
	minChangePercent     float64
	maxChangePercent     float64
	confidenceMultiplier float64
	confidenceExponent   float64
	isCPU                bool
}

// NewEngine creates a new RecommendationEngine with the specified parameters.
func NewEngine(percentile int, safetyMargin float64, minBound, maxBound resource.Quantity,
	maxChangePercent float64, isCPU ...bool,
) *RecommendationEngine {
	cpu := len(isCPU) > 0 && isCPU[0]
	return &RecommendationEngine{
		percentile:           percentile,
		safetyMargin:         safetyMargin,
		minBound:             minBound.DeepCopy(),
		maxBound:             maxBound.DeepCopy(),
		minChangePercent:     10.0,
		maxChangePercent:     maxChangePercent,
		confidenceMultiplier: 1.0,
		confidenceExponent:   2.0,
		isCPU:                cpu,
	}
}

// Recommend produces a resource recommendation for the given usage profile
// and current allocation. It returns the recommended quantity and whether
// the recommendation differs from the current value.
func (e *RecommendationEngine) Recommend(profile metrics.UsageProfile, current resource.Quantity) (recommended resource.Quantity, changed bool) {
	recommended, _, changed = e.RecommendWithExplanation(profile, current)
	return recommended, changed
}

// RecommendWithExplanation produces a resource recommendation and returns the
// estimator-chain intermediate values that led to it.
func (e *RecommendationEngine) RecommendWithExplanation(profile metrics.UsageProfile, current resource.Quantity) (recommended resource.Quantity, explanation RecommendationExplanation, changed bool) {
	percentileEstimator := &PercentileEstimator{Percentile: e.percentile, IsCPU: e.isCPU}
	rawPercentile := percentileEstimator.Estimate(profile, current)

	afterSafetyMargin := scaleQuantity(rawPercentile, e.safetyMargin)

	// Burst-aware boost: if the profile detected a burst (max > 3x p95),
	// widen the safety margin proportionally using a logarithmic scale
	// so extreme bursts don't inflate the recommendation excessively.
	burstFactor := 1.0
	if profile.BurstDetected && profile.BurstMagnitude > 1 {
		burstFactor = 1.0 + math.Log2(profile.BurstMagnitude)*0.1
	}
	afterBurst := scaleQuantity(afterSafetyMargin, burstFactor)

	confidence := profile.Confidence
	if confidence < 0.1 {
		confidence = 0.1
	}
	confidenceFactor := 1.0
	if e.confidenceMultiplier != 0 && e.confidenceExponent != 0 {
		confidenceFactor = math.Pow(1+e.confidenceMultiplier/confidence, e.confidenceExponent)
	}
	afterConfidence := scaleQuantity(afterBurst, confidenceFactor)

	afterBounds := afterConfidence.DeepCopy()
	boundsApplied := ""
	if afterBounds.Cmp(e.minBound) < 0 {
		afterBounds = e.minBound.DeepCopy()
		boundsApplied = "min"
	} else if afterBounds.Cmp(e.maxBound) > 0 {
		afterBounds = e.maxBound.DeepCopy()
		boundsApplied = "max"
	}

	afterChangeFilter := afterBounds.DeepCopy()
	changeFilterApplied := ""
	currentMillis := float64(current.MilliValue())
	if currentMillis != 0 {
		afterBoundsMillis := float64(afterBounds.MilliValue())
		changePct := math.Abs(afterBoundsMillis-currentMillis) / currentMillis * 100
		if changePct < e.minChangePercent {
			afterChangeFilter = current.DeepCopy()
			changeFilterApplied = "min_change_filtered"
		} else if changePct > e.maxChangePercent {
			maxDelta := currentMillis * e.maxChangePercent / 100
			capped := currentMillis - maxDelta
			if afterBoundsMillis > currentMillis {
				capped = currentMillis + maxDelta
			}
			if afterBounds.Format == resource.BinarySI {
				afterChangeFilter = *resource.NewQuantity(int64(math.Ceil(capped/1000)), resource.BinarySI)
			} else {
				afterChangeFilter = *resource.NewMilliQuantity(int64(math.Ceil(capped)), resource.DecimalSI)
			}
			changeFilterApplied = "max_change_capped"
		}
	}

	explanation = RecommendationExplanation{
		RawPercentile:       rawPercentile.DeepCopy(),
		SafetyMargin:        e.safetyMargin,
		AfterSafetyMargin:   afterSafetyMargin.DeepCopy(),
		BurstFactor:         burstFactor,
		AfterBurst:          afterBurst.DeepCopy(),
		Confidence:          profile.Confidence,
		ConfidenceFactor:    confidenceFactor,
		AfterConfidence:     afterConfidence.DeepCopy(),
		MinBound:            e.minBound.DeepCopy(),
		MaxBound:            e.maxBound.DeepCopy(),
		BoundsApplied:       boundsApplied,
		AfterBounds:         afterBounds.DeepCopy(),
		MinChangePercent:    e.minChangePercent,
		MaxChangePercent:    e.maxChangePercent,
		ChangeFilterApplied: changeFilterApplied,
		AfterChangeFilter:   afterChangeFilter.DeepCopy(),
		Final:               afterChangeFilter.DeepCopy(),
	}
	recommended = afterChangeFilter
	changed = recommended.Cmp(current) != 0
	return recommended, explanation, changed
}
