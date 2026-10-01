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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/recommendation"
)

// surgeTestNow is 15:40 UTC so a 30m window stays inside hour 15.
var surgeTestNow = time.Date(2026, 10, 1, 15, 40, 0, 0, time.UTC)

func repeatFloat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func flatProfile(p, confidence float64) rsmetrics.UsageProfile {
	ps := rsmetrics.PercentileSet{P50: p, P90: p, P95: p, P99: p, Max: p}
	profile := rsmetrics.UsageProfile{
		OverallPercentiles: ps,
		DataPoints:         80,
		Confidence:         confidence,
		TimeSpanDays:       7,
	}
	for h := 0; h < 24; h++ {
		profile.HourlyPercentiles[h] = ps
	}
	return profile
}

// surgeSamples puts outside values at 10:00 and inside values in the
// last 20 minutes, which is inside the default 30m window and hour 15.
func surgeSamples(now time.Time, inside, outside []float64) []rsmetrics.Sample {
	out := make([]rsmetrics.Sample, 0, len(inside)+len(outside))
	for i, v := range outside {
		out = append(out, rsmetrics.Sample{
			Timestamp: time.Date(2026, 10, 1, 10, i%50, 0, 0, time.UTC),
			Value:     v,
		})
	}
	start := now.Add(-20 * time.Minute)
	step := time.Minute
	if len(inside) > 1 {
		step = (18 * time.Minute) / time.Duration(len(inside)-1)
		if step <= 0 {
			step = time.Second
		}
	}
	for i, v := range inside {
		out = append(out, rsmetrics.Sample{
			Timestamp: start.Add(time.Duration(i) * step),
			Value:     v,
		})
	}
	return out
}

