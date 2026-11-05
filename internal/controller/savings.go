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

	corev1 "k8s.io/api/core/v1"
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

// accumulateSavings sums resource diffs. A nil pod map keeps the historical
// one-diff-per-container behavior for callers that only have the recommendation.
// A non-nil map sums (live request - recommended) for every pod of that
// workload. A pod already at the recommendation adds nothing. Missing pods
// add nothing: do not multiply the largest request by spec.replicas.
func accumulateSavings(recommendations []attunev1alpha1.WorkloadRecommendation, podsByWorkload map[string][]corev1.Pod) savingsAccumulator {
	var acc savingsAccumulator
	for _, rec := range recommendations {
		if rec.Stale {
			continue
		}
		if podsByWorkload == nil {
			for _, c := range rec.Containers {
				addSavingsDiff(&acc, c.Current.CPURequest, c.Recommended.CPURequest, true)
				addSavingsDiff(&acc, c.Current.MemoryRequest, c.Recommended.MemoryRequest, false)
			}
			continue
		}
		pods := podsByWorkload[rec.Workload]
		for _, c := range rec.Containers {
			for i := range pods {
				pod := &pods[i]
				if !savingsPodCounts(pod) {
					continue
				}
				ctr := findContainerByName(pod, c.Name)
				if ctr == nil {
					continue
				}
				// A missing request is zero. Skipping the key hid an increase
				// when the recommendation is non-zero.
				addSavingsDiff(&acc, ctr.Resources.Requests[corev1.ResourceCPU], c.Recommended.CPURequest, true)
				addSavingsDiff(&acc, ctr.Resources.Requests[corev1.ResourceMemory], c.Recommended.MemoryRequest, false)
			}
		}
	}
	return acc
}

// savingsPodCounts reports whether the pod still holds a request the policy
// can resize. Finished and deleting pods do not. A startup-boost pod is
// holding a temporary request, so it is not part of the steady sum.
func savingsPodCounts(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Annotations != nil && pod.Annotations[annotationStartupBoostAt] != "" {
		return false
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	default:
		return true
	}
}

func addSavingsDiff(acc *savingsAccumulator, live, recommended resource.Quantity, cpu bool) {
	if cpu {
		acc.totalCPU += live.MilliValue()
		diff := live.MilliValue() - recommended.MilliValue()
		if diff > 0 {
			acc.totalCPUSaved += diff
		} else if diff < 0 {
			acc.totalCPUIncrease += -diff
		}
		return
	}
	acc.totalMem += live.Value()
	diff := live.Value() - recommended.Value()
	if diff > 0 {
		acc.totalMemSaved += diff
	} else if diff < 0 {
		acc.totalMemIncrease += -diff
	}
}

// computeSavings calculates the aggregate resource savings across all recommendations.
// It also returns the accumulator so callers can pass it to updateSavingsGauges
// without re-iterating the recommendations. podsByWorkload nil keeps one diff
// per container. A non-nil map sums each live pod.
func (r *AttunePolicyReconciler) computeSavings(recommendations []attunev1alpha1.WorkloadRecommendation, defaults *attunev1alpha1.AttuneDefaults, podsByWorkload map[string][]corev1.Pod) (attunev1alpha1.SavingsStatus, savingsAccumulator) {
	acc := accumulateSavings(recommendations, podsByWorkload)

	savings := attunev1alpha1.SavingsStatus{}
	if acc.totalCPU > 0 {
		savings.CPURequestTotal = resource.NewMilliQuantity(acc.totalCPU, resource.DecimalSI).String()
	}
	if acc.totalMem > 0 {
		savings.MemoryRequestTotal = resource.NewQuantity(acc.totalMem, resource.BinarySI).String()
	}
	if acc.totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(acc.totalCPUSaved, resource.DecimalSI).String()
		// Alias for bin-packing / cluster-autoscaler capacity planning (#446).
		savings.ReclaimedCPURequest = savings.CPURequestReduction
	}
	if acc.totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(acc.totalMemSaved, resource.BinarySI).String()
		savings.ReclaimedMemoryRequest = savings.MemoryRequestReduction
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

// updateReclaimedCapacityGauges publishes freeable request capacity for
// bin-packing / cluster-autoscaler feedback (#446). Labels include policy.
func updateReclaimedCapacityGauges(namespace, policy string, acc savingsAccumulator) {
	cpuCoresSaved := float64(acc.totalCPUSaved) / 1000.0
	operatormetrics.ReclaimedRequestCPU.WithLabelValues(namespace, policy).Set(cpuCoresSaved)
	operatormetrics.ReclaimedRequestMemory.WithLabelValues(namespace, policy).Set(float64(acc.totalMemSaved))
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

// scaleLimits sets the new limit from the live request:limit ratio, or from
// multiplier when it is non-nil. A zero current request or limit returns a
// zero quantity so the limit stays omitted. Overflow keeps the current limit.
// A NaN, Inf, or non-positive ratio returns newReq.
// A scaled memory limit is rounded up to a whole byte; CPU keeps millicores.
func scaleLimits(name corev1.ResourceName, currentReq, currentLim, newReq resource.Quantity, multiplier *float64) resource.Quantity {
	if currentReq.IsZero() || currentLim.IsZero() {
		// Return zero so buildResizeTarget excludes this limit from the target.
		// Setting limit = request would change the pod's QoS class.
		return resource.Quantity{}
	}
	ratio := float64(currentLim.MilliValue()) / float64(currentReq.MilliValue())
	if multiplier != nil {
		ratio = *multiplier
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return newReq.DeepCopy()
	}
	product := float64(newReq.MilliValue()) * ratio
	if product >= float64(math.MaxInt64) || product < 0 {
		return currentLim.DeepCopy()
	}
	if name == corev1.ResourceMemory {
		return *resource.NewQuantity(ceilWholeBytes(product), currentLim.Format)
	}
	return *resource.NewMilliQuantity(int64(product), currentLim.Format)
}

// wholeByteEpsilon is the relative distance from a whole byte that
// ceilWholeBytes treats as floating-point noise. The product carries a few
// ulps of error (about 1e-15 relative); 1e-14 leaves a wide margin while a
// genuine fraction is dropped only below 1e-14 of the value (under one byte
// up to about 100 TB, far below a millibyte at realistic limits).
const wholeByteEpsilon = 1e-14

// ceilWholeBytes rounds a non-negative millibyte product, already checked to
// be below MaxInt64, up to a whole byte. A product within wholeByteEpsilon of
// a whole byte is that byte, so float noise above a whole product adds none.
func ceilWholeBytes(milli float64) int64 {
	b := milli / 1000
	if r := math.Round(b); math.Abs(b-r) <= wholeByteEpsilon*b {
		return int64(r)
	}
	return int64(math.Ceil(b))
}

// limitMultiplierRatio parses an explicit limitMultiplier. Nil means omitted,
// which keeps the live ratio. Invalid values also return nil so a bypassed
// webhook cannot publish a multiple below 1 or above MaxLimitMultiplier.
// A multiple below 1 would make the limit smaller than the request.
func limitMultiplierRatio(raw *string) *float64 {
	if raw == nil || *raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(*raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > float64(attunev1alpha1.MaxLimitMultiplier) {
		return nil
	}
	return &v
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
