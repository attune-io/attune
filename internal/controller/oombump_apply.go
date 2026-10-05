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
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// oomBumpPodStamp is one pod whose annotation may change.
// AnnotationOnly refreshes oomAt and is written even when no resize runs.
// Any other stamp is written only after UpdateResize succeeds for that pod.
// A BudgetExhausted, IncreaseExceedsBudget, or PreservesQoS skip drops it
// and the caller emits skipped instead of Result.
type oomBumpPodStamp struct {
	Namespace      string
	PodName        string
	Stamp          oomBumpRecord
	AnnotationOnly bool
	Result         string
	Event          string
	applied        bool
	// bumpBytes is the origin-math request before a higher percentile
	// raises Stamp.Floor. The stale path publishes this, not the percentile.
	bumpBytes int64
}

// oomBumpWorkloadPlan is the memory request to publish for one workload
// container. MetricNow is emitted immediately (skipped, capped). Applied and
// clamped sit on Stamps and are emitted only after a successful resize.
// A quiet reclamp is not listed in MetricNow: that would count on every
// reconcile. WorkloadValue includes planned stamps. If the caller drops a
// stamp, it must not write a workload count higher than the stamps it
// actually stored.
type oomBumpWorkloadPlan struct {
	Active        bool
	UsePublish    bool
	PublishBytes  int64
	Note          bool
	MetricNow     []string
	Event         string
	Stamps        []oomBumpPodStamp
	WorkloadValue string
	// BaseHeld is the in-hold records already stored, without this cycle's
	// planned stamps. The resize path recomputes the workload annotation
	// from BaseHeld plus the stamps it actually stored.
	BaseHeld []oomBumpRecord
	// QuietClamp is an in-hold floor lowered to maxAllowed with no fresh
	// applied or clamped stamp. The caller counts clamped once per floor
	// and cap. The stored floor is unchanged.
	QuietClamp bool
	// QuietClampFrom is PublishBytes before that clamp.
	QuietClampFrom int64
}

// oomBumpRevertDecision is how a safety verdict treats one container that
// may be inside an OOM bump hold. A zero value means today's revert.
type oomBumpRevertDecision struct {
	Suppress bool
	Step     bool
	Capped   bool
	Floor    int64
	Stored   *oomBumpRecord
}

