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

// Package lifecycle names the in-band safety states already stored on
// pods as attune.io annotations. It does not add a second store.
package lifecycle

import "time"

// Phase is the observation lifecycle derived from tracking annotations.
type Phase string

const (
	// Idle: no tracking annotations. Cleanup already ran, or no resize yet.
	Idle Phase = "Idle"
	// Incomplete: tracked label or keys present but resized-at is missing
	// or unparseable. Keep annotations and retry next reconcile.
	Incomplete Phase = "Incomplete"
	// Observing: tracking present, observation period has not elapsed.
	Observing Phase = "Observing"
	// Evaluating: period elapsed; CheckPod / revert / restore may run.
	Evaluating Phase = "Evaluating"
	// RestorePending: live resources already match the original snapshot
	// and AfterSuccessfulResize persist still needs a template restore.
	RestorePending Phase = "RestorePending"
)

// Input is the annotation-derived view used by Classify.
type Input struct {
	Tracked             bool
	HasResizedAt        bool
	ResizedAt           time.Time
	Now                 time.Time
	ObservationPeriod   time.Duration
	LiveMatchesOriginal bool
	PersistAfterSuccess bool
}

// InputFromAnnotations builds an Input from the tracking label and
// attune.io/resized-at value. Live/persist flags stay false; callers
// that already know those set them on the returned Input.
func InputFromAnnotations(tracked bool, resizedAt string, now time.Time, period time.Duration) Input {
	in := Input{
		Tracked:           tracked,
		Now:               now,
		ObservationPeriod: period,
	}
	if resizedAt == "" {
		return in
	}
	in.HasResizedAt = true
	t, err := time.Parse(time.RFC3339, resizedAt)
	if err != nil {
		return in
	}
	in.ResizedAt = t
	return in
}

// Classify maps stored tracking annotations to a named phase.
func Classify(in Input) Phase {
	if !in.Tracked && !in.HasResizedAt {
		return Idle
	}
	if !in.HasResizedAt || in.ResizedAt.IsZero() {
		return Incomplete
	}
	if in.Now.Sub(in.ResizedAt) < in.ObservationPeriod {
		return Observing
	}
	if in.PersistAfterSuccess && in.LiveMatchesOriginal {
		return RestorePending
	}
	return Evaluating
}

// Summary counts pods in each safety phase for a policy condition.
type Summary struct {
	Observing      int
	Evaluating     int
	RestorePending int
	Incomplete     int
}

// Add increments the matching summary bucket.
func (s *Summary) Add(p Phase) {
	switch p {
	case Observing:
		s.Observing++
	case Evaluating:
		s.Evaluating++
	case RestorePending:
		s.RestorePending++
	case Incomplete:
		s.Incomplete++
	}
}

// Dominant is the user-visible reason for the SafetyObservation condition.
// Restore and incomplete win over observing so a stuck retry is not hidden
// behind a still-warming sibling.
func (s Summary) Dominant() Phase {
	switch {
	case s.Incomplete > 0:
		return Incomplete
	case s.RestorePending > 0:
		return RestorePending
	case s.Evaluating > 0:
		return Evaluating
	case s.Observing > 0:
		return Observing
	default:
		return Idle
	}
}

// Active is the number of pods still in the observation lifecycle.
func (s Summary) Active() int {
	return s.Observing + s.Evaluating + s.RestorePending + s.Incomplete
}
