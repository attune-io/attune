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
// pipeline: percentile -> overhead -> confidence -> bounds -> change_filter.
// Each step is configured via the fields below and executed inline in
// RecommendWithExplanation.
type RecommendationEngine struct {
	percentile           int
	overhead             float64 // percentage to add (e.g. 20.0 = +20%); converted to multiplier via 1+overhead/100
	burstSensitivity     float64
	minBound             resource.Quantity
	maxBound             resource.Quantity
	minChangePercent     float64
	maxIncreasePercent   float64
	maxDecreasePercent   float64
	confidenceMultiplier float64
	confidenceExponent   float64
	isCPU                bool
}

// EngineOpts holds optional parameters for NewEngine.
type EngineOpts struct {
	// IsCPU selects CPU-specific percentile resolution.
	IsCPU bool
	// BurstSensitivity controls the burst boost multiplier.
	// Default (0) means use the standard 0.1; set explicitly to disable or tune.
	// Negative values are treated as 0 (no boost).
	BurstSensitivity *float64
}

// DefaultBurstSensitivity is the default burst sensitivity used when
// BurstSensitivity is nil.
const DefaultBurstSensitivity = 0.1

// NewEngine creates a new RecommendationEngine with the specified parameters.
// overhead is the percentage of additional resources (e.g., 20.0 for 20% extra).
// maxIncreasePct/maxDecreasePct cap directional changes per cycle.
func NewEngine(percentile int, overhead float64, minBound, maxBound resource.Quantity,
	maxIncreasePct, maxDecreasePct float64, opts ...EngineOpts,
) *RecommendationEngine {
	var opt EngineOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	bs := DefaultBurstSensitivity
	if opt.BurstSensitivity != nil {
		bs = *opt.BurstSensitivity
		if bs < 0 {
			bs = 0
		}
	}
	return &RecommendationEngine{
		percentile:           percentile,
		overhead:             overhead,
		burstSensitivity:     bs,
		minBound:             minBound.DeepCopy(),
		maxBound:             maxBound.DeepCopy(),
		minChangePercent:     10.0,
		maxIncreasePercent:   maxIncreasePct,
		maxDecreasePercent:   maxDecreasePct,
		confidenceMultiplier: 1.0,
		confidenceExponent:   2.0,
		isCPU:                opt.IsCPU,
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

	// Convert overhead percentage to multiplier: 20% overhead -> 1.2x multiplier.
	overheadMultiplier := 1 + e.overhead/100
	afterOverhead := scaleQuantity(rawPercentile, overheadMultiplier)

	// Burst-aware boost: if the profile detected a burst (max > 3x p95),
	// widen the overhead proportionally using a logarithmic scale
	// so extreme bursts don't inflate the recommendation excessively.
	burstFactor := 1.0
	if profile.BurstDetected && profile.BurstMagnitude > 1 && e.burstSensitivity > 0 {
		burstFactor = 1.0 + math.Log2(profile.BurstMagnitude)*e.burstSensitivity
	}
	afterBurst := scaleQuantity(afterOverhead, burstFactor)

	confidence := profile.Confidence
	if confidence < 0.1 {
		confidence = 0.1
	}
	if confidence > 1.0 {
		confidence = 1.0
	}
	// Confidence factor adds a buffer for uncertainty: at confidence=1.0
	// (7 days of data), factor=1.0 (no extra buffer). At confidence=0.1
	// (minimum data), factor≈1.8 (80% extra buffer on top of overhead).
	// Formula: 1 + multiplier * (1-confidence)^exponent.
	confidenceFactor := 1.0
	if e.confidenceMultiplier != 0 && e.confidenceExponent != 0 {
		confidenceFactor = 1 + e.confidenceMultiplier*math.Pow(1-confidence, e.confidenceExponent)
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
	// Determine which directional cap applies. Default to increase cap
	// for the explanation when current is zero (no change filter runs).
	maxPct := e.maxIncreasePercent
	currentMillis := float64(current.MilliValue())
	if currentMillis != 0 {
		afterBoundsMillis := float64(afterBounds.MilliValue())
		changePct := math.Abs(afterBoundsMillis-currentMillis) / currentMillis * 100
		isIncrease := afterBoundsMillis > currentMillis
		maxPct = e.maxDecreasePercent
		if isIncrease {
			maxPct = e.maxIncreasePercent
		}
		if changePct < e.minChangePercent {
			afterChangeFilter = current.DeepCopy()
			changeFilterApplied = "min_change_filtered"
		} else if changePct > maxPct {
			maxDelta := currentMillis * maxPct / 100
			capped := currentMillis - maxDelta
			if isIncrease {
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
		Overhead:            e.overhead,
		AfterOverhead:       afterOverhead.DeepCopy(),
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
		MaxChangePercent:    maxPct,
		ChangeFilterApplied: changeFilterApplied,
		AfterChangeFilter:   afterChangeFilter.DeepCopy(),
		Final:               afterChangeFilter.DeepCopy(),
	}
	recommended = afterChangeFilter
	changed = recommended.Cmp(current) != 0
	return recommended, explanation, changed
}
