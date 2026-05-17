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

// Package throttle defines shared interfaces for CPU throttle checking
// to avoid import cycles between metrics and safety packages.
package throttle

import (
	"context"
	"time"
)

// Checker queries Prometheus for CPU throttle ratio.
type Checker interface {
	// GetThrottleRatio returns the CPU throttle ratio (0.0-1.0) for a container
	// at the given timestamp. Returns 0.0 if data is unavailable.
	GetThrottleRatio(ctx context.Context, namespace, pod, container string, ts time.Time) (float64, error)
}
