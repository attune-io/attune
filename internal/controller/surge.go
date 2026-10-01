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
	"math"
	"strconv"
	"time"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/recommendation"
)

// surgeNote is appended to finalAdjustment when the short window is used.
const surgeNote = "surge"

// surgeChoice is the profile and percentile to feed the engine.
// fired is false when the long window stays in use.
type surgeChoice struct {
	profile    rsmetrics.UsageProfile
	percentile int
	fired      bool
}

// finiteInWindow returns finite samples inside [now-window, now].
// finiteAll counts finite samples in the whole slice. Non-finite
// samples are dropped and do not count as being outside the window.
func finiteInWindow(samples []rsmetrics.Sample, now time.Time, window time.Duration) (in []rsmetrics.Sample, finiteAll, finiteIn int) {
	if window <= 0 {
		return nil, 0, 0
	}
	start := now.Add(-window)
	in = make([]rsmetrics.Sample, 0, len(samples))
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		finiteAll++
		if !s.Timestamp.Before(start) && !s.Timestamp.After(now) {
			finiteIn++
			in = append(in, s)
		}
	}
	return in, finiteAll, finiteIn
}

// applySurge returns the profile to feed the engine.
// On fire, the profile is the short window with confidence copied from
// the long profile. Call it only after the long window has passed
// minimumDataPoints. A nil block returns the long profile.
// Empty triggerRatio means 1.5. A nil percentile means 99. A non-nil 0
// does not fire. A nil window means 30m. An explicit 0 window does not fire.
func applySurge(
	block *attunev1alpha1.Surge,
	longProfile rsmetrics.UsageProfile,
	raw []rsmetrics.Sample,
	now time.Time,
	queryStep time.Duration,
	parentPercentile int,
) surgeChoice {
	keep := surgeChoice{profile: longProfile, percentile: parentPercentile}
	if block == nil {
		return keep
	}
	ratio, ratioOK := surgeTriggerRatio(block.TriggerRatio)
	pct, pctOK := surgePercentile(block.Percentile)
	window, windowOK := surgeWindow(block)
	if !ratioOK || !pctOK || !windowOK {
		return keep
	}
	in, finiteAll, finiteIn := finiteInWindow(raw, now, window)
	// The same finite count means the fetched series was already inside
	// the short window. Missing older samples are not treated as zeros.
	if finiteIn == 0 || finiteIn >= finiteAll {
		return keep
	}
	if !shortSampleFloorMet(finiteIn, window, queryStep) {
		return keep
	}
	shortProfile := rsmetrics.BuildProfile(in)
	longVal := publishedPercentile(longProfile, parentPercentile, false)
	shortVal := publishedPercentile(shortProfile, pct, true)
	if !finiteStat(longVal) || !finiteStat(shortVal) || longVal < 0 || shortVal < 0 {
		return keep
	}
	fire := false
	if longVal > 0 {
		fire = shortVal/longVal >= ratio
	} else if shortVal > 0 {
		fire = true
	}
	if !fire {
		return keep
	}
	shortProfile.Confidence = longProfile.Confidence
	return surgeChoice{profile: shortProfile, percentile: pct, fired: true}
}

func publishedPercentile(profile rsmetrics.UsageProfile, percentile int, overallOnly bool) float64 {
	est := &recommendation.PercentileEstimator{Percentile: percentile, OverallOnly: overallOnly}
	return est.SelectedMax(profile)
}

func finiteStat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func surgeTriggerRatio(raw string) (float64, bool) {
	if raw == "" {
		raw = attunev1alpha1.DefaultSurgeTriggerRatio
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if v <= 1 || v > float64(attunev1alpha1.MaxSurgeTriggerRatio) {
		return 0, false
	}
	return v, true
}

func surgePercentile(p *int32) (int, bool) {
	if p == nil {
		return int(attunev1alpha1.DefaultSurgePercentile), true
	}
	switch *p {
	case 50, 90, 95, 99:
		return int(*p), true
	default:
		return 0, false
	}
}

func surgeWindow(block *attunev1alpha1.Surge) (time.Duration, bool) {
	if block.Window == nil {
		return attunev1alpha1.DefaultSurgeWindow, true
	}
	if block.Window.Duration <= 0 {
		return 0, false
	}
	return block.Window.Duration, true
}

// shortSampleFloorMet requires at least 3 finite points. When queryStep
// is positive it also requires half of window/queryStep. minimumDataPoints
// is not applied here.
func shortSampleFloorMet(finiteIn int, window, queryStep time.Duration) bool {
	if finiteIn < 3 {
		return false
	}
	if queryStep <= 0 {
		return true
	}
	expected := int(window / queryStep)
	return finiteIn >= expected/2
}