// planWorkloadOOMBump plans one container. A nil block leaves the feature off.
// excluded with a new OOM emits skipped and does not publish a floor. A quiet
// excluded container emits nothing. percentileOK false still bumps on a new OOM.
// maxAllowed nil is uncapped. A non-nil value, including zero, is a ceiling.
// An in-hold floor above that ceiling is lowered to it. The stored floor
// and holdUntil stay. That clamp is not a new OOM. A percentile with no
// in-hold floor is left as the caller passed it.
func planWorkloadOOMBump(
	block *attunev1alpha1.OOMBump,
	container string,
	excluded bool,
	percentileBytes int64,
	percentileOK bool,
	maxAllowed *int64,
	workloadRaw string,
	pods []corev1.Pod,
	now time.Time,
	consumed []oomBumpConsumed,
) oomBumpWorkloadPlan {
	if block == nil {
		return oomBumpWorkloadPlan{}
	}
	annKey, ok := oomBumpKey(container)
	if !ok {
		return oomBumpWorkloadPlan{}
	}
	var workloadStored *oomBumpRecord
	if rec, parsed := parseOOMBumpRecord(workloadRaw); parsed {
		cp := rec
		workloadStored = &cp
	}
	snaps := snapshotOOMPods(pods, container, annKey, workloadStored)
	snaps = suppressConsumedOOMSignals(snaps, consumed)
	if excluded {
		plan := oomBumpWorkloadPlan{Active: true}
		for _, snap := range snaps {
			if snap.newOOM {
				plan.MetricNow = append(plan.MetricNow, oomBumpSkipped)
				plan.Stamps = append(plan.Stamps, skippedOOMStamp(snap, now))
				break
			}
		}
		return plan
	}
	ratio, minBump, maxBumps, hold, resolved := resolveOOMBump(block)
	if !resolved {
		plan := oomBumpWorkloadPlan{Active: true}
		for _, snap := range snaps {
			if snap.newOOM {
				plan.MetricNow = append(plan.MetricNow, oomBumpSkipped)
				plan.Stamps = append(plan.Stamps, skippedOOMStamp(snap, now))
				break
			}
		}
		return plan
	}

	plan := oomBumpWorkloadPlan{Active: true}
	if percentileOK && percentileBytes > 0 {
		plan.UsePublish = true
		plan.PublishBytes = percentileBytes
	}
	var held []oomBumpRecord
	if workloadStored != nil && now.Before(workloadStored.HoldUntil) {
		held = append(held, *workloadStored)
	}
	for _, snap := range snaps {
		if snap.stored != nil && now.Before(snap.stored.HoldUntil) {
			held = append(held, *snap.stored)
		}
	}
	var heldFloor int64
	republishHold := false
	if best, found := highestHeldBump(held, now); found {
		plan.Note = true
		heldFloor = best.Floor
		republishHold = true
		if !plan.UsePublish || best.Floor > plan.PublishBytes {
			plan.UsePublish = true
			plan.PublishBytes = best.Floor
		}
	}

	for _, snap := range snaps {
		if !snap.newOOM && !snap.fill {
			continue
		}
		stored := snap.stored
		if stored == nil && !snap.freshStart {
			stored = workloadStored
		}
		prop := proposeOOMBump(oomBumpInput{
			LiveBytes:       snap.live,
			PercentileBytes: percentileBytes,
			HasPercentile:   percentileOK && percentileBytes > 0,
			Ratio:           ratio,
			MinBumpBytes:    minBump,
			MaxBumps:        maxBumps,
			MaxAllowed:      maxAllowed,
			Now:             now,
			Hold:            hold,
			NewOOM:          snap.newOOM,
			FinishedAt:      snap.finished,
			Restart:         snap.restart,
			Stored:          stored,
		})
		if prop.AnnotationOnly && prop.Stamp != nil {
			plan.Stamps = append(plan.Stamps, oomBumpPodStamp{
				Namespace:      snap.namespace,
				PodName:        snap.name,
				Stamp:          *prop.Stamp,
				AnnotationOnly: true,
			})
			if prop.Result == oomBumpSkipped {
				plan.MetricNow = append(plan.MetricNow, oomBumpSkipped)
				continue
			}
			if prop.Result != oomBumpCapped {
				continue
			}
			plan.MetricNow = append(plan.MetricNow, oomBumpCapped)
			plan.Event = "OOMBumpCapped"
			plan.Note = true
			if prop.UsePublish && (!plan.UsePublish || prop.PublishBytes > plan.PublishBytes) {
				plan.UsePublish = true
				plan.PublishBytes = prop.PublishBytes
			}
			continue
		}
		switch prop.Result {
		case oomBumpCapped:
			plan.MetricNow = append(plan.MetricNow, oomBumpCapped)
			plan.Event = "OOMBumpCapped"
			plan.Note = true
			if prop.UsePublish && (!plan.UsePublish || prop.PublishBytes > plan.PublishBytes) {
				plan.UsePublish = true
				plan.PublishBytes = prop.PublishBytes
			}
		case oomBumpSkipped:
			plan.MetricNow = append(plan.MetricNow, oomBumpSkipped)
		case oomBumpApplied, oomBumpClamped:
			if prop.Stamp == nil {
				plan.MetricNow = append(plan.MetricNow, oomBumpSkipped)
				continue
			}
			stamp := oomBumpPodStamp{
				Namespace: snap.namespace,
				PodName:   snap.name,
				Stamp:     *prop.Stamp,
				Result:    prop.Result,
				bumpBytes: prop.Stamp.Floor,
			}
			if prop.Result == oomBumpClamped {
				stamp.Event = "OOMBumpClamped"
			}
			plan.Stamps = append(plan.Stamps, stamp)
			plan.Note = true
			if prop.UsePublish && (!plan.UsePublish || prop.PublishBytes > plan.PublishBytes) {
				plan.UsePublish = true
				plan.PublishBytes = prop.PublishBytes
			}
		}
	}

	if plan.UsePublish {
		for i := range plan.Stamps {
			if plan.Stamps[i].AnnotationOnly {
				continue
			}
			if plan.PublishBytes > plan.Stamps[i].Stamp.Floor {
				plan.Stamps[i].Stamp.Floor = plan.PublishBytes
			}
		}
	}
	plan.BaseHeld = append([]oomBumpRecord(nil), held...)
	plan.WorkloadValue = heldWorkloadValue(held, plan.Stamps, now)
	// WorkloadValue already stored the uncapped floor.
	clampQuietOOMPublish(&plan, maxAllowed, heldFloor, republishHold)
	return plan
}

