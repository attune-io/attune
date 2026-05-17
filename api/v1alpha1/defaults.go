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

import "k8s.io/apimachinery/pkg/api/resource"

// Update strategy mode aliases for backward compatibility.
const (
	ModeRecommend = UpdateModeRecommend
	ModeObserve   = UpdateModeObserve
	ModeOneShot   = UpdateModeOneShot
	ModeCanary    = UpdateModeCanary
	ModeAuto      = UpdateModeAuto
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
)

// Default values for RightSizePolicy fields. These are the single source
// of truth, referenced by the webhook defaulter, mergeDefaults, and
// computeRecommendations.
const (
	DefaultCPUPercentile          int32 = 95
	DefaultCPUSafetyMargin              = "1.2"
	DefaultMemoryPercentile       int32 = 99
	DefaultMemorySafetyMargin           = "1.3"
	DefaultUpdateMode                   = UpdateModeRecommend
	DefaultMaxCPUChangePercent    int32 = 50
	DefaultMaxMemoryChangePercent int32 = 30
	DefaultWeight                 int32 = 100
	DefaultControlledValues             = ControlledRequestsOnly
	DefaultHistoryWindow                = "168h"
	DefaultCooldown                     = "1h"
)

// Default resource bounds applied when a policy does not specify explicit bounds.
// These are package-level vars (parsed once at init) rather than inline
// MustParse calls in the reconciler hot path.
var (
	DefaultCPUBoundsMin    = resource.MustParse("50m")
	DefaultCPUBoundsMax    = resource.MustParse("4000m")
	DefaultMemoryBoundsMin = resource.MustParse("64Mi")
	DefaultMemoryBoundsMax = resource.MustParse("8Gi")
)