func TestApplySurge_Decision(t *testing.T) {
	now := surgeTestNow
	step := 5 * time.Minute
	long := flatProfile(10, 1)
	on := &attunev1alpha1.Surge{}

	t.Run("nil block keeps the long window", func(t *testing.T) {
		hot := append(repeatFloat(95, 10), repeatFloat(5, 100)...)
		choice := applySurge(nil, long, surgeSamples(now, hot, []float64{10}), now, step, 95)
		assert.False(t, choice.fired)
	})

	t.Run("below trigger", func(t *testing.T) {
		samples := surgeSamples(now, repeatFloat(20, 14), []float64{10})
		choice := applySurge(on, long, samples, now, step, 95)
		assert.False(t, choice.fired)
		assert.Equal(t, long.Confidence, choice.profile.Confidence)
	})

	t.Run("at trigger", func(t *testing.T) {
		samples := surgeSamples(now, repeatFloat(20, 15), []float64{10})
		choice := applySurge(on, long, samples, now, step, 95)
		assert.True(t, choice.fired)
		assert.Equal(t, 99, choice.percentile)
		assert.Equal(t, long.Confidence, choice.profile.Confidence)
		assert.Equal(t, 20, choice.profile.DataPoints)
	})

	t.Run("one spike does not fire", func(t *testing.T) {
		inside := append(repeatFloat(99, 10), 15)
		choice := applySurge(on, long, surgeSamples(now, inside, []float64{10}), now, step, 95)
		assert.False(t, choice.fired)
	})

	t.Run("under 3 finite points", func(t *testing.T) {
		choice := applySurge(on, long, surgeSamples(now, []float64{100, 100}, []float64{10}), now, step, 95)
		assert.False(t, choice.fired)
	})

	t.Run("under half of window over step", func(t *testing.T) {
		choice := applySurge(on, long, surgeSamples(now, repeatFloat(4, 100), []float64{10}), now, 3*time.Minute, 95)
		assert.False(t, choice.fired)
	})

	t.Run("30m at 5m with six points fires", func(t *testing.T) {
		inside := append(repeatFloat(5, 10), 100)
		choice := applySurge(on, long, surgeSamples(now, inside, []float64{10}), now, step, 95)
		require.True(t, choice.fired)
		assert.Equal(t, 1.0, choice.profile.Confidence)
		assert.Equal(t, 6, choice.profile.DataPoints)
		assert.GreaterOrEqual(t, choice.profile.OverallPercentiles.P99, 15.0)
	})

	t.Run("query step 0 only requires 3 points", func(t *testing.T) {
		choice := applySurge(on, long, surgeSamples(now, repeatFloat(4, 100), []float64{10}), now, 0, 95)
		assert.True(t, choice.fired)
	})

	t.Run("long zero and short positive fires", func(t *testing.T) {
		choice := applySurge(on, flatProfile(0, 1), surgeSamples(now, repeatFloat(6, 20), []float64{0}), now, step, 95)
		require.True(t, choice.fired)
		eng := recommendation.NewEngine(95, 0, resource.MustParse("1m"), resource.Quantity{}, 500, 500, recommendation.EngineOpts{IsCPU: true, NoMax: true})
		_, expl, _ := eng.ForSurge(choice.percentile).RecommendWithExplanation(choice.profile, resource.MustParse("100m"))
		assert.False(t, math.IsNaN(expl.RawPercentile.AsApproximateFloat64()))
		assert.Greater(t, expl.RawPercentile.MilliValue(), int64(0))
	})

	t.Run("long zero and short zero does not fire", func(t *testing.T) {
		zeroLong := flatProfile(0, 1)
		choice := applySurge(on, zeroLong, surgeSamples(now, repeatFloat(6, 0), []float64{0}), now, step, 95)
		assert.False(t, choice.fired)
		eng := recommendation.NewEngine(95, 0, resource.MustParse("1m"), resource.Quantity{}, 100, 100, recommendation.EngineOpts{IsCPU: true, NoMax: true})
		_, expl, _ := eng.RecommendWithExplanation(choice.profile, resource.MustParse("100m"))
		assert.False(t, math.IsNaN(expl.RawPercentile.AsApproximateFloat64()))
		assert.Equal(t, int64(0), expl.RawPercentile.MilliValue())
	})

	t.Run("non finite long does not fire", func(t *testing.T) {
		nanLong := flatProfile(10, 1)
		nanLong.OverallPercentiles.P95 = math.NaN()
		choice := applySurge(on, nanLong, surgeSamples(now, repeatFloat(6, 100), []float64{10}), now, step, 95)
		assert.False(t, choice.fired)
		eng := recommendation.NewEngine(95, 0, resource.MustParse("1m"), resource.Quantity{}, 100, 100, recommendation.EngineOpts{IsCPU: true, NoMax: true})
		current := resource.MustParse("500m")
		_, expl, _ := eng.RecommendWithExplanation(choice.profile, current)
		assert.False(t, math.IsNaN(expl.RawPercentile.AsApproximateFloat64()))
		assert.Equal(t, current.MilliValue(), expl.RawPercentile.MilliValue())
	})

	t.Run("negative long or short does not fire", func(t *testing.T) {
		neg := flatProfile(-1, 1)
		choice := applySurge(on, neg, surgeSamples(now, repeatFloat(6, 100), []float64{10}), now, step, 95)
		assert.False(t, choice.fired)
		choice = applySurge(on, long, surgeSamples(now, repeatFloat(6, -5), []float64{-5}), now, step, 95)
		assert.False(t, choice.fired)
	})

	t.Run("non finite samples do not shorten the window", func(t *testing.T) {
		inside := append(repeatFloat(95, 10), repeatFloat(5, 100)...)
		samples := surgeSamples(now, inside, nil)
		samples = append(samples, rsmetrics.Sample{Timestamp: now.Add(-2 * time.Hour), Value: math.NaN()})
		choice := applySurge(on, long, samples, now, step, 95)
		assert.False(t, choice.fired, "a NaN outside the window is not a longer history")
	})

	t.Run("NaN and Inf are dropped before the short percentile", func(t *testing.T) {
		inside := append(repeatFloat(95, 10), repeatFloat(5, 100)...)
		samples := surgeSamples(now, inside, []float64{10})
		samples = append(samples,
			rsmetrics.Sample{Timestamp: now.Add(-time.Minute), Value: math.NaN()},
			rsmetrics.Sample{Timestamp: now.Add(-2 * time.Minute), Value: math.Inf(1)},
		)
		choice := applySurge(on, long, samples, now, step, 95)
		require.True(t, choice.fired)
		want := rsmetrics.BuildProfile(surgeSamples(now, inside, nil))
		assert.InDelta(t, want.OverallPercentiles.P99, choice.profile.OverallPercentiles.P99, 1e-9)
		assert.False(t, math.IsNaN(choice.profile.OverallPercentiles.P99))
		assert.False(t, math.IsInf(choice.profile.OverallPercentiles.P99, 0))
	})

	t.Run("every fetched sample already inside the window", func(t *testing.T) {
		inside := append(repeatFloat(95, 10), repeatFloat(5, 100)...)
		choice := applySurge(on, long, surgeSamples(now, inside, nil), now, step, 95)
		assert.False(t, choice.fired)
	})

	t.Run("explicit zero percentile or window does not fire", func(t *testing.T) {
		zero := int32(0)
		samples := surgeSamples(now, repeatFloat(6, 100), []float64{10})
		choice := applySurge(&attunev1alpha1.Surge{Percentile: &zero}, long, samples, now, step, 95)
		assert.False(t, choice.fired)
		choice = applySurge(&attunev1alpha1.Surge{Window: &metav1.Duration{}}, long, samples, now, step, 95)
		assert.False(t, choice.fired)
		choice = applySurge(&attunev1alpha1.Surge{TriggerRatio: "NaN"}, long, samples, now, step, 95)
		assert.False(t, choice.fired)
	})
}

