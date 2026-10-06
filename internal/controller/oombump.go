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
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	oomBumpKeyPrefix = "oom-bump."
	oomBumpKeyLimit  = 63
	oomKilledReason  = "OOMKilled"

	oomBumpApplied = "applied"
	oomBumpClamped = "clamped"
	oomBumpCapped  = "capped"
	oomBumpSkipped = "skipped"
)

var (
	errOOMBumpRatio    = errors.New("oom bump ratio is invalid")
	errOOMBumpOverflow = errors.New("oom bump overflows int64")
	errOOMBumpCount    = errors.New("oom bump count must be positive")
)

// oomBumpRecord is the parsed attune.io/oom-bump.<container> value.
// Origin is the live memory request before the first bump of the streak.
// Floor is the request already applied for Count. HoldUntil is bumpedAt + hold.
type oomBumpRecord struct {
	Count     int
	Origin    int64
	Floor     int64
	OOMAt     time.Time
	Restart   int32
	HoldUntil time.Time
}

// oomBumpInput is one container on one pod. Bytes are memory requests.
// MaxAllowed nil means uncapped. A non-nil zero is a real ceiling.
type oomBumpInput struct {
	LiveBytes       int64
	PercentileBytes int64
	HasPercentile   bool
	Ratio           string
	MinBumpBytes    int64
	MaxBumps        int
	MaxAllowed      *int64
	Now             time.Time
	Hold            time.Duration
	NewOOM          bool
	FinishedAt      time.Time
	Restart         int32
	Stored          *oomBumpRecord
	Excluded        bool
}

// oomBumpProposal is the memory request to publish and the annotation to
// store after a successful resize. Stamp is nil when count must not change.
// AnnotationOnly updates oomAt without a resize. Capped, and an in-hold
// skip that cannot step above live, emit one MetricNow sample. A timestamp
// fill does not.
type oomBumpProposal struct {
	Result         string
	PublishBytes   int64
	UsePublish     bool
	Stamp          *oomBumpRecord
	AnnotationOnly bool
}

// oomBumpKey returns attune.io/oom-bump.<container>. The name segment after
// attune.io/ must fit in 63 characters. Empty names and names containing /
// are skipped. The container name is never truncated.
func oomBumpKey(container string) (string, bool) {
	if container == "" || strings.Contains(container, "/") {
		return "", false
	}
	name := oomBumpKeyPrefix + container
	if len(name) > oomBumpKeyLimit {
		return "", false
	}
	return "attune.io/" + name, true
}

// ceilRatioPow returns ceil(origin * ratio^n) in integer bytes.
// ratio is parsed with big.Rat so "1.2" is 6/5, not a float64.
func ceilRatioPow(origin int64, ratio string, n int) (int64, error) {
	if origin < 0 || n < 0 {
		return 0, errOOMBumpCount
	}
	if n == 0 {
		return origin, nil
	}
	r := new(big.Rat)
	if _, ok := r.SetString(ratio); !ok || r.Sign() <= 0 {
		return 0, errOOMBumpRatio
	}
	acc := new(big.Rat).SetInt64(origin)
	for i := 0; i < n; i++ {
		acc.Mul(acc, r)
	}
	q, rem := new(big.Int).QuoRem(acc.Num(), acc.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errOOMBumpOverflow
	}
	return q.Int64(), nil
}

func originPlusSteps(origin, minBump int64, n int) (int64, error) {
	if origin < 0 || minBump < 0 || n < 0 {
		return 0, errOOMBumpCount
	}
	if n == 0 || minBump == 0 {
		return origin, nil
	}
	if int64(n) > 0 && minBump > math.MaxInt64/int64(n) {
		return 0, errOOMBumpOverflow
	}
	add := minBump * int64(n)
	if origin > math.MaxInt64-add {
		return 0, errOOMBumpOverflow
	}
	return origin + add, nil
}