// clampQuietOOMPublish lowers an in-hold floor to maxAllowed.
// A nil cap stays uncapped. The published value must be that floor, so a
// percentile the caller already settled is left alone. Stamp floors stay
// so the annotation keeps the uncapped floor. QuietClamp stays false when
// a fresh applied or clamped stamp exists, because that resize owns the
// counter.
func clampQuietOOMPublish(plan *oomBumpWorkloadPlan, maxAllowed *int64, heldFloor int64, republishHold bool) {
	if plan == nil || !republishHold || maxAllowed == nil {
		return
	}
	if !plan.UsePublish || plan.PublishBytes != heldFloor || plan.PublishBytes <= *maxAllowed {
		return
	}
	from := plan.PublishBytes
	plan.PublishBytes = *maxAllowed
	for _, st := range plan.Stamps {
		if !st.AnnotationOnly && (st.Result == oomBumpApplied || st.Result == oomBumpClamped) {
			return
		}
	}
	plan.QuietClamp = true
	plan.QuietClampFrom = from
}

// oomBumpConsumed is an OOM signal a full revert already handled.
// The same finishedAt and restart must not start another bump.
// A later OOM starts at count 1 from the live request.
type oomBumpConsumed struct {
	Namespace string
	PodName   string
	OOMAt     time.Time
	Restart   int32
}

// suppressConsumedOOMSignals clears newOOM when the visible signal is not
// strictly newer than an OOM a full revert already consumed. A newer
// signal, including a non-zero finishedAt, starts at count 1 from this
// pod's live request. A sibling workload annotation is not that basis.
func suppressConsumedOOMSignals(snaps []oomPodSnap, consumed []oomBumpConsumed) []oomPodSnap {
	if len(consumed) == 0 || len(snaps) == 0 {
		return snaps
	}
	for i := range snaps {
		for _, c := range consumed {
			if c.Namespace != snaps[i].namespace || c.PodName != snaps[i].name {
				continue
			}
			stored := &oomBumpRecord{OOMAt: c.OOMAt, Restart: c.Restart}
			if oomSignalIsNew(snaps[i].finished, snaps[i].restart, stored) {
				snaps[i].newOOM = true
				snaps[i].stored = nil
				snaps[i].fill = false
				snaps[i].freshStart = true
				continue
			}
			snaps[i].newOOM = false
			snaps[i].fill = false
		}
	}
	return snaps
}

// stripClearedOOMBumps copies pods and drops bump annotations a full revert
// removed. The caller's pod objects are left unchanged. Workload raw is
// blanked only when it equals a cleared value and no remaining pod still
// has that value. The returned consumed signals keep the same OOM from
// looking new.
func stripClearedOOMBumps(clears []oomBumpClear, container string, pods []corev1.Pod, workloadRaw string) ([]corev1.Pod, string, []oomBumpConsumed) {
	if len(clears) == 0 {
		return pods, workloadRaw, nil
	}
	key, ok := oomBumpKey(container)
	if !ok {
		return pods, workloadRaw, nil
	}
	drop := map[string]oomBumpClear{}
	for _, c := range clears {
		if c.container != container {
			continue
		}
		drop[c.namespace+"/"+c.podName] = c
	}
	if len(drop) == 0 {
		return pods, workloadRaw, nil
	}
	out := make([]corev1.Pod, len(pods))
	consumed := make([]oomBumpConsumed, 0, len(drop))
	var clearedRaws []string
	for i := range pods {
		out[i] = pods[i]
		c, hit := drop[pods[i].Namespace+"/"+pods[i].Name]
		if !hit {
			continue
		}
		consumed = append(consumed, oomBumpConsumed{
			Namespace: pods[i].Namespace,
			PodName:   pods[i].Name,
			OOMAt:     c.oomAt,
			Restart:   c.restart,
		})
		if c.raw != "" {
			clearedRaws = append(clearedRaws, c.raw)
		}
		if pods[i].Annotations == nil {
			continue
		}
		anns := make(map[string]string, len(pods[i].Annotations))
		for k, v := range pods[i].Annotations {
			if k == key {
				continue
			}
			anns[k] = v
		}
		out[i].Annotations = anns
	}
	return out, blankClearedWorkloadRaw(workloadRaw, clearedRaws, out, key), consumed
}

