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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadStatus returns the per-app canary row, or nil.
func (cs *CanaryStatus) WorkloadStatus(name string) *CanaryWorkloadStatus {
	if cs == nil || name == "" {
		return nil
	}
	for i := range cs.Workloads {
		if cs.Workloads[i].Workload == name {
			return &cs.Workloads[i]
		}
	}
	return nil
}

func podInList(pods []string, name string) bool {
	if name == "" {
		return false
	}
	for _, p := range pods {
		if p == name {
			return true
		}
	}
	return false
}

// WorkloadPromoted reports whether this app has reached FullRollout.
func (cs *CanaryStatus) WorkloadPromoted(workload string) bool {
	if cs == nil {
		return false
	}
	if ws := cs.WorkloadStatus(workload); ws != nil {
		return ws.Phase == CanaryPhaseFullRollout
	}
	// Per-app rows exist: an unlisted app has not been watched yet.
	if len(cs.Workloads) > 0 {
		return false
	}
	return cs.Phase == CanaryPhaseFullRollout
}

// AllowsCreateSizing is true when a new pod may receive recommended
// requests at CREATE. Canary apps stay at template size until that app
// is promoted, or the pod is already in the watched canary slice.
func (cs *CanaryStatus) AllowsCreateSizing(workload, podName string) bool {
	if cs == nil {
		return false
	}
	if cs.WorkloadPromoted(workload) {
		return true
	}
	if ws := cs.WorkloadStatus(workload); ws != nil {
		return podInList(ws.Pods, podName)
	}
	return podInList(cs.Pods, podName)
}

// AllowsStartupBoost matches CREATE: only the canary slice or a
// promoted app.
func (cs *CanaryStatus) AllowsStartupBoost(workload, podName string) bool {
	return cs.AllowsCreateSizing(workload, podName)
}

// AllowsHPARetune is true only after this app's own canary has
// promoted. HPA is Deployment-wide, so a canary-only resize must not
// rewrite the fleet target.
func (cs *CanaryStatus) AllowsHPARetune(workload string) bool {
	return cs.WorkloadPromoted(workload)
}

// UpsertWorkload returns the per-app row, creating it if needed.
func (cs *CanaryStatus) UpsertWorkload(name string) *CanaryWorkloadStatus {
	if cs == nil || name == "" {
		return nil
	}
	if ws := cs.WorkloadStatus(name); ws != nil {
		return ws
	}
	cs.Workloads = append(cs.Workloads, CanaryWorkloadStatus{
		Workload: name,
		Phase:    CanaryPhaseInProgress,
	})
	return &cs.Workloads[len(cs.Workloads)-1]
}

// RollupPhase sets policy Phase to FullRollout only when every listed
// app is promoted. Empty Workloads leaves Phase unchanged.
func (cs *CanaryStatus) RollupPhase() {
	if cs == nil || len(cs.Workloads) == 0 {
		return
	}
	for i := range cs.Workloads {
		if cs.Workloads[i].Phase != CanaryPhaseFullRollout {
			cs.Phase = CanaryPhaseInProgress
			return
		}
	}
	cs.Phase = CanaryPhaseFullRollout
}

// SyncRollupClock copies the earliest live per-app watch onto the
// policy-wide StartTime/Pods fields. Clears them when no app is watching.
func (cs *CanaryStatus) SyncRollupClock() {
	if cs == nil || len(cs.Workloads) == 0 {
		return
	}
	var earliest *metav1.Time
	var pods []string
	for i := range cs.Workloads {
		ws := &cs.Workloads[i]
		if ws.StartTime != nil {
			if earliest == nil || ws.StartTime.Before(earliest) {
				t := *ws.StartTime
				earliest = &t
			}
		}
		pods = append(pods, ws.Pods...)
	}
	cs.StartTime = earliest
	cs.Pods = pods
}
