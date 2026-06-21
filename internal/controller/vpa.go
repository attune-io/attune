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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/recommendation"
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
) (rec *attunev1alpha1.WorkloadRecommendation, maxDataPoints int, err error) { //nolint:unparam // error return kept for interface contract
	logger := log.FromContext(ctx)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, 0, nil
	}

	// Build engines if not pre-built (used in tests).
	if cpuEngine == nil || memEngine == nil {
		cpuEngine, memEngine = buildRecommendationEngines(policy)
	}
	if excludeSet == nil {
		excludeSet = make(map[string]bool, len(policy.Spec.ExcludedContainers))
		for _, name := range policy.Spec.ExcludedContainers {
			excludeSet[name] = true
		}
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

	for _, container := range containers {
		containerName := container.Name

		if excludeSet[containerName] {
			logger.Info("Skipping excluded container", "container", containerName)
			continue
		}

		vpaRec, found := vpaByContainer[containerName]
		if !found {
			logger.V(1).Info("No VPA recommendation for container", "container", containerName)
			continue
		}

		// Build synthetic UsageProfile from VPA target values.
		// VPA does its own percentile/confidence computation internally,
		// so we set all percentiles to the target value and confidence to 1.0.
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

		if vpaDataPoints > maxDataPoints {
			maxDataPoints = vpaDataPoints
		}

		cRec := newContainerRecommendation(container,
			safeInt32(cpuProfile.DataPoints+memProfile.DataPoints),
			1.0, // VPA does its own confidence internally
			now)

		explanation := &attunev1alpha1.ContainerRecommendationExplanation{}

		// Compute CPU recommendation through the standard engine pipeline.
		cpuRec, cpuExplain, _ := cpuEngine.RecommendWithExplanation(cpuProfile, cRec.Current.CPURequest)
		cpuAllowDecrease := policy.Spec.CPU.AllowDecrease == nil || *policy.Spec.CPU.AllowDecrease
		cpuRec = r.enforceAllowDecrease(cpuAllowDecrease, cpuRec, cRec.Current.CPURequest, &cpuExplain, policy, containerName, "CPU")
		cRec.Recommended.CPURequest = cpuRec
		explanation.CPU = toAPIRecommendationExplanation(cpuExplain)

		// Compute memory recommendation through the standard engine pipeline.
		memRec, memExplain, _ := memEngine.RecommendWithExplanation(memProfile, cRec.Current.MemoryRequest)
		memAllowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
		memRec = r.enforceAllowDecrease(memAllowDecrease, memRec, cRec.Current.MemoryRequest, &memExplain, policy, containerName, "memory")
		cRec.Recommended.MemoryRequest = memRec
		explanation.Memory = toAPIRecommendationExplanation(memExplain)
		explanation.CPU.FinalAdjustment = appendNote(explanation.CPU.FinalAdjustment, "source: VPA")
		explanation.Memory.FinalAdjustment = appendNote(explanation.Memory.FinalAdjustment, "source: VPA")

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

		containerRecs = append(containerRecs, cRec)
	}

	if len(containerRecs) == 0 {
		return nil, maxDataPoints, nil
	}

	lastDataTime := metav1.NewTime(now)
	return &attunev1alpha1.WorkloadRecommendation{
		Containers:   containerRecs,
		LastDataTime: &lastDataTime,
	}, maxDataPoints, nil
}
