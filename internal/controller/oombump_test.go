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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func qtyBytes(t *testing.T, raw string) int64 {
	t.Helper()
	q, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return q.Value()
}

func i64ptr(v int64) *int64 { return &v }

func TestOOMBumpBytes(t *testing.T) {
	t.Parallel()
	mi512 := qtyBytes(t, "512Mi")
	mi200 := qtyBytes(t, "200Mi")
	mi100 := qtyBytes(t, "100Mi")
	mi300 := qtyBytes(t, "300Mi")
	mi250 := qtyBytes(t, "250Mi")
	mi320 := qtyBytes(t, "320Mi")

	tests := []struct {
		name    string
		origin  int64
		min     int64
		live    int64
		count   int
		max     *int64
		want    int64
		result  string
		wantErr bool
	}{
		{
			name:   "512Mi times 1.2 ceils to a byte",
			origin: mi512, min: mi100, live: mi512, count: 1,
			want: 644245095, result: oomBumpApplied,
		},
		{
			name:   "min bump beats a smaller ratio step",
			origin: mi200, min: mi100, live: mi200, count: 1,
			want: mi300, result: oomBumpApplied,
		},
		{
			name:   "maxAllowed below the step clamps",
			origin: mi200, min: mi100, live: mi200, count: 1,
			max: i64ptr(mi250), want: mi250, result: oomBumpClamped,
		},
		{
			name:   "maxAllowed above the step is not a clamp",
			origin: mi200, min: mi100, live: mi200, count: 1,
			max: i64ptr(mi320), want: mi300, result: oomBumpApplied,
		},
		{
			name:   "ceiling at or below the live request skips",
			origin: mi200, min: mi100, live: mi300, count: 1,
			max: i64ptr(mi250), want: mi250, result: oomBumpSkipped,
		},
		{
			name:   "second step multiplies the original request",
			origin: mi512, min: mi100, live: 644245095, count: 2,
			want: 773094114, result: oomBumpApplied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, result, err := oomBumpBytes(tt.origin, tt.min, tt.live, "1.2", tt.count, tt.max)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.result, result)
		})
	}
}

func TestOOMSignalIsNew(t *testing.T) {
	t.Parallel()
	oomAt := time.Date(2026, 9, 29, 12, 4, 1, 0, time.UTC)
	stored := &oomBumpRecord{OOMAt: oomAt, Restart: 4}

	assert.False(t, oomSignalIsNew(time.Time{}, 1, nil), "zero finishedAt with no annotation")
	assert.True(t, oomSignalIsNew(oomAt, 1, nil))
	assert.False(t, oomSignalIsNew(oomAt, 4, stored), "same finishedAt and restart")
	assert.False(t, oomSignalIsNew(oomAt.Add(-time.Second), 5, stored))
	assert.True(t, oomSignalIsNew(oomAt.Add(time.Second), 5, stored))
	assert.True(t, oomSignalIsNew(time.Time{}, 5, stored), "zero finishedAt with a higher restart")
	assert.False(t, oomSignalIsNew(time.Time{}, 4, stored))

	zeroStored := &oomBumpRecord{Restart: 4}
	filled := oomAt
	assert.False(t, oomSignalIsNew(filled, 4, zeroStored), "timestamp fill is not a second bump")
	assert.True(t, oomTimestampFilled(filled, 4, zeroStored))
	assert.True(t, oomSignalIsNew(filled, 5, zeroStored))
}

func TestContainerOOMSignal(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 9, 29, 12, 4, 1, 0, time.UTC)
	waiting := &corev1.ContainerStatus{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     oomKilledReason,
			FinishedAt: metav1.NewTime(finished),
		}},
		RestartCount: 4,
	}
	got, restart, ok := containerOOMSignal(waiting)
	assert.True(t, ok)
	assert.True(t, got.Equal(finished))
	assert.Equal(t, int32(4), restart)

	completed := &corev1.ContainerStatus{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error",
		}},
	}
	_, _, ok = containerOOMSignal(completed)
	assert.False(t, ok)

	current := &corev1.ContainerStatus{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     oomKilledReason,
			FinishedAt: metav1.NewTime(finished),
		}},
		RestartCount: 2,
	}
	got, _, ok = containerOOMSignal(current)
	assert.True(t, ok)
	assert.True(t, got.Equal(finished))
}

