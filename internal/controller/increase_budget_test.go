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
)

func TestIncreaseRateBucket_DrawAndRefill(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	b := newIncreaseRateBucket(1000, -1, now)

	assert.True(t, b.tryDraw(600, 0, now), "full bucket must allow 600m")
	assert.False(t, b.tryDraw(500, 0, now), "remaining 400m must reject 500m")
	assert.True(t, b.tryDraw(400, 0, now), "exact remaining must pass")

	later := now.Add(30 * time.Second)
	assert.False(t, b.tryDraw(600, 0, later), "30s refill is 500m, not enough for 600m")
	assert.True(t, b.tryDraw(500, 0, later), "30s of 1000m/min is 500m")

	full := later.Add(time.Minute)
	assert.True(t, b.tryDraw(1000, 0, full), "after a minute the bucket is full again")
	assert.False(t, b.tryDraw(1, 0, full), "burst cannot exceed one minute of rate")
}

func TestIncreaseRateBucket_Refund(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	b := newIncreaseRateBucket(1000, 1<<30, now)
	assert.True(t, b.tryDraw(800, 100, now))
	b.refund(800, 100)
	assert.True(t, b.tryDraw(1000, 1<<30, now), "refund must restore tokens up to cap")
}

func TestIncreaseRateBucket_UnlimitedResource(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	b := newIncreaseRateBucket(-1, 100, now)
	assert.True(t, b.tryDraw(1<<40, 50, now))
	assert.False(t, b.tryDraw(0, 60, now))
}

func TestIncreaseRateBucket_BothRatesDoNotStarveCPU(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	// Memory rate is large (bytes/min) so 1ms already refills mem.
	// CPU is 1000m/min, so it needs a full second for ~16m.
	b := newIncreaseRateBucket(1000, 1<<30, now)
	assert.True(t, b.tryDraw(1000, 1<<30, now))
	tick := now
	for i := 0; i < 60; i++ {
		tick = tick.Add(time.Second)
		b.tryDraw(0, 0, tick)
	}
	assert.True(t, b.tryDraw(960, 0, tick), "CPU must refill across mem-driven 1s ticks")
}

func TestIncreaseRateBucket_LongIdleFillsToCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	b := newIncreaseRateBucket(1000, 1<<40, now)
	assert.True(t, b.tryDraw(1000, 1<<40, now))
	later := now.Add(365 * 24 * time.Hour)
	assert.True(t, b.tryDraw(1000, 1<<40, later), "year-long idle must refill to cap, not overflow")
}