// oomBumpBytes is max(ceil(origin * ratio^count), origin + minBump*count),
// then maxAllowed. count is the new streak count, measured from origin.
// A result of skipped means the post-ceiling value is not strictly above live.
func oomBumpBytes(origin, minBump, live int64, ratio string, count int, maxAllowed *int64) (int64, string, error) {
	if count < 1 {
		return 0, "", errOOMBumpCount
	}
	ceilBytes, err := ceilRatioPow(origin, ratio, count)
	if err != nil {
		return 0, "", err
	}
	plus, err := originPlusSteps(origin, minBump, count)
	if err != nil {
		return 0, "", err
	}
	next := ceilBytes
	if plus > next {
		next = plus
	}
	result := oomBumpApplied
	if maxAllowed != nil && next > *maxAllowed {
		next = *maxAllowed
		result = oomBumpClamped
	}
	if next <= live {
		return next, oomBumpSkipped, nil
	}
	return next, result, nil
}

// containerOOMSignal prefers a current OOMKilled termination, then lastState.
// CrashLoopBackOff does not carry the reason; the reason stays on lastState.
func containerOOMSignal(cs *corev1.ContainerStatus) (time.Time, int32, bool) {
	if cs == nil {
		return time.Time{}, 0, false
	}
	if term := cs.State.Terminated; term != nil && term.Reason == oomKilledReason {
		return term.FinishedAt.Time, cs.RestartCount, true
	}
	if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == oomKilledReason {
		return term.FinishedAt.Time, cs.RestartCount, true
	}
	return time.Time{}, 0, false
}

// oomSignalIsNew reports whether this OOMKilled termination has not been
// consumed yet. The first sighting with a zero finishedAt and no stored
// annotation is not a bump. A zero finishedAt whose restart count is above
// the stored restart is new. A kubelet that later fills finishedAt for that
// same restart is not a second bump.
func oomSignalIsNew(finished time.Time, restart int32, stored *oomBumpRecord) bool {
	if stored == nil {
		return !finished.IsZero()
	}
	if finished.Equal(stored.OOMAt) && restart == stored.Restart {
		return false
	}
	if !finished.IsZero() && finished.After(stored.OOMAt) {
		if stored.OOMAt.IsZero() && restart == stored.Restart {
			return false
		}
		return true
	}
	return finished.IsZero() && restart > stored.Restart
}

// oomTimestampFilled reports a zero stored oomAt that now has a finishedAt
// at the same restart. The caller refreshes oomAt and does not increment.
func oomTimestampFilled(finished time.Time, restart int32, stored *oomBumpRecord) bool {
	if stored == nil || !stored.OOMAt.IsZero() || finished.IsZero() {
		return false
	}
	return restart == stored.Restart
}

func proposeOOMBump(in oomBumpInput) oomBumpProposal {
	base := percentileProposal(in)
	if in.Stored != nil && oomTimestampFilled(in.FinishedAt, in.Restart, in.Stored) {
		refreshed := *in.Stored
		refreshed.OOMAt = in.FinishedAt.UTC()
		base.Stamp = &refreshed
		base.AnnotationOnly = true
		return base
	}
	if !in.NewOOM {
		return base
	}
	if in.Excluded {
		return skippedSignalStamp(in, base)
	}
	origin := in.LiveBytes
	storedCount := 0
	if in.Stored != nil && in.Now.Before(in.Stored.HoldUntil) {
		origin = in.Stored.Origin
		storedCount = in.Stored.Count
	}
	if in.MaxBumps > 0 && storedCount >= in.MaxBumps {
		return cappedProposal(in, base)
	}
	next, result, err := oomBumpBytes(origin, in.MinBumpBytes, in.LiveBytes, in.Ratio, storedCount+1, in.MaxAllowed)
	if err != nil {
		return skippedSignalStamp(in, base)
	}
	if result == oomBumpSkipped {
		return inHoldLiveStep(in, base, origin, storedCount)
	}
	stamp := oomBumpRecord{
		Count:     storedCount + 1,
		Origin:    origin,
		Floor:     next,
		OOMAt:     in.FinishedAt.UTC(),
		Restart:   in.Restart,
		HoldUntil: in.Now.UTC().Add(in.Hold),
	}
	return oomBumpProposal{
		Result:       result,
		PublishBytes: next,
		UsePublish:   true,
		Stamp:        &stamp,
	}
}

