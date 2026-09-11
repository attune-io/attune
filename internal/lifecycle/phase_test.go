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

package lifecycle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	period := 5 * time.Minute

	tests := []struct {
		name string
		in   Input
		want Phase
	}{
		{
			name: "no tracking is idle",
			in:   Input{Now: now, ObservationPeriod: period},
			want: Idle,
		},
		{
			name: "tracked without resized-at is incomplete",
			in: Input{
				Tracked:           true,
				Now:               now,
				ObservationPeriod: period,
			},
			want: Incomplete,
		},
		{
			name: "resized-at zero is incomplete",
			in: Input{
				HasResizedAt:      true,
				Now:               now,
				ObservationPeriod: period,
			},
			want: Incomplete,
		},
		{
			name: "inside observation window",
			in: Input{
				Tracked:           true,
				HasResizedAt:      true,
				ResizedAt:         now.Add(-2 * time.Minute),
				Now:               now,
				ObservationPeriod: period,
			},
			want: Observing,
		},
		{
			name: "period elapsed is evaluating",
			in: Input{
				Tracked:           true,
				HasResizedAt:      true,
				ResizedAt:         now.Add(-6 * time.Minute),
				Now:               now,
				ObservationPeriod: period,
			},
			want: Evaluating,
		},
		{
			name: "live already original needs restore",
			in: Input{
				Tracked:             true,
				HasResizedAt:        true,
				ResizedAt:           now.Add(-6 * time.Minute),
				Now:                 now,
				ObservationPeriod:   period,
				LiveMatchesOriginal: true,
				PersistAfterSuccess: true,
			},
			want: RestorePending,
		},
		{
			name: "live original without persist is evaluating",
			in: Input{
				Tracked:             true,
				HasResizedAt:        true,
				ResizedAt:           now.Add(-6 * time.Minute),
				Now:                 now,
				ObservationPeriod:   period,
				LiveMatchesOriginal: true,
			},
			want: Evaluating,
		},
		{
			name: "observing ignores persist flags",
			in: Input{
				Tracked:             true,
				HasResizedAt:        true,
				ResizedAt:           now.Add(-time.Minute),
				Now:                 now,
				ObservationPeriod:   period,
				LiveMatchesOriginal: true,
				PersistAfterSuccess: true,
			},
			want: Observing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, Classify(tc.in))
		})
	}
}

func TestInputFromAnnotations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	period := time.Minute
	resizedAt := now.Add(-30 * time.Second)

	idle := InputFromAnnotations(false, "", now, period)
	assert.Equal(t, Idle, Classify(idle))

	incomplete := InputFromAnnotations(true, "not-a-time", now, period)
	assert.Equal(t, Incomplete, Classify(incomplete))

	observing := InputFromAnnotations(true, resizedAt.Format(time.RFC3339), now, period)
	assert.Equal(t, Observing, Classify(observing))
}

func TestSummaryDominant(t *testing.T) {
	t.Parallel()

	s := Summary{}
	assert.Equal(t, Idle, s.Dominant())
	assert.Equal(t, 0, s.Active())

	s.Add(Observing)
	s.Add(Observing)
	assert.Equal(t, Observing, s.Dominant())
	assert.Equal(t, 2, s.Active())

	s.Add(Evaluating)
	assert.Equal(t, Evaluating, s.Dominant())

	s.Add(RestorePending)
	assert.Equal(t, RestorePending, s.Dominant())

	s.Add(Incomplete)
	assert.Equal(t, Incomplete, s.Dominant())
	assert.Equal(t, 5, s.Active())

	s.Add(Idle)
	assert.Equal(t, Incomplete, s.Dominant())
	assert.Equal(t, 5, s.Active())
}
