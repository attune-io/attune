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

// Package recommendation provides a composable recommendation engine that
// combines percentile-based estimation, safety margins, confidence adjustments,
// bounds clamping, and change filtering into a chain of estimators.
package recommendation

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SebTardif/kube-rightsize/internal/metrics"
)

// Estimator computes a recommended resource quantity based on a usage profile
// and the current resource allocation.
type Estimator interface {
	Estimate(profile metrics.UsageProfile, current resource.Quantity) resource.Quantity
}

// scaleQuantity multiplies q by factor, preserving BinarySI vs DecimalSI format.
func scaleQuantity(q resource.Quantity, factor float64) resource.Quantity {
	if q.Format == resource.BinarySI {
		return *resource.NewQuantity(int64(math.Ceil(float64(q.Value())*factor)), resource.BinarySI)
	}
	return *resource.NewMilliQuantity(int64(math.Ceil(float64(q.MilliValue())*factor)), resource.DecimalSI)
}