// skippedSignalStamp records this OOM once without a new step.
// HoldUntil stays in the past unless a stored hold is still open,
// so the record does not become a floor the next reconcile republishes.
func skippedSignalStamp(in oomBumpInput, base oomBumpProposal) oomBumpProposal {
	rec := oomBumpRecord{
		Origin:    in.LiveBytes,
		Floor:     in.LiveBytes,
		OOMAt:     in.FinishedAt.UTC(),
		Restart:   in.Restart,
		HoldUntil: in.Now.UTC(),
	}
	if in.Stored != nil {
		rec.Count = in.Stored.Count
		rec.Origin = in.Stored.Origin
		rec.Floor = in.Stored.Floor
		if !in.Stored.HoldUntil.IsZero() {
			rec.HoldUntil = in.Stored.HoldUntil
		}
	}
	base.Result = oomBumpSkipped
	base.Stamp = &rec
	base.AnnotationOnly = true
	return base
}

// clampedAtMaxProposal records an out-of-hold OOM that cannot rise
// because maxAllowed is already at or below live. Count and floor stay.
// A first sighting stores the live request as the floor and does not
// open a hold, so nothing is published above live.
func clampedAtMaxProposal(in oomBumpInput, base oomBumpProposal) oomBumpProposal {
	rec := oomBumpRecord{
		Origin:    in.LiveBytes,
		Floor:     in.LiveBytes,
		OOMAt:     in.FinishedAt.UTC(),
		Restart:   in.Restart,
		HoldUntil: in.Now.UTC(),
	}
	if in.Stored != nil {
		rec.Count = in.Stored.Count
		rec.Origin = in.Stored.Origin
		rec.Floor = in.Stored.Floor
		if !in.Stored.HoldUntil.IsZero() {
			rec.HoldUntil = in.Stored.HoldUntil
		}
	}
	base.Result = oomBumpClamped
	base.Stamp = &rec
	base.AnnotationOnly = true
	return base
}

// inHoldLiveStep runs when the frozen-origin step is not strictly above
// live. Inside a hold, one step from live publishes a higher request.
// When that live step cannot rise, the stored signal is refreshed and
// the count stays put. Outside a hold, maxAllowed at or below live
// stores one clamped signal and does not move the request.
func inHoldLiveStep(in oomBumpInput, base oomBumpProposal, origin int64, storedCount int) oomBumpProposal {
	if in.Stored == nil || !in.Now.Before(in.Stored.HoldUntil) {
		if in.MaxAllowed != nil && *in.MaxAllowed <= in.LiveBytes {
			return clampedAtMaxProposal(in, base)
		}
		base.Result = oomBumpSkipped
		return base
	}
	liveNext, liveResult, err := oomBumpBytes(in.LiveBytes, in.MinBumpBytes, in.LiveBytes, in.Ratio, 1, in.MaxAllowed)
	if err != nil || liveResult == oomBumpSkipped {
		refreshed := *in.Stored
		refreshed.OOMAt = in.FinishedAt.UTC()
		refreshed.Restart = in.Restart
		base.Result = oomBumpSkipped
		base.Stamp = &refreshed
		base.AnnotationOnly = true
		return base
	}
	stamp := oomBumpRecord{
		Count:     storedCount + 1,
		Origin:    origin,
		Floor:     liveNext,
		OOMAt:     in.FinishedAt.UTC(),
		Restart:   in.Restart,
		HoldUntil: in.Now.UTC().Add(in.Hold),
	}
	return oomBumpProposal{
		Result:       liveResult,
		PublishBytes: liveNext,
		UsePublish:   true,
		Stamp:        &stamp,
	}
}

