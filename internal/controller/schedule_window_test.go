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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestIsWithinResizeWindow_NoSchedule(t *testing.T) {
	assert.True(t, isWithinResizeWindow(nil, time.Now()))
}

func TestIsWithinResizeWindow_DayOfWeek(t *testing.T) {
	// Wednesday 10:00 UTC
	wed := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)
	schedule := &attunev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
	}
	assert.True(t, isWithinResizeWindow(schedule, wed))

	// Thursday should be blocked
	thu := time.Date(2026, 1, 8, 10, 0, 0, 0, time.UTC)
	assert.False(t, isWithinResizeWindow(schedule, thu))
}

func TestIsWithinResizeWindow_TimeWindow(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows: []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
	}
	// 03:00 is inside
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_OvernightWindow(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows: []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
	}
	// 23:00 is inside (after start)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 23, 0, 0, 0, time.UTC)))
	// 03:00 is inside (before end, wraps past midnight)
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC)))
	// 10:00 is outside
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_OvernightWindowWithDayOfWeek(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
		DaysOfWeek: []string{"Wednesday"},
	}
	// Wed 23:00: pre-midnight portion, today is Wednesday -> allowed
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 23, 0, 0, 0, time.UTC)))
	// Thu 03:00: post-midnight portion, window opened on Wednesday -> allowed
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 8, 3, 0, 0, 0, time.UTC)))
	// Thu 23:00: pre-midnight portion, today is Thursday (not in list) -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 8, 23, 0, 0, 0, time.UTC)))
	// Fri 03:00: post-midnight portion, window would have opened Thu (not in list) -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 9, 3, 0, 0, 0, time.UTC)))
	// Wed 10:00: outside the window entirely -> blocked
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)))
}

func TestIsWithinResizeWindow_InvalidTimezoneFailsOpen(t *testing.T) {
	schedule := &attunev1alpha1.ResizeSchedule{
		Timezone: "Invalid/Zone",
	}
	// Invalid timezone should fail open (allow resize)
	assert.True(t, isWithinResizeWindow(schedule, time.Now()))
}

func TestIsWithinResizeWindow_DSTSpringForward(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// 2026-03-08: clocks jump from 01:59 EST to 03:00 EDT. 02:00-03:00 never exists.
	skipped := &attunev1alpha1.ResizeSchedule{
		Windows:  []attunev1alpha1.TimeWindow{{Start: "02:00", End: "03:00"}},
		Timezone: "America/New_York",
	}
	assert.False(t, isWithinResizeWindow(skipped, time.Date(2026, 3, 8, 1, 45, 0, 0, loc)),
		"01:45 is before a 02:00-03:00 window")
	assert.False(t, isWithinResizeWindow(skipped, time.Date(2026, 3, 8, 3, 15, 0, 0, loc)),
		"03:15 is after a window that never opened")

	// 01:30-02:30 is only the 01:30-01:59 half; 03:15 is past End=02:30.
	partial := &attunev1alpha1.ResizeSchedule{
		Windows:  []attunev1alpha1.TimeWindow{{Start: "01:30", End: "02:30"}},
		Timezone: "America/New_York",
	}
	assert.True(t, isWithinResizeWindow(partial, time.Date(2026, 3, 8, 1, 45, 0, 0, loc)))
	assert.False(t, isWithinResizeWindow(partial, time.Date(2026, 3, 8, 3, 15, 0, 0, loc)))
}

func TestIsWithinResizeWindow_DSTFallBack(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// 2026-11-01: 01:00-02:00 occurs twice. Pin both via UTC so we do not
	// depend on which occurrence time.Date picks in the repeated hour.
	window := &attunev1alpha1.ResizeSchedule{
		Windows:  []attunev1alpha1.TimeWindow{{Start: "01:00", End: "02:00"}},
		Timezone: "America/New_York",
	}
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC).In(loc)  // 01:30 EDT
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC).In(loc) // 01:30 EST
	assert.Equal(t, 1, first.Hour())
	assert.Equal(t, 1, second.Hour())
	assert.True(t, isWithinResizeWindow(window, first))
	assert.True(t, isWithinResizeWindow(window, second),
		"second 01:30 after fall-back is the same local HH:MM")
	assert.False(t, isWithinResizeWindow(window, time.Date(2026, 11, 1, 2, 15, 0, 0, loc)))
}

func TestIsWithinResizeWindow_OvernightNonUTCDayBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// Monday 22:00-06:00 America/New_York. Tuesday 03:00 is the tail of Monday.
	schedule := &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
		DaysOfWeek: []string{"Monday"},
		Timezone:   "America/New_York",
	}
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 5, 23, 0, 0, 0, loc)),
		"Monday 23:00 ET is inside")
	assert.True(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 6, 3, 0, 0, 0, loc)),
		"Tuesday 03:00 ET is the Monday window tail")
	assert.False(t, isWithinResizeWindow(schedule, time.Date(2026, 1, 6, 23, 0, 0, 0, loc)),
		"Tuesday 23:00 ET opened on Tuesday")
}

func TestParseHHMM(t *testing.T) {
	assert.Equal(t, 120, parseHHMM("02:00"))
	assert.Equal(t, 1380, parseHHMM("23:00"))
	assert.Equal(t, 0, parseHHMM("00:00"))
	assert.Equal(t, -1, parseHHMM("25:00"))
	assert.Equal(t, -1, parseHHMM("bad"))
}
