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

package controller

import (
	"fmt"
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

const (
	// Default on-demand Linux pricing (approximate).
	defaultCPUPerCoreHour = 0.031
	defaultMemPerGiBHour  = 0.004
	hoursPerMonth         = 730
)

// savingsAccumulator holds accumulated resource diffs across all recommendations.
type savingsAccumulator struct {
	totalCPU         int64
	totalMem         int64
	totalCPUSaved    int64
	totalMemSaved    int64
	totalCPUIncrease int64
	totalMemIncrease int64
}

// accumulateSavings iterates over recommendations and accumulates resource diffs.
func accumulateSavings(recommendations []attunev1alpha1.WorkloadRecommendation) savingsAccumulator {
	var acc savingsAccumulator
	for _, rec := range recommendations {
		for _, c := range rec.Containers {
			acc.totalCPU += c.Current.CPURequest.MilliValue()
			acc.totalMem += c.Current.MemoryRequest.Value()

			cpuDiff := c.Current.CPURequest.MilliValue() - c.Recommended.CPURequest.MilliValue()
			if cpuDiff > 0 {
				acc.totalCPUSaved += cpuDiff
			} else if cpuDiff < 0 {
				acc.totalCPUIncrease += -cpuDiff
			}

			memDiff := c.Current.MemoryRequest.Value() - c.Recommended.MemoryRequest.Value()
			if memDiff > 0 {
				acc.totalMemSaved += memDiff
			} else if memDiff < 0 {
				acc.totalMemIncrease += -memDiff
			}
		}
	}
	return acc
}

// computeSavings calculates the aggregate resource savings across all recommendations.
// It also returns the accumulator so callers can pass it to updateSavingsGauges
// without re-iterating the recommendations.
func (r *AttunePolicyReconciler) computeSavings(recommendations []attunev1alpha1.WorkloadRecommendation, defaults *attunev1alpha1.AttuneDefaults) (attunev1alpha1.SavingsStatus, savingsAccumulator) {
	acc := accumulateSavings(recommendations)

	savings := attunev1alpha1.SavingsStatus{}
	if acc.totalCPU > 0 {
		savings.CPURequestTotal = resource.NewMilliQuantity(acc.totalCPU, resource.DecimalSI).String()
	}
	if acc.totalMem > 0 {
		savings.MemoryRequestTotal = resource.NewQuantity(acc.totalMem, resource.BinarySI).String()
	}
	if acc.totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(acc.totalCPUSaved, resource.DecimalSI).String()
	}
	if acc.totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(acc.totalMemSaved, resource.BinarySI).String()
	}
	if acc.totalCPUIncrease > 0 {
		savings.CPURequestIncrease = resource.NewMilliQuantity(acc.totalCPUIncrease, resource.DecimalSI).String()
	}
	if acc.totalMemIncrease > 0 {
		savings.MemoryRequestIncrease = resource.NewQuantity(acc.totalMemIncrease, resource.BinarySI).String()
	}

	cpuPrice, memPrice := getCostPricing(defaults)

	cpuCoresSaved := float64(acc.totalCPUSaved) / 1000.0
	memGiBSaved := float64(acc.totalMemSaved) / (1024 * 1024 * 1024)
	monthlySavings := (cpuCoresSaved*cpuPrice + memGiBSaved*memPrice) * hoursPerMonth
	if monthlySavings > 0 {
		savings.EstimatedMonthlySavings = fmt.Sprintf("$%.2f", monthlySavings)
	}

	cpuCoresIncrease := float64(acc.totalCPUIncrease) / 1000.0
	memGiBIncrease := float64(acc.totalMemIncrease) / (1024 * 1024 * 1024)
	monthlyCostIncrease := (cpuCoresIncrease*cpuPrice + memGiBIncrease*memPrice) * hoursPerMonth
	if monthlyCostIncrease > 0 {
		savings.EstimatedMonthlyCostIncrease = fmt.Sprintf("$%.2f", monthlyCostIncrease)
	}

	return savings, acc
}

