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

// newTestMemEngine creates a memory recommendation engine with wide bounds
// and no change filter so that derived values pass through unmodified.
func newTestMemEngine() *recommendation.RecommendationEngine {
	return recommendation.NewEngine(
		99,                         // percentile (irrelevant for synthetic profiles)
		0,                          // overhead (0% so derived value is not inflated)
		resource.MustParse("1Mi"),  // minBound
		resource.MustParse("64Gi"), // maxBound
		100,                        // maxIncreasePct (wide)
		100,                        // maxDecreasePct (wide)
	)
}

func TestSamplesForContainer(t *testing.T) {
	named := []rsmetrics.Sample{{Value: 1.5}}
	fallback := []rsmetrics.Sample{{Value: 9.0}}

	tests := []struct {
		name      string
		grouped   map[string][]rsmetrics.Sample
		container string
		want      []rsmetrics.Sample
	}{
		{
			name: "named-key hit",
			grouped: map[string][]rsmetrics.Sample{
				"web": named,
				"":    fallback,
			},
			container: "web",
			want:      named,
		},
		{
			name: "empty-key fallback",
			grouped: map[string][]rsmetrics.Sample{
				"": fallback,
			},
			container: "web",
			want:      fallback,
		},
		{
			name:      "nil map",
			grouped:   nil,
			container: "web",
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := samplesForContainer(tt.grouped, tt.container)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLatestFiniteSampleTime(t *testing.T) {
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	t3 := time.Unix(3000, 0)
	t4 := time.Unix(4000, 0)

	tests := []struct {
		name   string
		groups []map[string][]rsmetrics.Sample
		want   time.Time
	}{
		{
			name: "newest finite across two groups",
			groups: []map[string][]rsmetrics.Sample{
				{
					"web": {
						{Timestamp: t1, Value: 1.0},
						{Timestamp: t4, Value: math.NaN()},
					},
				},
				{
					"db": {
						{Timestamp: t3, Value: math.Inf(1)},
						{Timestamp: t2, Value: 2.0},
						{Timestamp: t4, Value: math.Inf(-1)},
					},
				},
			},
			want: t2,
		},
		{
			name: "zero time when every point is non-finite",
			groups: []map[string][]rsmetrics.Sample{
				{
					"web": {
						{Timestamp: t3, Value: math.NaN()},
						{Timestamp: t4, Value: math.Inf(1)},
					},
				},
				{
					"db": {
						{Timestamp: t2, Value: math.Inf(-1)},
					},
				},
			},
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := latestFiniteSampleTime(tt.groups...)
			assert.True(t, got.Equal(tt.want), "got %v want %v", got, tt.want)
		})
	}
}

func Test_secretForCacheKey(t *testing.T) {
	tests := []struct {
		name    string
		val     string
		wantLen int // 0 means empty, >0 means non-empty hex string
	}{
		{
			name:    "empty string returns empty",
			val:     "",
			wantLen: 0,
		},
		{
			name:    "non-empty string returns hex hash",
			val:     "my-secret-token",
			wantLen: 16, // FNV-64a produces 16 hex chars
		},
		{
			name:    "different values produce different hashes",
			val:     "another-token",
			wantLen: 16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := secretForCacheKey(tt.val)
			if tt.wantLen == 0 {
				assert.Empty(t, got)
			} else {
				assert.Len(t, got, tt.wantLen, "expected %d hex chars", tt.wantLen)
			}
		})
	}

	// Verify distinct inputs produce distinct outputs.
	a := secretForCacheKey("token-A")
	b := secretForCacheKey("token-B")
	assert.NotEqual(t, a, b, "different secrets must produce different cache keys")
}

func TestDeriveMemoryFromCPU(t *testing.T) {
	tests := []struct {
		name          string
		cpuRec        string
		ratio         float64
		currentMem    string
		allowDecrease bool
		wantApplied   bool
		wantMemMin    string // minimum expected memory (inclusive)
		wantMemMax    string // maximum expected memory (inclusive)
		wantAdjust    string // substring expected in FinalAdjustment
	}{
		{
			name:        "1 core with ratio 2.0 produces ~2Gi",
			cpuRec:      "1000m",
			ratio:       2.0,
			currentMem:  "512Mi",
			wantApplied: true,
			wantMemMin:  "1Gi",
			wantMemMax:  "2500Mi", // ~2Gi raw, no confidence inflation
		},
		{
			name:        "100m CPU with ratio 4.0 produces ~400Mi",
			cpuRec:      "100m",
			ratio:       4.0,
			currentMem:  "128Mi",
			wantApplied: true,
			wantMemMin:  "200Mi",
			wantMemMax:  "512Mi",
		},
		{
			name:        "500m CPU with ratio 1.0 produces ~512Mi",
			cpuRec:      "500m",
			ratio:       1.0,
			currentMem:  "256Mi",
			wantApplied: true,
			wantMemMin:  "256Mi",
			wantMemMax:  "768Mi",
		},
		{
			name:          "decrease blocked by allowDecrease=false",
			cpuRec:        "100m",
			ratio:         1.0,
			currentMem:    "1Gi",
			allowDecrease: false,
			wantApplied:   true,
			wantMemMin:    "1Gi", // must not go below current
			wantMemMax:    "1Gi",
			wantAdjust:    "allowDecrease=false",
		},
		{
			name:          "decrease allowed when flag is true",
			cpuRec:        "100m",
			ratio:         1.0,
			currentMem:    "1Gi",
			allowDecrease: true,
			wantApplied:   true,
			wantMemMin:    "64Mi",  // can go below current
			wantMemMax:    "256Mi", // ~102Mi raw, no confidence inflation
		},
		{
			name:        "zero ratio not applied",
			cpuRec:      "1000m",
			ratio:       0,
			currentMem:  "512Mi",
			wantApplied: false,
		},
		{
			name:        "negative ratio not applied",
			cpuRec:      "1000m",
			ratio:       -1.0,
			currentMem:  "512Mi",
			wantApplied: false,
		},
		{
			name:        "large CPU with small ratio",
			cpuRec:      "4000m",
			ratio:       0.5,
			currentMem:  "512Mi",
			wantApplied: true,
			wantMemMin:  "1Gi",
			wantMemMax:  "3Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpuRec := resource.MustParse(tt.cpuRec)
			currentMem := resource.MustParse(tt.currentMem)
			engine := newTestMemEngine()

			memRec, explain, applied := deriveMemoryFromCPU(
				cpuRec, tt.ratio, engine, 48, currentMem, tt.allowDecrease)

			assert.Equal(t, tt.wantApplied, applied, "applied mismatch")
			if !tt.wantApplied {
				return
			}

			require.False(t, memRec.IsZero(), "derived memory should not be zero")

			if tt.wantMemMin != "" {
				minQ := resource.MustParse(tt.wantMemMin)
				assert.True(t, memRec.Cmp(minQ) >= 0,
					"memory %s should be >= %s", memRec.String(), minQ.String())
			}
			if tt.wantMemMax != "" {
				maxQ := resource.MustParse(tt.wantMemMax)
				assert.True(t, memRec.Cmp(maxQ) <= 0,
					"memory %s should be <= %s", memRec.String(), maxQ.String())
			}

			// Explanation should always have a non-zero Final when applied.
			assert.False(t, explain.Final.IsZero(), "explanation.Final should not be zero")

			if tt.wantAdjust != "" {
				assert.Contains(t, explain.FinalAdjustment, tt.wantAdjust)
			}
		})
	}
}