func blankClearedWorkloadRaw(workloadRaw string, clearedRaws []string, pods []corev1.Pod, key string) string {
	if workloadRaw == "" {
		return ""
	}
	matched := false
	for _, raw := range clearedRaws {
		if raw == workloadRaw {
			matched = true
			break
		}
	}
	if !matched {
		return workloadRaw
	}
	for i := range pods {
		if pods[i].Annotations != nil && pods[i].Annotations[key] == workloadRaw {
			return workloadRaw
		}
	}
	return ""
}

func siblingHoldsOOMBumpRaw(pods []corev1.Pod, skipNS, skipName, key, raw string) bool {
	if raw == "" || key == "" {
		return false
	}
	for i := range pods {
		if pods[i].Namespace == skipNS && pods[i].Name == skipName {
			continue
		}
		if pods[i].Annotations != nil && pods[i].Annotations[key] == raw {
			return true
		}
	}
	return false
}

// oomBumpRevertDecisionFor reports whether RevertPod must be suppressed.
// throttle, notready, and slo keep today's CPU revert but set Floor so memory
// does not go below the bump. Error, and a restart that includes a non-OOM
// death, leave Floor at 0 so the whole pod reverts. Hold expiry returns a
// zero decision.
func oomBumpRevertDecisionFor(pod *corev1.Pod, container, reason string, resizedAt, now time.Time, maxBumps int) oomBumpRevertDecision {
	if pod == nil {
		return oomBumpRevertDecision{}
	}
	key, ok := oomBumpKey(container)
	if !ok {
		return oomBumpRevertDecision{}
	}
	rec, parsed := parseOOMBumpRecord(pod.Annotations[key])
	if !parsed || !now.Before(rec.HoldUntil) {
		return oomBumpRevertDecision{}
	}
	if maxBumps <= 0 {
		maxBumps = int(attunev1alpha1.DefaultOOMBumpMaxBumps)
	}
	cp := rec
	floor, _ := activeBumpFloor(&cp, now)
	switch {
	case reason == "oomkill" || reason == "restart":
		cs := findContainerStatusByName(pod, container)
		suppress, step, capped := criticalBumpAction(reason, resizedAt, &cp, now, cs, maxBumps)
		decision := oomBumpRevertDecision{
			Suppress: suppress,
			Step:     step,
			Capped:   capped,
			Stored:   &cp,
		}
		// A non-OOM death still reverts the whole pod, including memory.
		if suppress {
			decision.Floor = floor
		}
		return decision
	case reason == "throttle" || reason == "notready" || strings.HasPrefix(reason, "slo"):
		return oomBumpRevertDecision{Floor: floor, Stored: &cp}
	default:
		return oomBumpRevertDecision{}
	}
}

type oomPodSnap struct {
	namespace string
	name      string
	live      int64
	stored    *oomBumpRecord
	finished  time.Time
	restart   int32
	newOOM    bool
	fill      bool
	// freshStart means a full revert already consumed an older signal.
	// The next OOM must not inherit a sibling workload record.
	freshStart bool
}

