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

// Package conflict detects conflicts with VPA, HPA, and other
// RightSizePolicies that could interfere with resize operations.
package conflict

import (
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConflictType identifies the kind of resource that conflicts with a
// RightSizePolicy.
type ConflictType string

const (
	// ConflictVPA indicates a VerticalPodAutoscaler targets the same workload.
	ConflictVPA ConflictType = "VPA"
	// ConflictHPA indicates a HorizontalPodAutoscaler targets the same workload.
	ConflictHPA ConflictType = "HPA"
	// ConflictPolicy indicates another RightSizePolicy targets the same workload.
	ConflictPolicy ConflictType = "RightSizePolicy"
)

// Conflict describes a single detected conflict.
type Conflict struct {
	Type    ConflictType
	Name    string
	Message string
}

// Detector checks for conflicts on target workloads.
type Detector struct {
	logger logr.Logger
}

// NewDetector creates a Detector with the given logger.
func NewDetector(logger logr.Logger) *Detector {
	return &Detector{
		logger: logger,
	}
}

// CheckAnnotationOptOut returns true if the object carries the annotation
// "rightsize.io/skip" set to "true", indicating that the workload has opted
// out of automatic right-sizing.
func (d *Detector) CheckAnnotationOptOut(obj metav1.ObjectMeta) bool {
	if obj.Annotations == nil {
		return false
	}
	return obj.Annotations["rightsize.io/skip"] == "true"
}

// CheckActiveRollout returns true if the deployment has an active rollout in
// progress. A rollout is considered active when UpdatedReplicas does not
// match Replicas, meaning not all pods have been updated to the latest
// revision yet.
func (d *Detector) CheckActiveRollout(deployment *appsv1.Deployment) bool {
	return deployment.Status.UpdatedReplicas != deployment.Status.Replicas
}