// updateSavingsGauges publishes savings metrics to Prometheus gauges.
// Called from Reconcile after computeSavings. Separated so computeSavings
// remains a pure function that tests can call without registering collectors.
// Accepts a pre-computed accumulator to avoid re-iterating recommendations.
func updateSavingsGauges(namespace string, acc savingsAccumulator, defaults *attunev1alpha1.AttuneDefaults) {
	cpuCoresSaved := float64(acc.totalCPUSaved) / 1000.0
	memGiBSaved := float64(acc.totalMemSaved) / (1024 * 1024 * 1024)
	operatormetrics.SavingsCPU.WithLabelValues(namespace).Set(cpuCoresSaved)
	operatormetrics.SavingsMemory.WithLabelValues(namespace).Set(float64(acc.totalMemSaved))

	cpuPrice, memPrice := getCostPricing(defaults)
	monthlySavings := (cpuCoresSaved*cpuPrice + memGiBSaved*memPrice) * hoursPerMonth
	operatormetrics.SavingsEstimatedMonthly.WithLabelValues(namespace).Set(monthlySavings)
}

// getCostPricing reads pricing from AttuneDefaults, falling back to defaults.
func getCostPricing(defaults *attunev1alpha1.AttuneDefaults) (cpuPerCoreHour, memPerGiBHour float64) {
	cpuPerCoreHour = defaultCPUPerCoreHour
	memPerGiBHour = defaultMemPerGiBHour

	if defaults == nil {
		return
	}

	pricing := defaults.Spec.CostPricing
	if pricing == nil {
		return
	}

	if v := parseFloat64(pricing.CPUPerCoreHour, 0); v > 0 {
		cpuPerCoreHour = v
	}
	if v := parseFloat64(pricing.MemoryPerGiBHour, 0); v > 0 {
		memPerGiBHour = v
	}
	return
}

// scaleLimits scales a resource limit proportionally to maintain the same
// request:limit ratio when the request changes. Protects against int64
// overflow from extreme limit/request ratios.
func scaleLimits(currentReq, currentLim, newReq resource.Quantity) resource.Quantity {
	if currentReq.IsZero() || currentLim.IsZero() {
		// Return zero so buildResizeTarget excludes this limit from the target.
		// Setting limit = request would change the pod's QoS class.
		return resource.Quantity{}
	}
	ratio := float64(currentLim.MilliValue()) / float64(currentReq.MilliValue())
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return newReq.DeepCopy()
	}
	product := float64(newReq.MilliValue()) * ratio
	if product > float64(math.MaxInt64) || product < 0 {
		return currentLim.DeepCopy()
	}
	return *resource.NewMilliQuantity(int64(product), currentLim.Format)
}

// parseFloat64 parses a string as a float64, returning the fallback on error
// or if the value is NaN, Inf, negative, or unreasonably large (>10.0).
// Defense-in-depth: webhook validates first, but this protects when webhooks
// are disabled or bypassed.
func parseFloat64(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 10.0 {
		return fallback
	}
	return v
}

// parseFloat64Ratio parses a positive float64 with a ceiling of 1000
// (suitable for ratios like memoryFromCpuRatio). Returns 0 on error,
// NaN, Inf, non-positive, or > 1000.
func parseFloat64Ratio(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1000.0 { //nolint:mnd // 1000 is a generous ceiling for GiB-per-core ratios
		return 0
	}
	return v
}

// parseOverheadPercent parses an overhead percentage string (e.g. "20" for 20%).
// Returns fallback on error, NaN, Inf, negative, or > 900.
// Defense-in-depth: webhook validates first, but this protects when webhooks
// are disabled or bypassed.
func parseOverheadPercent(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 900 {
		return fallback
	}
	return v
}

// parseFloat64NonNeg parses a string as a non-negative float64, capped at 1.0.
// Returns fallback on error, NaN, Inf, or negative values.
func parseFloat64NonNeg(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fallback
	}
	if v > 1.0 {
		return 1.0
	}
	return v
}

// safeInt32 converts an int to int32, clamping to math.MaxInt32 on overflow.
func safeInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v) // #nosec G115 -- overflow guarded by check above
}
