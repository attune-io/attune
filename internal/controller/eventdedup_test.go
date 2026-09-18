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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEventDedup_SuppressesDuplicates(t *testing.T) {
	d := newEventDedup(time.Hour)
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "first should emit")
	assert.False(t, d.shouldEmit("policy1/HPAConflict/msg"), "duplicate within TTL should suppress")
	assert.True(t, d.shouldEmit("policy1/VPAConflict/msg"), "different reason should emit")
	assert.True(t, d.shouldEmit("policy2/HPAConflict/msg"), "different policy should emit")
}

func TestEventDedup_ReEmitsAfterTTL(t *testing.T) {
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	d := newEventDedup(time.Second)
	d.now = func() time.Time { return now }
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "first should emit")
	now = now.Add(2 * time.Second)
	assert.True(t, d.shouldEmit("policy1/HPAConflict/msg"), "should re-emit after TTL")
}

func TestEventDedup_PrunesExpiredEntries(t *testing.T) {
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	d := newEventDedup(time.Second)
	d.now = func() time.Time { return now }

	// Insert 5 entries that will expire.
	for i := 0; i < 5; i++ {
		d.shouldEmit(fmt.Sprintf("expired-%d", i))
	}
	now = now.Add(2 * time.Second)

	// Add entries up to the 1000-call sweep threshold.
	for i := 5; i < 999; i++ {
		d.shouldEmit(fmt.Sprintf("filler-%d", i))
	}
	// At call 1000, the sweep should remove the 5 expired entries.
	d.shouldEmit("trigger-sweep")

	d.mu.Lock()
	for i := 0; i < 5; i++ {
		_, exists := d.seen[fmt.Sprintf("expired-%d", i)]
		assert.False(t, exists, "expired entry %d should have been pruned", i)
	}
	// Recent entries should still exist.
	_, exists := d.seen["trigger-sweep"]
	assert.True(t, exists, "recent entry should survive pruning")
	d.mu.Unlock()
}

func TestEventDedup_ConcurrentAccess(t *testing.T) {
	d := newEventDedup(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				d.shouldEmit(fmt.Sprintf("goroutine-%d", j))
			}
		}()
	}
	wg.Wait()
}

// ---------- Throttle integration ----------

// mockThrottleCollector extends mockCollector with ThrottleChecker.
type mockThrottleCollector struct {
	mockCollector
	throttleRatio float64
}