func TestDeriveMemoryFromCPU_BoundsEnforced(t *testing.T) {
	// Engine with tight bounds to verify clamping.
	engine := recommendation.NewEngine(99, 0,
		resource.MustParse("100Mi"), // minBound
		resource.MustParse("512Mi"), // maxBound
		100, 100)

	// 4 cores * ratio 2.0 = 8Gi, but maxBound is 512Mi.
	cpuRec := resource.MustParse("4000m")
	currentMem := resource.MustParse("256Mi")

	memRec, _, applied := deriveMemoryFromCPU(cpuRec, 2.0, engine, 48, currentMem, true)
	require.True(t, applied)

	maxBound := resource.MustParse("512Mi")
	assert.True(t, memRec.Cmp(maxBound) <= 0,
		"memory %s should be clamped to maxBound %s", memRec.String(), maxBound.String())
}

func TestDeriveMemoryFromCPU_MinBoundEnforced(t *testing.T) {
	engine := recommendation.NewEngine(99, 0,
		resource.MustParse("256Mi"), // minBound
		resource.MustParse("64Gi"),  // maxBound
		100, 100)

	// 10m CPU * ratio 0.5 = ~5Mi, but minBound is 256Mi.
	cpuRec := resource.MustParse("10m")
	currentMem := resource.MustParse("128Mi")

	memRec, _, applied := deriveMemoryFromCPU(cpuRec, 0.5, engine, 48, currentMem, true)
	require.True(t, applied)

	minBound := resource.MustParse("256Mi")
	assert.True(t, memRec.Cmp(minBound) >= 0,
		"memory %s should be clamped to minBound %s", memRec.String(), minBound.String())
}

