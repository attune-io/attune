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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// flippedSuccessRevert is the production history shape after
// markLatestCycleReverted: the Success row is flipped in place and
// keeps its original timestamp. Do not invent a later Reverted row.
func flippedSuccessRevert(workload, method string, ts time.Time, reason string) attunev1alpha1.ResizeHistoryEntry {
	return attunev1alpha1.ResizeHistoryEntry{
		Workload:  workload,
		Method:    method,
		Result:    attunev1alpha1.ResizeResultReverted,
		Reason:    reason,
		Timestamp: metav1.NewTime(ts),
	}
}
