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
	"sync"
	"time"
)

// increaseRateBucket is a wall-clock token bucket for resize increases.
// Rate is tokens per minute. Capacity is one minute of rate so a burst
// cannot exceed that minute. -1 rate means that resource is unlimited.
type increaseRateBucket struct {
	mu      sync.Mutex
	cpuLast time.Time
	memLast time.Time
	cpu     int64
	mem     int64
	cpuRate int64
	memRate int64
	cpuCap  int64
	memCap  int64
}

func newIncreaseRateBucket(cpuRatePerMin, memRatePerMin int64, now time.Time) *increaseRateBucket {
	b := &increaseRateBucket{
		cpuLast: now,
		memLast: now,
		cpuRate: cpuRatePerMin,
		memRate: memRatePerMin,
		cpuCap:  cpuRatePerMin,
		memCap:  memRatePerMin,
		cpu:     cpuRatePerMin,
		mem:     memRatePerMin,
	}
	if cpuRatePerMin < 0 {
		b.cpuCap, b.cpu = -1, -1
	}
	if memRatePerMin < 0 {
		b.memCap, b.mem = -1, -1
	}
	return b
}

func refillResource(tokens, rate, cap int64, last time.Time, now time.Time) (int64, time.Time) {
	if rate < 0 {
		return tokens, last
	}
	if !now.After(last) {
		if now.Before(last) {
			return tokens, now
		}
		return tokens, last
	}
	elapsed := now.Sub(last)
	// Cap is one minute of rate. Longer idle just fills the bucket
	// and avoids rate*elapsed overflowing int64.
	if elapsed >= time.Minute {
		return cap, now
	}
	add := rate * elapsed.Milliseconds() / 60000
	if add <= 0 {
		return tokens, last
	}
	tokens += add
	if tokens > cap {
		tokens = cap
	}
	// Advance last only by the whole tokens produced so leftover
	// milliseconds carry into the next tick.
	consumedMs := add * 60000 / rate
	return tokens, last.Add(time.Duration(consumedMs) * time.Millisecond)
}

func (b *increaseRateBucket) refillLocked(now time.Time) {
	b.cpu, b.cpuLast = refillResource(b.cpu, b.cpuRate, b.cpuCap, b.cpuLast, now)
	b.mem, b.memLast = refillResource(b.mem, b.memRate, b.memCap, b.memLast, now)
}

func (b *increaseRateBucket) tryDraw(cpuMilli, memBytes int64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	if (b.cpuRate >= 0 && cpuMilli > b.cpu) || (b.memRate >= 0 && memBytes > b.mem) {
		return false
	}
	if b.cpuRate >= 0 {
		b.cpu -= cpuMilli
	}
	if b.memRate >= 0 {
		b.mem -= memBytes
	}
	return true
}

func (b *increaseRateBucket) refund(cpuMilli, memBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cpuRate >= 0 {
		b.cpu += cpuMilli
		if b.cpu > b.cpuCap {
			b.cpu = b.cpuCap
		}
	}
	if b.memRate >= 0 {
		b.mem += memBytes
		if b.mem > b.memCap {
			b.mem = b.memCap
		}
	}
}
