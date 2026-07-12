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

// Package recommendation provides a composable recommendation engine that
// combines percentile-based estimation, overheads, confidence adjustments,
// bounds clamping, and change filtering into a chain of estimators.
package recommendation

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/attune-io/attune/internal/metrics"
)

// estimator computes a recommended resource quantity based on a usage profile
// and the current resource allocation. Individual estimator decorators exist
// for focused unit testing of each algorithm step. The production path is
// RecommendationEngine.RecommendWithExplanation which inlines the chain for
// explanation tracking.
type estimator interface {
	Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity
}

// RecommendationExplanation captures the intermediate values produced by the
// estimator chain for a single resource recommendation.
type RecommendationExplanation struct {
	RawPercentile       resource.Quantity
	Overhead            float64
	AfterOverhead       resource.Quantity
	BurstFactor         float64
	AfterBurst          resource.Quantity
	Confidence          float64
	ConfidenceFactor    float64
	AfterConfidence     resource.Quantity
	MinBound            resource.Quantity
	MaxBound            resource.Quantity
	BoundsApplied       string
	AfterBounds         resource.Quantity
	MinChangePercent    float64
	MaxChangePercent    float64
	ChangeFilterApplied string
	AfterChangeFilter   resource.Quantity
	Final               resource.Quantity
	FinalAdjustment     string
}

// scaleQuantity multiplies q by factor, preserving BinarySI vs DecimalSI format.
// Returns q unchanged if factor is NaN, Inf, or non-positive (defense-in-depth).
// Clamps the result to math.MaxInt64 to prevent int64 overflow (which would
// wrap to a negative quantity).
func scaleQuantity(q resource.Quantity, factor float64) resource.Quantity {
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 0 {
		return q.DeepCopy()
	}
	if q.Format == resource.BinarySI {
		return *resource.NewQuantity(safeIntScale(float64(q.Value()), factor), resource.BinarySI)
	}
	return *resource.NewMilliQuantity(safeIntScale(float64(q.MilliValue()), factor), resource.DecimalSI)
}

// safeIntScale multiplies base by factor, rounds up, and clamps to
// [0, math.MaxInt64] to prevent int64 overflow.
func safeIntScale(base, factor float64) int64 {
	v := math.Ceil(base * factor)
	if v >= float64(math.MaxInt64) || math.IsInf(v, 1) {
		return math.MaxInt64
	}
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	return int64(v)
}
