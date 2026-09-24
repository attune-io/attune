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
	selected := percentileEstimator.selectedMax(profile)
	// A positive percentile is a real sample, even when DataPoints was left
	// unset. Hold only when there is nothing to scale: non-finite values,
	// or a zero percentile with no samples. Otherwise the confidence buffer
	// would multiply the current request.
	noSample := selected <= 0 && profile.DataPoints == 0
	if noSample || math.IsNaN(selected) || math.IsInf(selected, 0) {
		return current.DeepCopy(), holdAtCurrent(e, current), false
	}
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
	if confidence > 1.0 {
		confidence = 1.0
	}
	if confidence < 0 {
		confidence = 0
	}
	// Confidence factor adds a buffer for uncertainty: at confidence=1.0
	// (7 days of data), factor=1.0 (no extra buffer). At confidence=0.0
	// (no data), factor=2.0 (100% extra buffer on top of overhead).
	// Formula: 1 + multiplier * (1-confidence)^exponent.
	confidenceFactor := 1.0
	if e.confidenceMultiplier != 0 && e.confidenceExponent != 0 {
		confidenceFactor = 1 + e.confidenceMultiplier*math.Pow(1-confidence, e.confidenceExponent)
	}
	afterConfidence := scaleQuantity(afterBurst, confidenceFactor)

	afterBounds, boundsApplied := applyBounds(afterConfidence, e.minBound, e.maxBound)

	afterChangeFilter, changeFilterApplied := applyChangeFilter(
		current, afterBounds, e.minChangePercent, e.maxIncreasePercent, e.maxDecreasePercent)
	// Bounds are a hard limit. The change filter runs on the clamped
	// target, so a current value already outside [min, max] can be kept
	// (step under minChangePercent) or only partly moved (directional
	// cap stops short). Pull the published value back inside.
	if reclamped, which := applyBounds(afterChangeFilter, e.minBound, e.maxBound); which != "" {
		afterChangeFilter = reclamped
		changeFilterApplied = ""
		if boundsApplied == "" {
			boundsApplied = which
		}
	}
	maxPct := e.maxIncreasePercent
	if current.MilliValue() != 0 && afterBounds.MilliValue() <= current.MilliValue() {
		maxPct = e.maxDecreasePercent
	}

	explanation = RecommendationExplanation{
		RawPercentile:       rawPercentile.DeepCopy(),
		Overhead:            e.overhead,
		AfterOverhead:       afterOverhead.DeepCopy(),
		BurstFactor:         burstFactor,
		AfterBurst:          afterBurst.DeepCopy(),
		Confidence:          confidence,
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

// holdAtCurrent records an unchanged recommendation when the percentile
// is not a usable sample.
func holdAtCurrent(e *RecommendationEngine, current resource.Quantity) RecommendationExplanation {
	q := current.DeepCopy()
	return RecommendationExplanation{
		RawPercentile:     q.DeepCopy(),
		Overhead:          e.overhead,
		AfterOverhead:     q.DeepCopy(),
		BurstFactor:       1,
		AfterBurst:        q.DeepCopy(),
		Confidence:        0,
		ConfidenceFactor:  1,
		AfterConfidence:   q.DeepCopy(),
		MinBound:          e.minBound.DeepCopy(),
		MaxBound:          e.maxBound.DeepCopy(),
		AfterBounds:       q.DeepCopy(),
		MinChangePercent:  e.minChangePercent,
		MaxChangePercent:  e.maxIncreasePercent,
		AfterChangeFilter: q.DeepCopy(),
		Final:             q.DeepCopy(),
	}
}
