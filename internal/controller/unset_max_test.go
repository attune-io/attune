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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/pkg/defaults"
)

func unsetMaxPolicy() *attunev1alpha1.AttunePolicy {
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.CPU.Percentile = 95
	policy.Spec.CPU.Overhead = "0"
	policy.Spec.CPU.MaxIncreasePercent = int32Ptr(100)
	policy.Spec.CPU.MaxDecreasePercent = int32Ptr(100)
	policy.Spec.Memory.Percentile = 99
	policy.Spec.Memory.Overhead = "0"
	policy.Spec.Memory.MaxIncreasePercent = int32Ptr(100)
	policy.Spec.Memory.MaxDecreasePercent = int32Ptr(100)
	policy.Spec.Memory.AllowDecrease = boolPtr(true)
	return policy
}

func cpuProfile(p95 float64) rsmetrics.UsageProfile {
	return rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P95: p95},
		DataPoints:         100,
		Confidence:         1,
	}
}

func memoryProfile(p99 float64) rsmetrics.UsageProfile {
	return rsmetrics.UsageProfile{
		OverallPercentiles: rsmetrics.PercentileSet{P99: p99},
		DataPoints:         100,
		Confidence:         1,
	}
}

func TestUnsetMax_NilCPUMaxPublishesTenCPU(t *testing.T) {
	policy := unsetMaxPolicy()
	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("10"))
	assert.True(t, resource.MustParse("10").Equal(got), "published %s", got.String())
	assert.Empty(t, expl.BoundsApplied)
	assert.Nil(t, expl.MaxBound)
}

func TestUnsetMax_NilCPUMaxStillLimitsIncrease(t *testing.T) {
	policy := unsetMaxPolicy()
	policy.Spec.CPU.MaxIncreasePercent = int32Ptr(50)
	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("1"))
	assert.True(t, resource.MustParse("1500m").Equal(got), "published %s", got.String())
	assert.Empty(t, expl.BoundsApplied)
	assert.Equal(t, "max_change_capped", expl.ChangeFilterApplied)
	assert.Nil(t, expl.MaxBound)
}

func TestUnsetMax_NilMemoryMaxPublishesTwelveGi(t *testing.T) {
	policy := unsetMaxPolicy()
	_, memEngine := buildRecommendationEngines(policy)
	twelveGi := 12 * float64(1024*1024*1024)
	got, expl, _ := memEngine.RecommendWithExplanation(memoryProfile(twelveGi), resource.MustParse("16Gi"))
	assert.True(t, resource.MustParse("12Gi").Equal(got), "published %s", got.String())
	assert.Empty(t, expl.BoundsApplied)
	assert.Nil(t, expl.MaxBound)
}

func TestUnsetMax_ExplicitCPUMaxReclampsAfterChangeFilter(t *testing.T) {
	policy := unsetMaxPolicy()
	policy.Spec.CPU.MaxAllowed = quantityPtr("2")
	policy.Spec.CPU.MaxIncreasePercent = int32Ptr(50)
	policy.Spec.CPU.MaxDecreasePercent = int32Ptr(50)
	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("10"))
	assert.LessOrEqual(t, got.MilliValue(), int64(2000))
	assert.True(t, resource.MustParse("2").Equal(got), "published %s", got.String())
	assert.Equal(t, "max", expl.BoundsApplied)
	assert.Empty(t, expl.ChangeFilterApplied)
	require.NotNil(t, expl.MaxBound)
	assert.True(t, resource.MustParse("2").Equal(*expl.MaxBound))
}

func TestUnsetMax_ExplicitZeroMaxStaysACap(t *testing.T) {
	policy := unsetMaxPolicy()
	policy.Spec.CPU.MaxAllowed = quantityPtr("0")
	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("10"))
	assert.False(t, resource.MustParse("10").Equal(got), "published %s", got.String())
	assert.True(t, resource.MustParse("1m").Equal(got), "published %s", got.String())
	assert.Equal(t, "max", expl.BoundsApplied)
	require.NotNil(t, expl.MaxBound)
	assert.True(t, resource.MustParse("0").Equal(*expl.MaxBound))
}

