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
