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
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/recommendation"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

// computeVPARecommendationsForWorkload builds WorkloadRecommendation by using
// VPA target values as the raw recommendation input. The VPA target is fed into
// the standard recommendation engines (overhead, confidence, bounds, change
// filter) as a synthetic UsageProfile.
func (r *AttunePolicyReconciler) computeVPARecommendationsForWorkload(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	vpaRecs []rsmetrics.VPAContainerRecommendation,
	cpuEngine, memEngine *recommendation.RecommendationEngine,
	excludeSet map[string]bool,
	pods []corev1.Pod,
) (rec *attunev1alpha1.WorkloadRecommendation, maxDataPoints int, err error) { //nolint:unparam // error return kept for interface contract
	logger := log.FromContext(ctx)
	logInvalidMemoryFromCPURatio(logger, policy)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, 0, nil
	}

	// Build engines if not pre-built (used in tests).
	if cpuEngine == nil || memEngine == nil {
		cpuEngine, memEngine = buildRecommendationEngines(policy)
	}
	if excludeSet == nil {
		excludeSet = pkgdefaults.EffectiveExcludedContainers(policy)
	}

	// Index VPA recommendations by container name for O(1) lookup.
	vpaByContainer := make(map[string]rsmetrics.VPAContainerRecommendation, len(vpaRecs))
	for _, v := range vpaRecs {
		vpaByContainer[v.ContainerName] = v
	}

	now := r.now()
	// VPA provides a single recommendation point; DataPoints=1 signals that.
	const vpaDataPoints = 1

	var containerRecs []attunev1alpha1.ContainerRecommendation
	eligibleContainers := 0
	partialUnfilled := false

	for _, container := range containers {
		containerName := container.Name

		if excludeSet[containerName] {
			logger.Info("Skipping excluded container",
				"container", containerName,
				"reason", pkgdefaults.ExclusionReason(policy, containerName))
			continue
		}
		eligibleContainers++

		vpaRec, found := vpaByContainer[containerName]
		if !found {
			logger.V(1).Info("No VPA recommendation for container", "container", containerName)
			continue
		}

		// Build synthetic UsageProfile from VPA target values.
		// VPA does its own percentile/confidence computation internally,
		// so we set all percentiles to the target value and confidence to 1.0.
		// Omitted targets must not be treated as 0 usage: that shrinks the
		// other resource through the engine. CPUSet/MemorySet come from parse.
		cpuValue := float64(vpaRec.CPUTarget.MilliValue()) / 1000.0 // cores
		memValue := float64(vpaRec.MemoryTarget.Value())            // bytes

		cpuProfile := rsmetrics.UsageProfile{
			OverallPercentiles: rsmetrics.PercentileSet{
				P50: cpuValue, P90: cpuValue, P95: cpuValue, P99: cpuValue, Max: cpuValue,
			},
			DataPoints: vpaDataPoints,
			Confidence: 1.0,
		}
		memProfile := rsmetrics.UsageProfile{
			OverallPercentiles: rsmetrics.PercentileSet{
				P50: memValue, P90: memValue, P95: memValue, P99: memValue, Max: memValue,
			},
			DataPoints: vpaDataPoints,
			Confidence: 1.0,
		}

		points := 0
		if vpaRec.CPUSet {
			points += vpaDataPoints
		}
		if vpaRec.MemorySet {
			points += vpaDataPoints
		}
		cRec := newContainerRecommendation(container,
			safeInt32(points),
			1.0, // VPA does its own confidence internally
			now)

		explanation := &attunev1alpha1.ContainerRecommendationExplanation{}
		cpuApplied := false
		var cpuRec resource.Quantity

		// Compute CPU recommendation through the standard engine pipeline.
		if vpaRec.CPUSet {
			var cpuExplain recommendation.RecommendationExplanation
			cpuRec, cpuExplain, _ = cpuEngine.RecommendWithExplanation(cpuProfile, cRec.Current.CPURequest)
			cpuAllowDecrease := policy.Spec.CPU.AllowDecrease == nil || *policy.Spec.CPU.AllowDecrease
			cpuRec = r.enforceAllowDecrease(cpuAllowDecrease, cpuRec, cRec.Current.CPURequest, &cpuExplain, policy, containerName, "CPU")
			cRec.Recommended.CPURequest = cpuRec
			explanation.CPU = toAPIRecommendationExplanation(cpuExplain)
			cpuApplied = true
		}

		// Compute memory recommendation. When memoryFromCpuRatio is set,
		// derive memory from the CPU recommendation instead of the VPA
		// memory target (same as the Prometheus path).
		memAllowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
		ratioSet := memoryFromCPURatioSet(policy)
		if cpuApplied && ratioSet && explanation.CPU != nil {
			ratio := parseFloat64Ratio(*policy.Spec.Memory.MemoryFromCPURatio)
			memRec, memExplain, applied := deriveMemoryFromCPU(
				cpuRec, ratio, memEngine, vpaDataPoints, cRec.Current.MemoryRequest, memAllowDecrease)
			if applied {
				cRec.Recommended.MemoryRequest = memRec
				memExplain.FinalAdjustment = appendNote(memExplain.FinalAdjustment,
					derivedFromCPURatioNote(*policy.Spec.Memory.MemoryFromCPURatio))
				explanation.Memory = toAPIRecommendationExplanation(memExplain)
			}
		}
		if ratioSet && !cpuApplied {
			// Do not credit vpaDataPoints while waiting: a memory-only
			// target would otherwise report 1/N Collecting data.
			logger.V(1).Info("memoryFromCpuRatio waiting for VPA CPU target",
				"container", containerName)
			continue
		}
		if explanation.Memory == nil && vpaRec.MemorySet && !ratioSet {
			memRec, memExplain, _ := memEngine.RecommendWithExplanation(memProfile, cRec.Current.MemoryRequest)
			memRec = r.enforceAllowDecrease(memAllowDecrease, memRec, cRec.Current.MemoryRequest, &memExplain, policy, containerName, "memory")
			cRec.Recommended.MemoryRequest = memRec
			explanation.Memory = toAPIRecommendationExplanation(memExplain)
		}
		if !cpuApplied || explanation.Memory == nil {
			prior := priorContainerRecommendation(policy, workloadKindName(workload), workload.GetName(), containerName)
			if !cpuApplied && !holdMissingResourceRequest(&cRec, corev1.ResourceCPU, pods, prior) {
				if cRec.Recommended.CPURequest.IsZero() {
					partialUnfilled = true
				}
			}
			if explanation.Memory == nil && !holdMissingResourceRequest(&cRec, corev1.ResourceMemory, pods, prior) {
				if cRec.Recommended.MemoryRequest.IsZero() {
					partialUnfilled = true
				}
			}
		}
		if explanation.CPU != nil {
			explanation.CPU.FinalAdjustment = appendNote(explanation.CPU.FinalAdjustment, "source: VPA")
		}
		if explanation.Memory != nil {
			explanation.Memory.FinalAdjustment = appendNote(explanation.Memory.FinalAdjustment, "source: VPA")
		}

		cRec.Explanation = explanation

		// Scale limits proportionally if ControlledValues is RequestsAndLimits.
		scaleControlledLimits(policy, &cRec, cRec.Current.CPURequest, cRec.Current.CPULimit, cRec.Current.MemoryRequest, cRec.Current.MemoryLimit)

		// Set recommendation gauges for this container.
		setRecommendationGauges(policy.Namespace, workload.GetName(), containerName, &cRec)

		logger.V(1).Info("Computed VPA-based recommendation",
			"container", containerName,
			"cpuCurrent", &cRec.Current.CPURequest,
			"cpuRecommended", &cRec.Recommended.CPURequest,
			"memCurrent", &cRec.Current.MemoryRequest,
			"memRecommended", &cRec.Recommended.MemoryRequest,
			"confidence", cRec.Confidence)

		// Count progress only for containers that produce a recommendation.
		if vpaDataPoints > maxDataPoints {
			maxDataPoints = vpaDataPoints
		}
		containerRecs = append(containerRecs, cRec)
	}

	if len(containerRecs) == 0 {
		// Only reuse when an eligible container had no VPA rec.
		// Exclude-all must still return nil so status drops the rec.
		// Under memoryFromCpuRatio, reuse only a ratio-derived prior rec.
		if eligibleContainers > 0 && staleReuseAllowed(policy, workloadKindName(workload), workload.GetName()) {
			freshness := recommendationFreshnessBound(0)
			if reused := reuseStaleRecommendation(policy, workloadKindName(workload), workload.GetName(), now, freshness); reused != nil {
				logger.Info("Reusing prior recommendation as stale; VPA returned no container recommendations",
					"workload", workload.GetName(),
					"kind", workloadKindName(workload))
				return reused, maxDataPoints, nil
			}
		}
		return nil, maxDataPoints, nil
	}

	lastDataTime := metav1.NewTime(now)
	return &attunev1alpha1.WorkloadRecommendation{
		Containers:   containerRecs,
		LastDataTime: &lastDataTime,
		Stale:        partialUnfilled,
	}, maxDataPoints, nil
}