func TestProposeOOMBump(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	oomAt := now.Add(-time.Minute)
	mi512 := qtyBytes(t, "512Mi")
	mi100 := qtyBytes(t, "100Mi")
	mi200 := qtyBytes(t, "200Mi")
	floor1 := int64(644245095)
	floor2 := int64(773094114)

	first := proposeOOMBump(oomBumpInput{
		LiveBytes: mi512, HasPercentile: true, PercentileBytes: mi200,
		Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		Now: now, Hold: 24 * time.Hour, NewOOM: true, FinishedAt: oomAt, Restart: 4,
	})
	require.NotNil(t, first.Stamp)
	assert.Equal(t, oomBumpApplied, first.Result)
	assert.Equal(t, floor1, first.PublishBytes)
	assert.Equal(t, 1, first.Stamp.Count)
	assert.Equal(t, mi512, first.Stamp.Origin)
	assert.Equal(t, floor1, first.Stamp.Floor)
	assert.True(t, first.Stamp.OOMAt.Equal(oomAt))
	assert.True(t, first.Stamp.HoldUntil.Equal(now.Add(24*time.Hour)))

	secondStored := *first.Stamp
	second := proposeOOMBump(oomBumpInput{
		LiveBytes: floor1, HasPercentile: true, PercentileBytes: mi200,
		Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		Now: now, Hold: 24 * time.Hour, NewOOM: true,
		FinishedAt: oomAt.Add(time.Minute), Restart: 5, Stored: &secondStored,
	})
	require.NotNil(t, second.Stamp)
	assert.Equal(t, oomBumpApplied, second.Result)
	assert.Equal(t, 2, second.Stamp.Count)
	assert.Equal(t, mi512, second.Stamp.Origin)
	assert.Equal(t, floor2, second.Stamp.Floor)
	assert.NotEqual(t, int64(644245095), second.Stamp.Floor)

	cappedStored := oomBumpRecord{
		Count: 3, Origin: mi512, Floor: floor2, OOMAt: oomAt, Restart: 6,
		HoldUntil: now.Add(time.Hour),
	}
	capped := proposeOOMBump(oomBumpInput{
		LiveBytes: floor2, HasPercentile: true, PercentileBytes: mi200,
		Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		Now: now, Hold: 24 * time.Hour, NewOOM: true,
		FinishedAt: oomAt.Add(2 * time.Minute), Restart: 7, Stored: &cappedStored,
	})
	assert.Equal(t, oomBumpCapped, capped.Result)
	require.NotNil(t, capped.Stamp)
	assert.True(t, capped.AnnotationOnly)
	assert.Equal(t, 3, capped.Stamp.Count)
	assert.Equal(t, mi512, capped.Stamp.Origin)
	assert.Equal(t, floor2, capped.Stamp.Floor)
	assert.True(t, capped.Stamp.OOMAt.Equal(oomAt.Add(2*time.Minute)))
	assert.Equal(t, int32(7), capped.Stamp.Restart)
	assert.True(t, capped.Stamp.HoldUntil.Equal(cappedStored.HoldUntil))
	assert.Equal(t, floor2, capped.PublishBytes)

	mi800 := qtyBytes(t, "800Mi")
	afterHold := proposeOOMBump(oomBumpInput{
		LiveBytes: floor2, HasPercentile: true, PercentileBytes: mi800,
		Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		Now: cappedStored.HoldUntil, Hold: 24 * time.Hour, NewOOM: true,
		FinishedAt: oomAt.Add(3 * time.Minute), Restart: 8, Stored: &cappedStored,
	})
	assert.Equal(t, oomBumpCapped, afterHold.Result)
	require.NotNil(t, afterHold.Stamp)
	assert.True(t, afterHold.AnnotationOnly)
	assert.Equal(t, 3, afterHold.Stamp.Count)
	assert.Equal(t, floor2, afterHold.Stamp.Floor)
	assert.True(t, afterHold.Stamp.HoldUntil.Equal(cappedStored.HoldUntil))
	assert.Equal(t, mi800, afterHold.PublishBytes)

	held := proposeOOMBump(oomBumpInput{
		LiveBytes: floor1, HasPercentile: true, PercentileBytes: mi200,
		Now: now, Stored: &secondStored,
	})
	assert.Empty(t, held.Result)
	assert.Nil(t, held.Stamp)
	assert.Equal(t, floor1, held.PublishBytes)

	after := proposeOOMBump(oomBumpInput{
		LiveBytes: floor1, HasPercentile: true, PercentileBytes: mi200,
		Now: secondStored.HoldUntil, Stored: &secondStored,
	})
	assert.Equal(t, mi200, after.PublishBytes)
	assert.Nil(t, after.Stamp)
	assert.Equal(t, mi512, secondStored.Origin)

	same := proposeOOMBump(oomBumpInput{
		LiveBytes: floor1, HasPercentile: true, PercentileBytes: mi200,
		Now: now, FinishedAt: secondStored.OOMAt, Restart: secondStored.Restart,
		Stored: &secondStored,
	})
	assert.Nil(t, same.Stamp)
	assert.Equal(t, floor1, same.PublishBytes)

	excluded := proposeOOMBump(oomBumpInput{
		LiveBytes: mi512, Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		Now: now, Hold: time.Hour, NewOOM: true, FinishedAt: oomAt, Restart: 1,
		Excluded: true,
	})
	assert.Equal(t, oomBumpSkipped, excluded.Result)
	assert.Nil(t, excluded.Stamp)

	quiet := proposeOOMBump(oomBumpInput{
		LiveBytes: mi512, HasPercentile: true, PercentileBytes: mi200, Now: now,
	})
	assert.Empty(t, quiet.Result)
	assert.Nil(t, quiet.Stamp)
	assert.Equal(t, mi200, quiet.PublishBytes)

	mi250 := qtyBytes(t, "250Mi")
	clamped := proposeOOMBump(oomBumpInput{
		LiveBytes: mi200, Ratio: "1.2", MinBumpBytes: mi100, MaxBumps: 3,
		MaxAllowed: i64ptr(mi250), Now: now, Hold: time.Hour,
		NewOOM: true, FinishedAt: oomAt, Restart: 1,
	})
	require.NotNil(t, clamped.Stamp)
	assert.Equal(t, oomBumpClamped, clamped.Result)
	assert.Equal(t, mi250, clamped.Stamp.Floor)
	assert.Equal(t, 1, clamped.Stamp.Count)

	zeroStored := oomBumpRecord{
		Count: 1, Origin: mi512, Floor: floor1, Restart: 4,
		HoldUntil: now.Add(time.Hour),
	}
	filled := proposeOOMBump(oomBumpInput{
		LiveBytes: floor1, HasPercentile: true, PercentileBytes: mi200,
		Now: now, FinishedAt: oomAt, Restart: 4, Stored: &zeroStored,
	})
	require.NotNil(t, filled.Stamp)
	assert.True(t, filled.AnnotationOnly)
	assert.Empty(t, filled.Result)
	assert.Equal(t, 1, filled.Stamp.Count)
	assert.True(t, filled.Stamp.OOMAt.Equal(oomAt))
	assert.Equal(t, floor1, filled.PublishBytes)
}

