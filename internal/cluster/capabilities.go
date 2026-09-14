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

// Package cluster discovers version-best Kubernetes capabilities once
// at process start (manager and kubectl attune doctor).
package cluster

import (
	"strconv"
	"strings"
)

// FeatureInPlacePodLevelResources is the Node.Status.DeclaredFeatures
// name for in-place resize of pod-level spec.resources (KEP-5419).
const FeatureInPlacePodLevelResources = "InPlacePodLevelResourcesVerticalScaling"

// Capabilities is the cluster's version-best feature set.
// Construct only via Discover or SafeDefaults.
type Capabilities struct {
	GitVersion string
	Major      uint
	Minor      uint

	// Probes (API answered).
	PodsResize               bool // discovery: pods/resize
	PodLevelResourcesField   bool // OpenAPI PodSpec.resources (CREATE/persist)
	InPlacePodLevelResources bool // DeclaredFeatures three-way probe
	HPAScaleToZero           bool // doctor/docs/E2E only

	// Version-locked (not discoverable).
	AllowInPlaceMemoryLimitDecrease bool // GitVersion >= 1.35

	// Hooks. Discover leaves these false until the feature is Beta + E2E.
	SchedulerResizePreemption bool
	MemoryBackedVolumeResize  bool
	ExclusiveCPUInPlace       bool
}

// SafeDefaults is only for an unusable ServerVersion. All probes are
// false and memory-limit decrease stays clamped.
func SafeDefaults() *Capabilities {
	return &Capabilities{}
}

// AllowsInPlaceMemoryLimitDecrease reports whether GitVersion
// (e.g. "v1.35.0") is at least Kubernetes 1.35.
func AllowsInPlaceMemoryLimitDecrease(gitVersion string) bool {
	major, minor, ok := ParseGitVersion(gitVersion)
	if !ok {
		return false
	}
	return atLeast(major, minor, 1, 35)
}

// ParseGitVersion extracts major and minor from a GitVersion string.
func ParseGitVersion(gitVersion string) (major, minor uint, ok bool) {
	v := strings.TrimSpace(gitVersion)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return 0, 0, false
	}
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj64, err1 := strconv.ParseUint(parts[0], 10, 32)
	min64, err2 := strconv.ParseUint(parts[1], 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uint(maj64), uint(min64), true
}

func atLeast(major, minor, wantMajor, wantMinor uint) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}