func TestResolvePodAggregation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want rsmetrics.PodAggregationMode
	}{
		{name: "avg", in: "Avg", want: rsmetrics.PodAggregationAvg},
		{name: "none", in: "None", want: rsmetrics.PodAggregationNone},
		{name: "max", in: "Max", want: rsmetrics.PodAggregationMax},
		{name: "empty defaults to max", in: "", want: rsmetrics.PodAggregationMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &attunev1alpha1.AttunePolicy{}
			policy.Spec.MetricsSource.PodAggregation = tt.in
			assert.Equal(t, tt.want, resolvePodAggregation(policy))
			assert.Equal(t, "podAggregation="+string(tt.want), podAggregationNote(policy))
		})
	}
}

func TestBurstSensitivityNote(t *testing.T) {
	zero := "0"
	tuned := "0.3"
	assert.Equal(t, "burstSensitivity=0.1", burstSensitivityNote(nil))
	assert.Equal(t, "burstSensitivity=0", burstSensitivityNote(&zero))
	assert.Equal(t, "burstSensitivity=0.3", burstSensitivityNote(&tuned))
}

func TestRecordQuerySettings_StampsResolvedValues(t *testing.T) {
	zero := "0"
	policy := &attunev1alpha1.AttunePolicy{}
	policy.Spec.MetricsSource.PodAggregation = "Avg"
	policy.Spec.CPU.BurstSensitivity = &zero
	explain := &attunev1alpha1.ContainerRecommendationExplanation{
		CPU:    &attunev1alpha1.ResourceRecommendationExplanation{},
		Memory: &attunev1alpha1.ResourceRecommendationExplanation{},
	}
	recordQuerySettings(policy, explain)
	assert.Contains(t, explain.CPU.FinalAdjustment, "podAggregation=Avg")
	assert.Contains(t, explain.CPU.FinalAdjustment, "burstSensitivity=0")
	assert.NotContains(t, explain.CPU.FinalAdjustment, "podAggregation=Max")
	assert.Contains(t, explain.Memory.FinalAdjustment, "podAggregation=Avg")
}

func Test_appendNote(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		note     string
		want     string
	}{
		{
			name:     "empty existing returns note only",
			existing: "",
			note:     "clamped to bounds",
			want:     "clamped to bounds",
		},
		{
			name:     "non-empty existing appends with separator",
			existing: "applied overhead",
			note:     "clamped to bounds",
			want:     "applied overhead; clamped to bounds",
		},
		{
			name:     "chaining three notes",
			existing: "a; b",
			note:     "c",
			want:     "a; b; c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendNote(tt.existing, tt.note)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_mustParseFloat_ValidInput(t *testing.T) {
	assert.Equal(t, 1.5, mustParseFloat("1.5"))
	assert.Equal(t, 0.0, mustParseFloat("0"))
	assert.Equal(t, -3.14, mustParseFloat("-3.14"))
	// Built-in overhead constants must stay finite (ParseFloat accepts Inf/NaN).
	assert.Equal(t, 20.0, mustParseFloat(attunev1alpha1.DefaultCPUOverhead))
	assert.Equal(t, 30.0, mustParseFloat(attunev1alpha1.DefaultMemoryOverhead))
}

func Test_mustParseFloat_InvalidInput_Panics(t *testing.T) {
	for _, s := range []string{"not-a-number", "NaN", "Inf", "+Inf", "-Inf"} {
		assert.Panicsf(t, func() {
			mustParseFloat(s)
		}, "mustParseFloat(%q) should panic", s)
	}
}

func TestDeriveMemoryFromCPU_ExactMath(t *testing.T) {
	// Verify the core math: 1000m CPU * ratio 2.0 = 2 GiB raw.
	// The engine applies overhead (0% in test engine) but the confidence
	// factor is neutralized (synthetic profile uses confidence=1e9).
	// With 0% overhead and wide bounds, expect close to 2Gi.
	engine := newTestMemEngine()
	cpuRec := resource.MustParse("1000m")
	currentMem := resource.MustParse("1Gi")

	memRec, _, applied := deriveMemoryFromCPU(cpuRec, 2.0, engine, 48, currentMem, true)
	require.True(t, applied)

	// Expect ~2Gi (no confidence inflation, 0% overhead).
	twoGi := resource.MustParse("2Gi")
	diff := memRec.Value() - twoGi.Value()
	pctDiff := float64(diff) / float64(twoGi.Value()) * 100
	assert.InDelta(t, 0, pctDiff, 5,
		"memory %s should be within 5%% of 2Gi (got %.1f%% diff)", memRec.String(), pctDiff)
}

func TestHoldMissingResourceRequest_PrefersLargerLastRecOverTemplateLive(t *testing.T) {
	templateMem := resource.MustParse("256Mi")
	lastRec := resource.MustParse("1Gi")
	rec := &attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			MemoryRequest: templateMem.DeepCopy(),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: templateMem.DeepCopy(),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}
	// Pods still at the template; last rec is the in-place target.
	pod := newResizePod("api-server", "500m", "256Mi", "1000m", "512Mi")
	prior := &attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: lastRec.DeepCopy(),
		},
	}

	ok := holdMissingResourceRequest(rec, corev1.ResourceMemory, []corev1.Pod{*pod}, prior)
	require.True(t, ok)
	assert.True(t, rec.Recommended.MemoryRequest.Equal(lastRec),
		"must keep last rec %s over template-live %s, got %s",
		lastRec.String(), templateMem.String(), rec.Recommended.MemoryRequest.String())
}