func TestOOMBumpAnnotationRoundTrip(t *testing.T) {
	t.Parallel()
	rec := oomBumpRecord{
		Count: 2, Origin: qtyBytes(t, "512Mi"), Floor: 773094114,
		OOMAt:     time.Date(2026, 9, 29, 12, 4, 1, 0, time.UTC),
		Restart:   4,
		HoldUntil: time.Date(2026, 9, 30, 12, 4, 1, 0, time.UTC),
	}
	raw, err := formatOOMBumpRecord(rec)
	require.NoError(t, err)
	assert.NotContains(t, raw, " ")
	parsed, ok := parseOOMBumpRecord(raw)
	require.True(t, ok)
	assert.Equal(t, rec.Count, parsed.Count)
	assert.Equal(t, rec.Origin, parsed.Origin)
	assert.Equal(t, rec.Floor, parsed.Floor)
	assert.Equal(t, rec.Restart, parsed.Restart)
	assert.True(t, rec.OOMAt.Equal(parsed.OOMAt))
	assert.True(t, rec.HoldUntil.Equal(parsed.HoldUntil))
	_, ok = parseOOMBumpRecord(strings.ReplaceAll(raw, ",", ", "))
	assert.False(t, ok)
}

func TestOOMBumpKey(t *testing.T) {
	t.Parallel()
	key, ok := oomBumpKey("app")
	assert.True(t, ok)
	assert.Equal(t, "attune.io/oom-bump.app", key)

	_, ok = oomBumpKey("")
	assert.False(t, ok)
	_, ok = oomBumpKey("a/b")
	assert.False(t, ok)

	longOK := strings.Repeat("a", 54)
	_, ok = oomBumpKey(longOK)
	assert.True(t, ok, "oom-bump. plus 54 characters is 63")
	_, ok = oomBumpKey(strings.Repeat("a", 55))
	assert.False(t, ok)
}