func TestApplySurge_BurstFactor(t *testing.T) {
	now := surgeTestNow
	long := flatProfile(0.01, 1)
	inside := append(repeatFloat(95, 0.01), repeatFloat(5, 0.10)...)
	choice := applySurge(&attunev1alpha1.Surge{}, long, surgeSamples(now, inside, []float64{0.01}), now, 5*time.Minute, 95)
	require.True(t, choice.fired)
	require.True(t, choice.profile.BurstDetected)
	require.Greater(t, choice.profile.BurstMagnitude, 3.0)
	assert.Equal(t, 1.0, choice.profile.Confidence)

	eng := recommendation.NewEngine(95, 0, resource.MustParse("1m"), resource.Quantity{}, 500, 500, recommendation.EngineOpts{IsCPU: true, NoMax: true})
	_, expl, _ := eng.ForSurge(choice.percentile).RecommendWithExplanation(choice.profile, resource.MustParse("100m"))
	want := 1 + math.Log2(choice.profile.BurstMagnitude)*0.1
	assert.InDelta(t, want, expl.BurstFactor, 1e-9)
	assert.InDelta(t, 1.0, expl.Confidence, 1e-9)
	wantMilli := int64(math.Ceil(float64(expl.AfterOverhead.MilliValue()) * want))
	assert.Equal(t, wantMilli, expl.AfterBurst.MilliValue(), "burst rounds up to the next millicore")
}