func TestHoldMissingResourceRequest_LargerLiveBeatsSmallerLastRec(t *testing.T) {
	liveMem := resource.MustParse("1Gi")
	rec := &attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("256Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}
	pod := newResizePod("api-server", "500m", "1Gi", "1000m", "1Gi")
	prior := &attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("256Mi"),
		},
	}

	ok := holdMissingResourceRequest(rec, corev1.ResourceMemory, []corev1.Pod{*pod}, prior)
	require.True(t, ok)
	assert.True(t, rec.Recommended.MemoryRequest.Equal(liveMem),
		"larger live %s must beat last rec 256Mi, got %s",
		liveMem.String(), rec.Recommended.MemoryRequest.String())
}

func TestHoldMissingResourceRequest_ZeroLiveLimitClearsTemplateLimit(t *testing.T) {
	liveMem := resource.MustParse("1Gi")
	rec := &attunev1alpha1.ContainerRecommendation{
		Name: "main",
		Current: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("256Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
		Recommended: attunev1alpha1.ResourceValues{
			MemoryRequest: resource.MustParse("256Mi"),
			MemoryLimit:   resource.MustParse("512Mi"),
		},
	}
	pod := newResizePod("api-server", "500m", "1Gi", "1000m", "1Gi")
	delete(pod.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)

	ok := holdMissingResourceRequest(rec, corev1.ResourceMemory, []corev1.Pod{*pod}, nil)
	require.True(t, ok)
	assert.True(t, rec.Recommended.MemoryRequest.Equal(liveMem),
		"held request must stay live %s, got %s", liveMem.String(), rec.Recommended.MemoryRequest.String())
	assert.True(t, rec.Recommended.MemoryLimit.IsZero(),
		"held rec must not keep template memory limit when live has none, got %s",
		rec.Recommended.MemoryLimit.String())
	assert.True(t, rec.Current.MemoryLimit.IsZero(),
		"Current limit must also drop so quota/undo does not keep the template limit")

	target, clamped := buildResizeTarget(*rec)
	gotTargetMem := target.Requests[corev1.ResourceMemory]
	assert.True(t, gotTargetMem.Equal(liveMem),
		"resize target must keep held %s, got %s", liveMem.String(), gotTargetMem.String())
	assert.Empty(t, clamped, "leftover template limit must not clamp the held request")
}

func TestRecommendContainer_SkipAndProduce(t *testing.T) {
	r := NewAttunePolicyReconciler()
	cpuEng, memEng := buildRecommendationEngines(&attunev1alpha1.AttunePolicy{})
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
	}
	container := corev1.Container{
		Name: "main",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	t.Run("insufficient both sides", func(t *testing.T) {
		_, ok, unfilled, pts := r.recommendContainer(context.Background(), recommendContainerInput{
			policy:            policy,
			workload:          deploy,
			container:         container,
			cpuEngine:         cpuEng,
			memEngine:         memEng,
			now:               time.Now(),
			minimumDataPoints: 1,
		})
		assert.False(t, ok)
		assert.False(t, unfilled)
		assert.Equal(t, 0, pts)
	})

	t.Run("cpu samples produce rec", func(t *testing.T) {
		now := time.Now()
		samples := []rsmetrics.Sample{
			{Value: 0.10, Timestamp: now.Add(-2 * time.Minute)},
			{Value: 0.12, Timestamp: now.Add(-time.Minute)},
			{Value: 0.11, Timestamp: now},
		}
		rec, ok, unfilled, pts := r.recommendContainer(context.Background(), recommendContainerInput{
			policy:            policy,
			workload:          deploy,
			container:         container,
			cpuSamples:        samples,
			cpuEngine:         cpuEng,
			memEngine:         memEng,
			now:               now,
			minimumDataPoints: 1,
		})
		require.True(t, ok)
		assert.Greater(t, pts, 0)
		assert.Equal(t, "main", rec.Name)
		assert.True(t, unfilled, "memory arm has no samples and no hold source")
	})
}