func TestUnsetMax_ExplicitZeroMaxDecreaseCapPublishesFloor(t *testing.T) {
	policy := unsetMaxPolicy()
	policy.Spec.CPU.MaxAllowed = quantityPtr("0")
	policy.Spec.CPU.MaxDecreasePercent = int32Ptr(50)
	policy.Spec.Memory.MaxAllowed = quantityPtr("0")
	policy.Spec.Memory.MaxDecreasePercent = int32Ptr(50)
	cpuEngine, memEngine := buildRecommendationEngines(policy)

	cpuFloor, err := resource.ParseQuantity("1m")
	require.NoError(t, err)
	cpuGot, cpuExpl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("10"))
	assert.True(t, cpuFloor.Equal(cpuGot), "published %s", cpuGot.String())
	assert.Equal(t, "max", cpuExpl.BoundsApplied)
	require.NotNil(t, cpuExpl.MaxBound)
	assert.True(t, resource.MustParse("0").Equal(*cpuExpl.MaxBound))

	memFloor, err := resource.ParseQuantity("4Mi")
	require.NoError(t, err)
	memCurrent, err := resource.ParseQuantity("128Mi")
	require.NoError(t, err)
	memGot, memExpl, _ := memEngine.RecommendWithExplanation(memoryProfile(128*1024*1024), memCurrent)
	assert.True(t, memFloor.Equal(memGot), "published %s", memGot.String())
	assert.Equal(t, "max", memExpl.BoundsApplied)
	require.NotNil(t, memExpl.MaxBound)
	assert.True(t, resource.MustParse("0").Equal(*memExpl.MaxBound))
}

func TestUnsetMax_NilMinFloorsAtOneMillicoreAndFourMi(t *testing.T) {
	policy := unsetMaxPolicy()
	cpuEngine, memEngine := buildRecommendationEngines(policy)
	cpuGot, cpuExpl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(0), resource.MustParse("0"))
	assert.True(t, resource.MustParse("1m").Equal(cpuGot), "published %s", cpuGot.String())
	assert.Equal(t, "min", cpuExpl.BoundsApplied)
	assert.Nil(t, cpuExpl.MaxBound)

	memGot, memExpl, _ := memEngine.RecommendWithExplanation(memoryProfile(0), resource.MustParse("0"))
	assert.True(t, resource.MustParse("4Mi").Equal(memGot), "published %s", memGot.String())
	assert.Equal(t, "min", memExpl.BoundsApplied)
	assert.Nil(t, memExpl.MaxBound)
}

func TestUnsetMax_DefaultsMaxStillClamps(t *testing.T) {
	policy := unsetMaxPolicy()
	defs := &attunev1alpha1.AttuneDefaults{}
	defs.Spec.CPU = &attunev1alpha1.ResourceConfig{MaxAllowed: quantityPtr("8")}
	require.NotEmpty(t, defaults.MergeDefaults(policy, defs))
	require.NotNil(t, policy.Spec.CPU.MaxAllowed)

	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(10), resource.MustParse("10"))
	assert.True(t, resource.MustParse("8").Equal(got), "published %s", got.String())
	assert.Equal(t, "max", expl.BoundsApplied)
	assert.Empty(t, expl.ChangeFilterApplied)
	require.NotNil(t, expl.MaxBound)
	assert.True(t, resource.MustParse("8").Equal(*expl.MaxBound))
}

func TestUnsetMax_DefaultsMinBelowFloorWins(t *testing.T) {
	policy := unsetMaxPolicy()
	defs := &attunev1alpha1.AttuneDefaults{}
	defs.Spec.CPU = &attunev1alpha1.ResourceConfig{MinAllowed: quantityPtr("100u")}
	require.NotEmpty(t, defaults.MergeDefaults(policy, defs))
	require.NotNil(t, policy.Spec.CPU.MinAllowed)

	cpuEngine, _ := buildRecommendationEngines(policy)
	got, expl, _ := cpuEngine.RecommendWithExplanation(cpuProfile(0), resource.MustParse("0"))
	assert.True(t, resource.MustParse("100u").Equal(got), "published %s", got.String())
	assert.Equal(t, "min", expl.BoundsApplied)
	assert.Nil(t, expl.MaxBound)
}