func TestRecommendContainer_Surge(t *testing.T) {
	r := NewAttunePolicyReconciler()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"}}
	now := surgeTestNow
	zeroBurst := "0"

	basePolicy := func(surge *attunev1alpha1.Surge) *attunev1alpha1.AttunePolicy {
		return &attunev1alpha1.AttunePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Spec: attunev1alpha1.AttunePolicySpec{
				CPU: attunev1alpha1.ResourceConfig{
					Overhead:         "0",
					BurstSensitivity: &zeroBurst,
					Surge:            surge,
				},
				Memory: attunev1alpha1.ResourceConfig{
					Overhead:         "0",
					BurstSensitivity: &zeroBurst,
				},
			},
		}
	}
	container := corev1.Container{
		Name: "main",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	run := func(policy *attunev1alpha1.AttunePolicy, cpu, mem []rsmetrics.Sample, minPts int32, box corev1.Container) attunev1alpha1.ContainerRecommendation {
		t.Helper()
		cpuEng, memEng := buildRecommendationEngines(policy)
		rec, ok, _, pts := r.recommendContainer(context.Background(), recommendContainerInput{
			policy:            policy,
			workload:          deploy,
			container:         box,
			cpuSamples:        cpu,
			memSamples:        mem,
			cpuEngine:         cpuEng,
			memEngine:         memEng,
			now:               now,
			minimumDataPoints: minPts,
		})
		require.True(t, ok)
		require.GreaterOrEqual(t, pts, int(minPts))
		return rec
	}

	t.Run("short series still passes the long data gate", func(t *testing.T) {
		inside := append(repeatFloat(19, 10), 100)
		outside := repeatFloat(40, 10)
		rec := run(basePolicy(&attunev1alpha1.Surge{}), surgeSamples(now, inside, outside), nil, 48, container)
		require.NotNil(t, rec.Explanation)
		require.NotNil(t, rec.Explanation.CPU)
		assert.Contains(t, rec.Explanation.CPU.FinalAdjustment, "surge")
		assert.GreaterOrEqual(t, rec.DataPoints, int32(48))
	})

	t.Run("one spike does not switch windows", func(t *testing.T) {
		inside := append(repeatFloat(99, 10), 15)
		rec := run(basePolicy(&attunev1alpha1.Surge{}), surgeSamples(now, inside, []float64{10}), nil, 1, container)
		require.NotNil(t, rec.Explanation.CPU)
		assert.NotContains(t, rec.Explanation.CPU.FinalAdjustment, "surge")
		assert.Less(t, rec.Explanation.CPU.RawPercentile.MilliValue(), int64(11000))
	})

	t.Run("flat short window keeps the long percentile", func(t *testing.T) {
		with := run(basePolicy(&attunev1alpha1.Surge{}), surgeSamples(now, repeatFloat(20, 0.2), []float64{0.2}), nil, 1, container)
		without := run(basePolicy(nil), surgeSamples(now, repeatFloat(20, 0.2), []float64{0.2}), nil, 1, container)
		assert.NotContains(t, with.Explanation.CPU.FinalAdjustment, "surge")
		assert.Equal(t, without.Explanation.CPU.RawPercentile.MilliValue(), with.Explanation.CPU.RawPercentile.MilliValue())
	})

	t.Run("change cap still limits a hot short window", func(t *testing.T) {
		inside := append(repeatFloat(95, 0.1), repeatFloat(5, 2)...)
		rec := run(basePolicy(&attunev1alpha1.Surge{}), surgeSamples(now, inside, []float64{0.1}), nil, 1, container)
		require.NotNil(t, rec.Explanation.CPU)
		assert.Contains(t, rec.Explanation.CPU.FinalAdjustment, "surge")
		assert.Equal(t, "max_change_capped", rec.Explanation.CPU.ChangeFilterApplied)
		assert.Equal(t, int64(1500), rec.Recommended.CPURequest.MilliValue())
	})

	t.Run("cpu surge does not rebuild the memory profile", func(t *testing.T) {
		allow := true
		maxChange := int32(100)
		policy := basePolicy(&attunev1alpha1.Surge{})
		policy.Spec.Memory.AllowDecrease = &allow
		policy.Spec.Memory.MaxChangePercent = &maxChange
		off := basePolicy(nil)
		off.Spec.Memory.AllowDecrease = &allow
		off.Spec.Memory.MaxChangePercent = &maxChange
		cpu := surgeSamples(now, append(repeatFloat(95, 0.05), repeatFloat(5, 1)...), []float64{0.05})
		mem := surgeSamples(now, repeatFloat(20, 64*1024*1024), []float64{64 * 1024 * 1024})
		with := run(policy, cpu, mem, 1, container)
		without := run(off, cpu, mem, 1, container)
		require.NotNil(t, with.Explanation.CPU)
		require.NotNil(t, with.Explanation.Memory)
		assert.Contains(t, with.Explanation.CPU.FinalAdjustment, "surge")
		assert.NotContains(t, with.Explanation.Memory.FinalAdjustment, "surge")
		assert.Equal(t, without.Explanation.Memory.RawPercentile.Value(), with.Explanation.Memory.RawPercentile.Value())
		assert.NotEqual(t, without.Explanation.CPU.RawPercentile.MilliValue(), with.Explanation.CPU.RawPercentile.MilliValue())
	})

	t.Run("memory from cpu ratio follows the surged request", func(t *testing.T) {
		ratio := "2"
		policy := basePolicy(&attunev1alpha1.Surge{})
		policy.Spec.Memory.MemoryFromCPURatio = &ratio
		off := basePolicy(nil)
		off.Spec.Memory.MemoryFromCPURatio = &ratio
		box := container.DeepCopy()
		box.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0")
		box.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("0")
		cpu := surgeSamples(now, append(repeatFloat(95, 0.05), repeatFloat(5, 1)...), []float64{0.05})
		with := run(policy, cpu, nil, 1, *box)
		without := run(off, cpu, nil, 1, *box)
		require.NotNil(t, with.Explanation.Memory)
		assert.Contains(t, with.Explanation.CPU.FinalAdjustment, "surge")
		assert.NotContains(t, with.Explanation.Memory.FinalAdjustment, "surge")
		assert.Contains(t, with.Explanation.Memory.FinalAdjustment, "memoryFromCpuRatio")
		assert.NotEqual(t, without.Recommended.CPURequest.MilliValue(), with.Recommended.CPURequest.MilliValue())
		assert.NotEqual(t, without.Recommended.MemoryRequest.Value(), with.Recommended.MemoryRequest.Value())
	})
}
