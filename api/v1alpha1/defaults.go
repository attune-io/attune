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

package v1alpha1

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Update strategy type aliases for backward compatibility.
const (
	ModeRecommend = UpdateTypeRecommend
	ModeObserve   = UpdateTypeObserve
	ModeOneShot   = UpdateTypeOneShot
	ModeCanary    = UpdateTypeCanary
	ModeAuto      = UpdateTypeAuto
)

// Controlled values options.
const (
	ControlledRequestsOnly      = "RequestsOnly"
	ControlledRequestsAndLimits = "RequestsAndLimits"
)

// Resize result aliases for backward compatibility.
const (
	ResultSuccess  = ResizeResultSuccess
	ResultFailed   = ResizeResultFailed
	ResultReverted = ResizeResultReverted
	ResultEvicted  = ResizeResultEvicted
)

// Default values for AttunePolicy fields. These are the single source
// of truth, referenced by the webhook defaulter, mergeDefaults, and
// computeRecommendations.
const (
	DefaultCPUPercentile          int32 = 95
	DefaultCPUOverhead                  = "20"
	DefaultMemoryPercentile       int32 = 99
	DefaultMemoryOverhead               = "30"
	DefaultUpdateType                   = UpdateTypeRecommend
	DefaultCPUMaxChangePercent    int32 = 50
	DefaultMemoryMaxChangePercent int32 = 30
	// DefaultDecreaseUsageMarginPercent is the default headroom above recent
	// memory usage when decreasing memory limits (see DecreaseUsageMarginPercent).
	DefaultDecreaseUsageMarginPercent int32 = 10
	DefaultWeight                     int32 = 100
	DefaultControlledValues                 = ControlledRequestsOnly
	DefaultHistoryWindow                    = "168h"
	DefaultCooldown                         = "1h"
	DefaultResizeMethod                     = ResizeMethodInPlaceOnly
	DefaultMinimumDataPoints          int32 = 48
	DefaultAutoRevert                       = true
	DefaultMaxConcurrentResizes       int32 = 1
	DefaultQueryStep                        = 5 * time.Minute
	// DefaultExcludeKnownSidecars skips well-known mesh/sidecar container
	// names unless the policy or AttuneDefaults sets excludeKnownSidecars
	// to false.
	DefaultExcludeKnownSidecars = true
	// DefaultPodAggregation is Max: max by (container) for PromQL.
	DefaultPodAggregation = "Max"
	// DefaultCloudWatchCPUUnit is the container_cpu_usage_total scale when
	// cpuUnit is empty. Nanocores divides by 1e9, which is what an omitted
	// unit did before the field existed. Millicores is opt-in.
	DefaultCloudWatchCPUUnit = "Nanocores"
	// MaxLimitMultiplier is the largest accepted limitMultiplier.
	// memoryFromCpuRatio uses 1000; a limit multiple does not need that.
	MaxLimitMultiplier = 100
	// DefaultOOMBumpRatio is used when memory.oomBump is set and ratio is nil.
	DefaultOOMBumpRatio = "1.2"
	// DefaultOOMBumpMaxBumps is used when memory.oomBump is set and maxBumps is nil.
	DefaultOOMBumpMaxBumps int32 = 3
	// DefaultOOMBumpHold is used when memory.oomBump is set and hold is nil.
	DefaultOOMBumpHold = 24 * time.Hour
	// MaxOOMBumpRatio is the largest accepted oomBump.ratio.
	MaxOOMBumpRatio = 10
	// MaxOOMBumpMaxBumps matches the CRD maximum on oomBump.maxBumps.
	MaxOOMBumpMaxBumps int32 = 10
	// MinOOMBumpHold and MaxOOMBumpHold bound oomBump.hold.
	MinOOMBumpHold = time.Minute
	MaxOOMBumpHold = 168 * time.Hour
	// DefaultSurgeTriggerRatio is used when surge is set and triggerRatio is empty.
	DefaultSurgeTriggerRatio = "1.5"
	// DefaultSurgePercentile is used when surge is set and percentile is nil.
	DefaultSurgePercentile int32 = 99
	// DefaultSurgeWindow is used when surge is set and window is nil.
	DefaultSurgeWindow = 30 * time.Minute
	// MinSurgeWindow is the shortest accepted surge.window.
	MinSurgeWindow = 5 * time.Minute
	// MaxSurgeTriggerRatio is the largest accepted surge.triggerRatio.
	MaxSurgeTriggerRatio = 100
	// DefaultMaxStatusRecommendations caps status.recommendations size.
	DefaultMaxStatusRecommendations int32 = 100
	// DefaultIncludeExplanationsInStatus keeps explanation chains in status.
	DefaultIncludeExplanationsInStatus = true
)

// KnownSidecarContainers is the curated list of container names that are
// auto-excluded when excludeKnownSidecars is true (the default). Names are
// exact matches only. Keep this list conservative: no generic names such as
// "envoy" or "proxy".
var KnownSidecarContainers = []string{
	"istio-proxy",
	"linkerd-proxy",
	"consul-dataplane",
	"kuma-dp",
	"vault-agent",
	"cloud-sql-proxy",
	"cloudsql-proxy",
	"gce-proxy",
}

// Default resource floors and ceilings applied when a policy does not
// specify minAllowed or maxAllowed. These are package-level vars (parsed
// once at init) rather than inline MustParse calls in the reconciler hot path.
// The ceilings stay in the recommendation engine. They are not written back
// onto the policy spec.
var (
	DefaultCPUBoundsMin    = resource.MustParse("1m")
	DefaultCPUBoundsMax    = resource.MustParse("4000m")
	DefaultMemoryBoundsMin = resource.MustParse("4Mi")
	DefaultMemoryBoundsMax = resource.MustParse("8Gi")
	// DefaultOOMBumpMinBump is used when memory.oomBump is set and minBump is nil.
	DefaultOOMBumpMinBump = resource.MustParse("100Mi")
)