func percentileProposal(in oomBumpInput) oomBumpProposal {
	if in.Stored != nil && in.Now.Before(in.Stored.HoldUntil) {
		pub := in.Stored.Floor
		if in.HasPercentile && in.PercentileBytes > pub {
			pub = in.PercentileBytes
		}
		return oomBumpProposal{UsePublish: true, PublishBytes: pub}
	}
	if in.HasPercentile {
		return oomBumpProposal{UsePublish: true, PublishBytes: in.PercentileBytes}
	}
	return oomBumpProposal{}
}

// cappedProposal records this OOM without incrementing. Count, origin,
// floor, and holdUntil stay. After holdUntil the percentile is left alone,
// so an expired hold does not keep publishing the old floor.
func cappedProposal(in oomBumpInput, base oomBumpProposal) oomBumpProposal {
	base.Result = oomBumpCapped
	if in.Stored == nil {
		return base
	}
	refreshed := *in.Stored
	refreshed.OOMAt = in.FinishedAt.UTC()
	refreshed.Restart = in.Restart
	base.Stamp = &refreshed
	base.AnnotationOnly = true
	if !in.Now.Before(in.Stored.HoldUntil) {
		return base
	}
	base.UsePublish = true
	base.PublishBytes = in.Stored.Floor
	if in.HasPercentile && in.PercentileBytes > base.PublishBytes {
		base.PublishBytes = in.PercentileBytes
	}
	return base
}

// highestHeldBump returns the record with the highest count whose hold is
// still in the future. Ties prefer the higher floor.
func highestHeldBump(records []oomBumpRecord, now time.Time) (oomBumpRecord, bool) {
	var best oomBumpRecord
	found := false
	for _, rec := range records {
		if !now.Before(rec.HoldUntil) {
			continue
		}
		if !found || rec.Count > best.Count || (rec.Count == best.Count && rec.Floor > best.Floor) {
			best = rec
			found = true
		}
	}
	return best, found
}

// maxInHoldLiveMemory is the highest live memory request on pods whose
// oom-bump annotation for container is still inside holdUntil. Floor must
// be positive. Expired and missing annotations do not count.
func maxInHoldLiveMemory(pods []corev1.Pod, container string, now time.Time) (resource.Quantity, bool) {
	key, ok := oomBumpKey(container)
	if !ok {
		return resource.Quantity{}, false
	}
	var best resource.Quantity
	found := false
	for i := range pods {
		rec, parsed := parseOOMBumpRecord(pods[i].Annotations[key])
		if !parsed || rec.Floor <= 0 || !now.Before(rec.HoldUntil) {
			continue
		}
		c := findContainerByName(&pods[i], container)
		if c == nil || c.Resources.Requests == nil {
			continue
		}
		live, exists := c.Resources.Requests[corev1.ResourceMemory]
		if !exists {
			continue
		}
		if !found || live.Cmp(best) > 0 {
			best = live
			found = true
		}
	}
	return best, found
}

// activeBumpFloor is the request a memory revert must not go below while
// holdUntil is in the future.
func activeBumpFloor(stored *oomBumpRecord, now time.Time) (int64, bool) {
	if stored == nil || stored.Floor <= 0 || !now.Before(stored.HoldUntil) {
		return 0, false
	}
	return stored.Floor, true
}

// criticalBumpAction classifies an oomkill or restart verdict while a bump
// hold is active. suppressRevert means do not call RevertPod. step means the
// next bump from origin is due. A non-OOM termination does not suppress.
// Throttle, NotReady, and SLO verdicts are unchanged (suppress is false);
// callers still keep memory at activeBumpFloor.
func criticalBumpAction(reason string, resizedAt time.Time, stored *oomBumpRecord, now time.Time, cs *corev1.ContainerStatus, maxBumps int) (suppressRevert, step, capped bool) {
	if stored == nil || !now.Before(stored.HoldUntil) {
		return false, false, false
	}
	switch reason {
	case "oomkill":
		return oomHoldAction(stored, cs, maxBumps)
	case "restart":
		// Visible terminations are all OOMKilled, including none after
		// resizedAt: stay inside the hold. A non-OOM termination does not.
		since := deathsSinceResize(cs, resizedAt)
		for _, term := range since {
			if term.Reason != oomKilledReason {
				return false, false, false
			}
		}
		return oomHoldAction(stored, cs, maxBumps)
	default:
		return false, false, false
	}
}