func skippedOOMStamp(snap oomPodSnap, now time.Time) oomBumpPodStamp {
	return oomBumpPodStamp{
		Namespace:      snap.namespace,
		PodName:        snap.name,
		AnnotationOnly: true,
		Result:         oomBumpSkipped,
		Stamp: oomBumpRecord{
			Origin:    snap.live,
			Floor:     snap.live,
			OOMAt:     snap.finished.UTC(),
			Restart:   snap.restart,
			HoldUntil: now.UTC(),
		},
	}
}

func snapshotOOMPods(pods []corev1.Pod, container, annKey string, workload *oomBumpRecord) []oomPodSnap {
	out := make([]oomPodSnap, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		live, found := containerMemoryLiveBytes(pod, container)
		if !found {
			continue
		}
		snap := oomPodSnap{
			namespace: pod.Namespace,
			name:      pod.Name,
			live:      live,
		}
		if raw := pod.Annotations[annKey]; raw != "" {
			if rec, ok := parseOOMBumpRecord(raw); ok {
				cp := rec
				snap.stored = &cp
			}
		}
		if cs := findContainerStatusByName(pod, container); cs != nil {
			finished, restart, isOOM := containerOOMSignal(cs)
			snap.finished = finished
			snap.restart = restart
			basis := snap.stored
			if basis == nil {
				basis = workload
			}
			if isOOM {
				snap.newOOM = oomSignalIsNew(finished, restart, basis)
			}
			if snap.stored != nil {
				snap.fill = oomTimestampFilled(finished, restart, snap.stored)
			}
		}
		out = append(out, snap)
	}
	return out
}

func containerMemoryLiveBytes(pod *corev1.Pod, name string) (int64, bool) {
	if pod == nil {
		return 0, false
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return memoryRequestBytes(c.Resources.Requests), true
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return memoryRequestBytes(c.Resources.Requests), true
		}
	}
	return 0, false
}

func memoryRequestBytes(requests corev1.ResourceList) int64 {
	q, ok := requests[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return q.Value()
}

func resolveOOMBump(block *attunev1alpha1.OOMBump) (ratio string, minBump int64, maxBumps int, hold time.Duration, ok bool) {
	if block == nil {
		return "", 0, 0, 0, false
	}
	ratio = attunev1alpha1.DefaultOOMBumpRatio
	if block.Ratio != nil {
		ratio = *block.Ratio
		if ratio == "" {
			return "", 0, 0, 0, false
		}
	}
	v, err := strconv.ParseFloat(ratio, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > float64(attunev1alpha1.MaxOOMBumpRatio) {
		return "", 0, 0, 0, false
	}
	minBump = attunev1alpha1.DefaultOOMBumpMinBump.Value()
	if block.MinBump != nil {
		if block.MinBump.Sign() <= 0 {
			return "", 0, 0, 0, false
		}
		minBump = block.MinBump.Value()
	}
	maxBumps = int(attunev1alpha1.DefaultOOMBumpMaxBumps)
	if block.MaxBumps != nil {
		maxBumps = int(*block.MaxBumps)
		if maxBumps < 1 || maxBumps > int(attunev1alpha1.MaxOOMBumpMaxBumps) {
			return "", 0, 0, 0, false
		}
	}
	hold = attunev1alpha1.DefaultOOMBumpHold
	if block.Hold != nil {
		hold = block.Hold.Duration
		if hold < attunev1alpha1.MinOOMBumpHold || hold > attunev1alpha1.MaxOOMBumpHold {
			return "", 0, 0, 0, false
		}
	}
	return ratio, minBump, maxBumps, hold, true
}

func heldWorkloadValue(held []oomBumpRecord, stamps []oomBumpPodStamp, now time.Time) string {
	best, found := highestHeldBump(held, now)
	for _, stamp := range stamps {
		rec := stamp.Stamp
		if !now.Before(rec.HoldUntil) {
			continue
		}
		if !found || rec.Count > best.Count || (rec.Count == best.Count && rec.Floor > best.Floor) {
			best = rec
			found = true
		}
	}
	if !found {
		return ""
	}
	raw, err := formatOOMBumpRecord(best)
	if err != nil {
		return ""
	}
	return raw
}