func TestHighestHeldBump(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	records := []oomBumpRecord{
		{Count: 1, Floor: 100, HoldUntil: now.Add(-time.Minute)},
		{Count: 2, Floor: 300, Origin: 200, HoldUntil: now.Add(time.Hour)},
		{Count: 2, Floor: 250, HoldUntil: now.Add(time.Hour)},
	}
	got, ok := highestHeldBump(records, now)
	require.True(t, ok)
	assert.Equal(t, 2, got.Count)
	assert.Equal(t, int64(300), got.Floor)
	assert.Equal(t, int64(200), got.Origin)
}

func TestCriticalBumpAction(t *testing.T) {
	t.Parallel()
	resized := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	trigger := resized.Add(-time.Second)
	now := resized.Add(time.Minute)
	stored := &oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "512Mi"), Floor: 644245095,
		OOMAt: trigger, Restart: 2, HoldUntil: resized.Add(24 * time.Hour),
	}
	triggerStatus := &corev1.ContainerStatus{
		RestartCount: 2,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: oomKilledReason, FinishedAt: metav1.NewTime(trigger),
		}},
	}
	suppress, step, capped := criticalBumpAction("oomkill", resized, stored, now, triggerStatus, 3)
	assert.True(t, suppress, "the trigger OOM is already stored")
	assert.False(t, step)
	assert.False(t, capped)

	later := resized.Add(30 * time.Second)
	twoOOM := &corev1.ContainerStatus{
		RestartCount: 4,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: oomKilledReason, FinishedAt: metav1.NewTime(resized.Add(40 * time.Second)),
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: oomKilledReason, FinishedAt: metav1.NewTime(later),
		}},
	}
	suppress, step, capped = criticalBumpAction("restart", resized, stored, now, twoOOM, 3)
	assert.True(t, suppress)
	assert.True(t, step)
	assert.False(t, capped)

	errored := &corev1.ContainerStatus{
		RestartCount: 4,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", FinishedAt: metav1.NewTime(later),
		}},
	}
	suppress, step, capped = criticalBumpAction("restart", resized, stored, now, errored, 3)
	assert.False(t, suppress)
	assert.False(t, step)
	assert.False(t, capped)

	// RestartCount is high enough for a restart verdict, but the only
	// termination is the pre-resize trigger. That is not a new death.
	preResizeOnly := &corev1.ContainerStatus{
		RestartCount: 4,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: oomKilledReason, FinishedAt: metav1.NewTime(trigger),
		}},
	}
	suppress, step, capped = criticalBumpAction("restart", resized, stored, now, preResizeOnly, 3)
	assert.True(t, suppress, "restart with only the stored trigger stays inside the hold")
	assert.False(t, step)
	assert.False(t, capped)

	suppress, _, _ = criticalBumpAction("oomkill", resized, stored, stored.HoldUntil, triggerStatus, 3)
	assert.False(t, suppress, "hold expiry uses the normal revert path")

	floor, ok := activeBumpFloor(stored, now)
	assert.True(t, ok)
	assert.Equal(t, stored.Floor, floor)
	_, ok = activeBumpFloor(stored, stored.HoldUntil)
	assert.False(t, ok)
}