func oomHoldAction(stored *oomBumpRecord, cs *corev1.ContainerStatus, maxBumps int) (bool, bool, bool) {
	finished, restart, ok := containerOOMSignal(cs)
	if !ok || !oomSignalIsNew(finished, restart, stored) {
		return true, false, false
	}
	if maxBumps > 0 && stored.Count >= maxBumps {
		return true, false, true
	}
	return true, true, false
}

func deathsSinceResize(cs *corev1.ContainerStatus, resizedAt time.Time) []corev1.ContainerStateTerminated {
	if cs == nil {
		return nil
	}
	var since []corev1.ContainerStateTerminated
	consider := func(term *corev1.ContainerStateTerminated) {
		if term == nil {
			return
		}
		finished := term.FinishedAt.Time
		if !finished.IsZero() && finished.Before(resizedAt) {
			return
		}
		since = append(since, *term)
	}
	consider(cs.State.Terminated)
	consider(cs.LastTerminationState.Terminated)
	return since
}

func formatOOMBumpRecord(rec oomBumpRecord) (string, error) {
	if rec.HoldUntil.IsZero() || rec.Count < 0 || rec.Restart < 0 || rec.Origin < 0 || rec.Floor < 0 {
		return "", errors.New("oom bump record is incomplete")
	}
	origin := resource.NewQuantity(rec.Origin, resource.BinarySI)
	floor := resource.NewQuantity(rec.Floor, resource.BinarySI)
	return fmt.Sprintf(
		"count=%d,origin=%s,floor=%s,oomAt=%s,restart=%d,holdUntil=%s",
		rec.Count,
		origin.String(),
		floor.String(),
		rec.OOMAt.UTC().Format(time.RFC3339),
		rec.Restart,
		rec.HoldUntil.UTC().Format(time.RFC3339),
	), nil
}

func parseOOMBumpRecord(raw string) (oomBumpRecord, bool) {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return oomBumpRecord{}, false
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 6 {
		return oomBumpRecord{}, false
	}
	vals := make(map[string]string, len(parts))
	for _, part := range parts {
		key, val, ok := strings.Cut(part, "=")
		if !ok || key == "" || val == "" {
			return oomBumpRecord{}, false
		}
		if _, exists := vals[key]; exists {
			return oomBumpRecord{}, false
		}
		vals[key] = val
	}
	count, err := strconv.Atoi(vals["count"])
	if err != nil || count < 0 {
		return oomBumpRecord{}, false
	}
	restart64, err := strconv.ParseInt(vals["restart"], 10, 32)
	if err != nil || restart64 < 0 {
		return oomBumpRecord{}, false
	}
	origin, ok := quantityBytes(vals["origin"])
	if !ok {
		return oomBumpRecord{}, false
	}
	floor, ok := quantityBytes(vals["floor"])
	if !ok {
		return oomBumpRecord{}, false
	}
	oomAt, err := time.Parse(time.RFC3339, vals["oomAt"])
	if err != nil {
		return oomBumpRecord{}, false
	}
	holdUntil, err := time.Parse(time.RFC3339, vals["holdUntil"])
	if err != nil || holdUntil.IsZero() {
		return oomBumpRecord{}, false
	}
	return oomBumpRecord{
		Count:     count,
		Origin:    origin,
		Floor:     floor,
		OOMAt:     oomAt.UTC(),
		Restart:   int32(restart64),
		HoldUntil: holdUntil.UTC(),
	}, true
}

func quantityBytes(raw string) (int64, bool) {
	q, err := resource.ParseQuantity(raw)
	if err != nil || q.Sign() < 0 {
		return 0, false
	}
	v := q.Value()
	if v < 0 {
		return 0, false
	}
	back := resource.NewQuantity(v, resource.BinarySI)
	return v, q.Cmp(*back) == 0
}
