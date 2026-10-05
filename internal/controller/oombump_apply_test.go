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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestPlanWorkloadOOMBump(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	hold := now.Add(time.Hour)
	expired := now.Add(-time.Minute)

	origin512 := qtyBytes(t, "512Mi")
	first512 := int64(644245095)
	second512 := int64(773094114)
	live200 := qtyBytes(t, "200Mi")
	floor300 := qtyBytes(t, "300Mi")
	cap250 := qtyBytes(t, "250Mi")
	cap320 := qtyBytes(t, "320Mi")

	empty := &attunev1alpha1.OOMBump{}
	ann := func(rec oomBumpRecord) string {
		t.Helper()
		raw, err := formatOOMBumpRecord(rec)
		require.NoError(t, err)
		return raw
	}
	stored := func(count int, origin, floor int64, at time.Time, restart int32, until time.Time) string {
		t.Helper()
		return ann(oomBumpRecord{
			Count: count, Origin: origin, Floor: floor,
			OOMAt: at, Restart: restart, HoldUntil: until,
		})
	}

	tests := []struct {
		name        string
		block       *attunev1alpha1.OOMBump
		container   string
		excluded    bool
		percentile  int64
		hasPct      bool
		maxAllowed  *int64
		workload    string
		pods        []corev1.Pod
		wantPub     bool
		wantBytes   int64
		wantNote    bool
		wantStamps  int
		wantCount   int
		wantOrigin  int64
		wantFloor   int64
		wantNow     []string
		wantEvent   string
		wantWork    bool
		wantAnnOnly bool
	}{
		{
			name: "nil block ignores oom",
			pods: []corev1.Pod{oomBumpPod("a", "app", "200Mi", oomKilledStatus(later, 1), "", false)},
		},
		{
			name:       "empty block uses defaults",
			block:      empty,
			container:  "app",
			pods:       []corev1.Pod{oomBumpPod("a", "app", "200Mi", oomKilledStatus(later, 1), "", false)},
			wantPub:    true,
			wantBytes:  floor300,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  1,
			wantOrigin: live200,
			wantFloor:  floor300,
			wantWork:   true,
		},
		{
			name:      "second bump stays on origin",
			block:     empty,
			container: "app",
			pods: []corev1.Pod{oomBumpPod("a", "app", "644245095", oomKilledStatus(later, 2),
				stored(1, origin512, first512, now, 1, hold), false)},
			wantPub:    true,
			wantBytes:  second512,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  2,
			wantOrigin: origin512,
			wantFloor:  second512,
			wantWork:   true,
		},
		{
			name:      "sibling does not increment",
			block:     empty,
			container: "app",
			pods: []corev1.Pod{
				oomBumpPod("oom", "app", "200Mi", oomKilledStatus(later, 1), "", false),
				oomBumpPod("quiet", "app", "200Mi", nil, "", false),
			},
			wantPub:    true,
			wantBytes:  floor300,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  1,
			wantOrigin: live200,
			wantFloor:  floor300,
			wantWork:   true,
		},
		{
			name:       "excluded new oom is skipped once",
			block:      empty,
			container:  "istio-proxy",
			excluded:   true,
			pods:       []corev1.Pod{oomBumpPod("a", "istio-proxy", "200Mi", oomKilledStatus(later, 1), "", false)},
			wantNow:    []string{oomBumpSkipped},
			wantStamps: 1,
		},
		{
			name:      "quiet excluded emits nothing",
			block:     empty,
			container: "istio-proxy",
			excluded:  true,
			pods:      []corev1.Pod{oomBumpPod("a", "istio-proxy", "200Mi", nil, "", false)},
		},
		{
			name:       "hold keeps floor above percentile",
			block:      empty,
			container:  "app",
			percentile: live200,
			hasPct:     true,
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", nil,
				stored(1, live200, floor300, now, 1, hold), false)},
			wantPub:   true,
			wantBytes: floor300,
			wantNote:  true,
			wantWork:  true,
		},
		{
			name:       "after hold percentile wins",
			block:      empty,
			container:  "app",
			percentile: live200,
			hasPct:     true,
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", nil,
				stored(1, live200, floor300, now, 1, expired), false)},
			wantPub:   true,
			wantBytes: live200,
		},
		{
			name:      "same finishedAt does not bump again",
			block:     empty,
			container: "app",
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", oomKilledStatus(now, 1),
				stored(1, live200, floor300, now, 1, hold), false)},
			wantPub:   true,
			wantBytes: floor300,
			wantNote:  true,
			wantWork:  true,
		},
		{
			name:      "zero finishedAt first sight is not a bump",
			block:     empty,
			container: "app",
			pods:      []corev1.Pod{oomBumpPod("a", "app", "200Mi", oomKilledStatus(time.Time{}, 1), "", false)},
		},
		{
			name:      "zero finishedAt with a higher restart is new",
			block:     empty,
			container: "app",
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", oomKilledStatus(time.Time{}, 2),
				stored(1, live200, floor300, time.Time{}, 1, hold), false)},
			wantPub:    true,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  2,
			wantOrigin: live200,
			wantWork:   true,
		},
		{
			name:       "native sidecar status is scanned",
			block:      empty,
			container:  "sidecar",
			pods:       []corev1.Pod{oomBumpPod("a", "sidecar", "200Mi", oomKilledStatus(later, 1), "", true)},
			wantPub:    true,
			wantBytes:  floor300,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  1,
			wantWork:   true,
		},
		{
			name:       "new pod continues workload count",
			block:      empty,
			container:  "app",
			workload:   stored(2, origin512, first512, now, 2, hold),
			pods:       []corev1.Pod{oomBumpPod("fresh", "app", "256Mi", oomKilledStatus(later, 1), "", false)},
			wantPub:    true,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  3,
			wantOrigin: origin512,
			wantWork:   true,
		},
		{
			name:       "maxAllowed clamps the step",
			block:      empty,
			container:  "app",
			maxAllowed: i64ptr(cap250),
			pods:       []corev1.Pod{oomBumpPod("a", "app", "200Mi", oomKilledStatus(later, 1), "", false)},
			wantPub:    true,
			wantBytes:  cap250,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  1,
			wantFloor:  cap250,
			wantEvent:  "",
			wantWork:   true,
		},
		{
			name:       "maxAllowed above the step stays applied",
			block:      empty,
			container:  "app",
			maxAllowed: i64ptr(cap320),
			pods:       []corev1.Pod{oomBumpPod("a", "app", "200Mi", oomKilledStatus(later, 1), "", false)},
			wantPub:    true,
			wantBytes:  floor300,
			wantStamps: 1,
			wantCount:  1,
			wantFloor:  floor300,
			wantNote:   true,
			wantWork:   true,
		},
		{
			name:        "maxAllowed below live consumes once",
			block:       empty,
			container:   "app",
			maxAllowed:  i64ptr(cap250),
			pods:        []corev1.Pod{oomBumpPod("a", "app", "300Mi", oomKilledStatus(later, 2), stored(1, live200, floor300, now, 1, hold), false)},
			wantPub:     true,
			wantBytes:   cap250,
			wantNote:    true,
			wantStamps:  1,
			wantCount:   1,
			wantOrigin:  live200,
			wantFloor:   floor300,
			wantNow:     []string{oomBumpSkipped},
			wantWork:    true,
			wantAnnOnly: true,
		},
		{
			name:      "slash in the container name is skipped",
			block:     empty,
			container: "a/b",
			pods:      []corev1.Pod{oomBumpPod("a", "a/b", "200Mi", oomKilledStatus(later, 1), "", false)},
		},
		{
			name:      "container name past 54 characters is skipped",
			block:     empty,
			container: strings.Repeat("c", 55),
			pods:      []corev1.Pod{oomBumpPod("a", strings.Repeat("c", 55), "200Mi", oomKilledStatus(later, 1), "", false)},
		},
		{
			name:      "fourth step is capped",
			block:     empty,
			container: "app",
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", oomKilledStatus(later, 4),
				stored(3, live200, floor300, now, 3, hold), false)},
			wantPub:     true,
			wantBytes:   floor300,
			wantNote:    true,
			wantStamps:  1,
			wantCount:   3,
			wantOrigin:  live200,
			wantFloor:   floor300,
			wantNow:     []string{oomBumpCapped},
			wantEvent:   "OOMBumpCapped",
			wantWork:    true,
			wantAnnOnly: true,
		},
		{
			name:       "expired hold steps once and keeps a higher percentile",
			block:      empty,
			container:  "app",
			percentile: origin512,
			hasPct:     true,
			pods: []corev1.Pod{oomBumpPod("a", "app", "300Mi", oomKilledStatus(later, 4),
				stored(3, live200, floor300, now, 3, expired), false)},
			wantPub:    true,
			wantBytes:  origin512,
			wantNote:   true,
			wantStamps: 1,
			wantCount:  1,
			wantOrigin: qtyBytes(t, "300Mi"),
			wantFloor:  origin512,
			wantWork:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planWorkloadOOMBump(tt.block, tt.container, tt.excluded, tt.percentile, tt.hasPct, tt.maxAllowed, tt.workload, tt.pods, now, nil)
			assert.Equal(t, tt.wantPub, got.UsePublish, "publish")
			if tt.wantBytes != 0 {
				assert.Equal(t, tt.wantBytes, got.PublishBytes, "bytes")
			}
			assert.Equal(t, tt.wantNote, got.Note, "note")
			assert.Len(t, got.Stamps, tt.wantStamps)
			assert.Equal(t, tt.wantNow, got.MetricNow)
			assert.Equal(t, tt.wantEvent, got.Event)
			if tt.wantWork {
				assert.NotEmpty(t, got.WorkloadValue)
			} else {
				assert.Empty(t, got.WorkloadValue)
			}
			if tt.wantStamps == 1 && len(got.Stamps) == 1 {
				stamp := got.Stamps[0]
				assert.Equal(t, tt.wantCount, stamp.Stamp.Count)
				if tt.wantOrigin != 0 {
					assert.Equal(t, tt.wantOrigin, stamp.Stamp.Origin)
				}
				if tt.wantFloor != 0 {
					assert.Equal(t, tt.wantFloor, stamp.Stamp.Floor)
				}
				if tt.name == "sibling does not increment" {
					assert.Equal(t, "oom", stamp.PodName)
				}
				if tt.name == "maxAllowed clamps the step" {
					assert.Equal(t, oomBumpClamped, stamp.Result)
					assert.Equal(t, "OOMBumpClamped", stamp.Event)
				}
				if tt.name == "maxAllowed above the step stays applied" {
					assert.Equal(t, oomBumpApplied, stamp.Result)
				}
				if tt.name == "second bump stays on origin" {
					assert.NotEqual(t, first512, stamp.Stamp.Floor)
				}
				if tt.wantAnnOnly {
					assert.True(t, stamp.AnnotationOnly)
					assert.Empty(t, stamp.Result)
				}
				if tt.name == "maxAllowed below live consumes once" {
					assert.True(t, got.QuietClamp)
					assert.Equal(t, floor300, got.QuietClampFrom)
				}
			}
		})
	}
}

func TestOOMBumpRevertDecision(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	resizedAt := now.Add(-time.Minute)
	triggerAt := resizedAt.Add(-time.Second)
	hold := now.Add(time.Hour)
	floor := qtyBytes(t, "300Mi")
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: floor,
		OOMAt: triggerAt, Restart: 1, HoldUntil: hold,
	})
	require.NoError(t, err)

	pod := oomBumpPod("a", "app", "300Mi", oomKilledStatus(triggerAt, 1), raw, false)
	bare := oomBumpPod("b", "app", "300Mi", oomKilledStatus(triggerAt, 1), "", false)

	none := oomBumpRevertDecisionFor(&bare, "app", "oomkill", resizedAt, now, 3)
	assert.False(t, none.Suppress)
	assert.Zero(t, none.Floor)

	same := oomBumpRevertDecisionFor(&pod, "app", "oomkill", resizedAt, now, 3)
	assert.True(t, same.Suppress)
	assert.False(t, same.Step)
	assert.False(t, same.Capped)
	assert.Equal(t, floor, same.Floor)

	newer := pod.DeepCopy()
	newerStatus := oomKilledStatus(now, 2)
	newerStatus.Name = "app"
	newer.Status.ContainerStatuses[0] = *newerStatus
	step := oomBumpRevertDecisionFor(newer, "app", "oomkill", resizedAt, now, 3)
	assert.True(t, step.Suppress)
	assert.True(t, step.Step)
	assert.False(t, step.Capped)

	capped := oomBumpRevertDecisionFor(newer, "app", "oomkill", resizedAt, now, 1)
	assert.True(t, capped.Suppress)
	assert.False(t, capped.Step)
	assert.True(t, capped.Capped)

	errPod := pod.DeepCopy()
	errPod.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 2,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", FinishedAt: metav1.NewTime(now),
		}},
	}
	full := oomBumpRevertDecisionFor(errPod, "app", "restart", resizedAt, now, 3)
	assert.False(t, full.Suppress)
	assert.Zero(t, full.Floor)

	throttled := oomBumpRevertDecisionFor(&pod, "app", "throttle", resizedAt, now, 3)
	assert.False(t, throttled.Suppress)
	assert.Equal(t, floor, throttled.Floor)

	slo := oomBumpRevertDecisionFor(&pod, "app", "slo:latency", resizedAt, now, 3)
	assert.False(t, slo.Suppress)
	assert.Equal(t, floor, slo.Floor)

	plainErr := oomBumpRevertDecisionFor(errPod, "app", "Error", resizedAt, now, 3)
	assert.False(t, plainErr.Suppress)
	assert.Zero(t, plainErr.Floor)

	expiredRaw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: floor,
		OOMAt: triggerAt, Restart: 1, HoldUntil: now.Add(-time.Second),
	})
	require.NoError(t, err)
	expiredPod := oomBumpPod("a", "app", "300Mi", oomKilledStatus(now, 2), expiredRaw, false)
	expiredDec := oomBumpRevertDecisionFor(&expiredPod, "app", "oomkill", resizedAt, now, 3)
	assert.False(t, expiredDec.Suppress)
	assert.Zero(t, expiredDec.Floor)
}

func oomBumpPod(name, container, live string, status *corev1.ContainerStatus, ann string, init bool) corev1.Pod {
	q, err := resource.ParseQuantity(live)
	if err != nil {
		panic(err)
	}
	c := corev1.Container{
		Name:      container,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: q}},
	}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	if init {
		pod.Spec.InitContainers = []corev1.Container{c}
	} else {
		pod.Spec.Containers = []corev1.Container{c}
	}
	if status != nil {
		st := *status
		st.Name = container
		if init {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{st}
		} else {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{st}
		}
	}
	if ann != "" {
		key, ok := oomBumpKey(container)
		if !ok {
			panic("oom bump test container does not fit the annotation key")
		}
		pod.Annotations = map[string]string{key: ann}
	}
	return pod
}

func oomKilledStatus(finished time.Time, restart int32) *corev1.ContainerStatus {
	term := &corev1.ContainerStateTerminated{Reason: oomKilledReason}
	if !finished.IsZero() {
		term.FinishedAt = metav1.NewTime(finished)
	}
	return &corev1.ContainerStatus{
		RestartCount:         restart,
		LastTerminationState: corev1.ContainerState{Terminated: term},
	}
}

func TestPlanWorkloadOOMBump_ExpiredHoldStepsFromLive(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	origin := qtyBytes(t, "256Mi")
	live := qtyBytes(t, "1Gi")
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: origin, Floor: origin,
		OOMAt: now.Add(-2 * time.Hour), Restart: 1, HoldUntil: now.Add(-time.Minute),
	})
	require.NoError(t, err)
	pod := oomBumpPod("a", "app", "1Gi", oomKilledStatus(now, 2), "", false)
	ratio := "1.5"
	block := &attunev1alpha1.OOMBump{Ratio: &ratio}
	plan := planWorkloadOOMBump(block, "app", false, 0, false, nil, raw, []corev1.Pod{pod}, now, nil)
	require.True(t, plan.UsePublish)
	assert.Greater(t, plan.PublishBytes, live)
	require.Len(t, plan.Stamps, 1)
	assert.Equal(t, live, plan.Stamps[0].Stamp.Origin)
	assert.Equal(t, 1, plan.Stamps[0].Stamp.Count)

	updated, err := formatOOMBumpRecord(plan.Stamps[0].Stamp)
	require.NoError(t, err)
	next := oomBumpPod("a", "app", "1Gi", oomKilledStatus(now, 2), updated, false)
	second := planWorkloadOOMBump(block, "app", false, 0, false, nil, updated, []corev1.Pod{next}, now, nil)
	assert.Empty(t, second.MetricNow)
	if len(second.Stamps) > 0 {
		assert.Equal(t, 1, second.Stamps[0].Stamp.Count)
	}

	heldAt := now.Add(-2 * time.Hour)
	heldRaw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: origin, Floor: origin,
		OOMAt: heldAt, Restart: 1, HoldUntil: now.Add(time.Hour),
	})
	require.NoError(t, err)
	held := oomBumpPod("a", "app", "1Gi", oomKilledStatus(heldAt, 1), heldRaw, false)
	heldPlan := planWorkloadOOMBump(block, "app", false, 0, false, nil, heldRaw, []corev1.Pod{held}, now, nil)
	assert.Empty(t, heldPlan.Stamps)
	assert.Equal(t, origin, heldPlan.PublishBytes)
}

func TestPlanWorkloadOOMBump_ConsumedSignalStartsOver(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	oomAt := now.Add(-time.Minute)
	later := now
	live := qtyBytes(t, "200Mi")
	floor := qtyBytes(t, "300Mi")
	block := &attunev1alpha1.OOMBump{}
	pod := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)
	consumed := []oomBumpConsumed{{
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		OOMAt:     oomAt,
		Restart:   1,
	}}

	same := planWorkloadOOMBump(block, "app", false, 0, false, nil, "", []corev1.Pod{pod}, now, consumed)
	assert.True(t, same.Active)
	assert.False(t, same.UsePublish)
	assert.Empty(t, same.Stamps)
	assert.Empty(t, same.MetricNow)
	assert.Empty(t, same.WorkloadValue)

	fresh := pod.DeepCopy()
	fresh.Status.ContainerStatuses[0] = *oomKilledStatus(later, 2)
	fresh.Status.ContainerStatuses[0].Name = "app"
	next := planWorkloadOOMBump(block, "app", false, 0, false, nil, "", []corev1.Pod{*fresh}, now, consumed)
	require.Len(t, next.Stamps, 1)
	assert.Equal(t, 1, next.Stamps[0].Stamp.Count)
	assert.Equal(t, live, next.Stamps[0].Stamp.Origin)
	assert.Equal(t, floor, next.Stamps[0].Stamp.Floor)
	assert.Empty(t, next.MetricNow)
}

func TestPlanWorkloadOOMBump_BumpBytesStaysAtOriginMath(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	oomAt := now.Add(-time.Minute)
	origin := qtyBytes(t, "200Mi")
	floor := qtyBytes(t, "300Mi")
	percentile := qtyBytes(t, "400Mi")
	pod := oomBumpPod("a", "app", "200Mi", oomKilledStatus(oomAt, 1), "", false)

	plan := planWorkloadOOMBump(&attunev1alpha1.OOMBump{}, "app", false, percentile, true, nil, "", []corev1.Pod{pod}, now, nil)
	require.Len(t, plan.Stamps, 1)
	assert.False(t, plan.Stamps[0].AnnotationOnly)
	assert.Equal(t, origin, plan.Stamps[0].Stamp.Origin)
	assert.Equal(t, floor, plan.Stamps[0].bumpBytes)
	assert.Equal(t, percentile, plan.Stamps[0].Stamp.Floor)
	assert.Equal(t, percentile, plan.PublishBytes)
}

func TestPlanWorkloadOOMBump_CappedDoesNotRepeat(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	hold := now.Add(time.Hour)
	live200 := qtyBytes(t, "200Mi")
	floor300 := qtyBytes(t, "300Mi")
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 3, Origin: live200, Floor: floor300,
		OOMAt: now, Restart: 3, HoldUntil: hold,
	})
	require.NoError(t, err)
	pod := oomBumpPod("a", "app", "300Mi", oomKilledStatus(later, 4), raw, false)

	first := planWorkloadOOMBump(&attunev1alpha1.OOMBump{}, "app", false, 0, false, nil, raw, []corev1.Pod{pod}, now, nil)
	require.Len(t, first.Stamps, 1)
	stamp := first.Stamps[0]
	assert.True(t, stamp.AnnotationOnly)
	assert.Equal(t, 3, stamp.Stamp.Count)
	assert.Equal(t, live200, stamp.Stamp.Origin)
	assert.Equal(t, floor300, stamp.Stamp.Floor)
	assert.True(t, stamp.Stamp.OOMAt.Equal(later))
	assert.Equal(t, int32(4), stamp.Stamp.Restart)
	assert.True(t, stamp.Stamp.HoldUntil.Equal(hold))
	assert.Equal(t, []string{oomBumpCapped}, first.MetricNow)
	assert.Equal(t, "OOMBumpCapped", first.Event)
	assert.True(t, first.UsePublish)
	assert.Equal(t, floor300, first.PublishBytes)

	updated, err := formatOOMBumpRecord(stamp.Stamp)
	require.NoError(t, err)
	nextPod := oomBumpPod("a", "app", "300Mi", oomKilledStatus(later, 4), updated, false)
	second := planWorkloadOOMBump(&attunev1alpha1.OOMBump{}, "app", false, 0, false, nil, updated, []corev1.Pod{nextPod}, now, nil)
	assert.Empty(t, second.Stamps)
	assert.Empty(t, second.MetricNow)
	assert.Empty(t, second.Event)
	assert.True(t, second.UsePublish)
	assert.Equal(t, floor300, second.PublishBytes)
}

func TestPlanWorkloadOOMBump_InHoldNewOOMStepsAboveLive(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	origin := qtyBytes(t, "256Mi")
	floor := qtyBytes(t, "384Mi")
	live := qtyBytes(t, "1Gi")
	above := qtyBytes(t, "1280Mi")
	ratio := "1.5"
	block := &attunev1alpha1.OOMBump{Ratio: &ratio}
	minBump := attunev1alpha1.DefaultOOMBumpMinBump.Value()
	stored := oomBumpRecord{
		Count: 1, Origin: origin, Floor: floor,
		OOMAt: now.Add(-time.Hour), Restart: 1, HoldUntil: now.Add(time.Hour),
	}
	raw, err := formatOOMBumpRecord(stored)
	require.NoError(t, err)
	originStep, originResult, bumpErr := oomBumpBytes(origin, minBump, live, ratio, stored.Count+1, nil)
	require.NoError(t, bumpErr)
	assert.Equal(t, oomBumpSkipped, originResult, "frozen origin step is not above live")
	assert.LessOrEqual(t, originStep, live)

	tests := []struct {
		name         string
		newOOM       bool
		maxAllowed   *int64
		wantCount    int
		wantAbove    bool
		wantEqualMax bool
		wantSkipped  bool
		wantOnly     bool
	}{
		{name: "steps above live", newOOM: true, wantCount: 2, wantAbove: true},
		{name: "clamped to maxAllowed above live", newOOM: true, maxAllowed: &above, wantCount: 2, wantAbove: true, wantEqualMax: true},
		{name: "maxAllowed at live consumes once", newOOM: true, maxAllowed: &live, wantCount: 1, wantSkipped: true, wantOnly: true},
		{name: "no new oom keeps the floor", newOOM: false, wantCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finished := stored.OOMAt
			restart := stored.Restart
			if tt.newOOM {
				finished = later
				restart = 2
			}
			pod := oomBumpPod("a", "app", "1Gi", oomKilledStatus(finished, restart), raw, false)
			first := planWorkloadOOMBump(block, "app", false, 0, false, tt.maxAllowed, raw, []corev1.Pod{pod}, now, nil)
			require.True(t, first.UsePublish)
			if !tt.newOOM {
				assert.Equal(t, floor, first.PublishBytes)
				assert.Empty(t, first.Stamps)
				assert.Empty(t, first.MetricNow)
				return
			}
			require.Len(t, first.Stamps, 1)
			stamp := first.Stamps[0]
			assert.Equal(t, tt.wantOnly, stamp.AnnotationOnly)
			assert.Equal(t, origin, stamp.Stamp.Origin)
			assert.Equal(t, tt.wantCount, stamp.Stamp.Count)
			assert.True(t, stamp.Stamp.OOMAt.Equal(finished))
			assert.Equal(t, restart, stamp.Stamp.Restart)
			if tt.wantSkipped {
				assert.Equal(t, []string{oomBumpSkipped}, first.MetricNow)
				assert.Equal(t, floor, stamp.Stamp.Floor)
				assert.Equal(t, floor, first.PublishBytes)
			} else {
				assert.NotContains(t, first.MetricNow, oomBumpSkipped)
				assert.Greater(t, first.PublishBytes, live)
				assert.Greater(t, stamp.Stamp.Floor, live)
				if tt.wantEqualMax {
					assert.Equal(t, *tt.maxAllowed, first.PublishBytes)
					assert.Equal(t, oomBumpClamped, stamp.Result)
				}
			}

			updated, fmtErr := formatOOMBumpRecord(stamp.Stamp)
			require.NoError(t, fmtErr)
			nextPod := oomBumpPod("a", "app", "1Gi", oomKilledStatus(finished, restart), updated, false)
			second := planWorkloadOOMBump(block, "app", false, 0, false, tt.maxAllowed, updated, []corev1.Pod{nextPod}, now, nil)
			assert.Empty(t, second.MetricNow)
			for _, again := range second.Stamps {
				assert.Equal(t, tt.wantCount, again.Stamp.Count)
			}
		})
	}
}

func TestPlanWorkloadOOMBump_RevertedPodDoesNotInheritSibling(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	consumedAt := now.Add(-2 * time.Hour)
	beforeSibling := now.Add(-30 * time.Minute)
	siblingAt := now.Add(-time.Minute)
	afterSibling := now
	hold := now.Add(time.Hour)
	live100 := qtyBytes(t, "100Mi")
	bump100 := qtyBytes(t, "200Mi")
	origin512 := qtyBytes(t, "512Mi")
	siblingFloor := int64(773094114)
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 2, Origin: origin512, Floor: siblingFloor,
		OOMAt: siblingAt, Restart: 2, HoldUntil: hold,
	})
	require.NoError(t, err)

	reverted := oomBumpPod("reverted", "app", "100Mi", oomKilledStatus(beforeSibling, 3), "", false)
	sibling := oomBumpPod("sib", "app", "512Mi", nil, raw, false)
	consumed := []oomBumpConsumed{{
		Namespace: reverted.Namespace,
		PodName:   reverted.Name,
		OOMAt:     consumedAt,
		Restart:   1,
	}}
	block := &attunev1alpha1.OOMBump{}

	plan := planWorkloadOOMBump(block, "app", false, 0, false, nil, raw, []corev1.Pod{reverted, sibling}, now, consumed)
	require.Len(t, plan.Stamps, 1)
	assert.Equal(t, "reverted", plan.Stamps[0].PodName)
	assert.Equal(t, 1, plan.Stamps[0].Stamp.Count)
	assert.Equal(t, live100, plan.Stamps[0].Stamp.Origin)
	assert.Equal(t, bump100, plan.Stamps[0].bumpBytes)
	assert.Equal(t, siblingFloor, plan.Stamps[0].Stamp.Floor)
	assert.Equal(t, siblingFloor, plan.PublishBytes)
	work, ok := parseOOMBumpRecord(plan.WorkloadValue)
	require.True(t, ok)
	assert.Equal(t, 2, work.Count)
	assert.Equal(t, origin512, work.Origin)

	laterPod := reverted.DeepCopy()
	laterStatus := *oomKilledStatus(afterSibling, 4)
	laterStatus.Name = "app"
	laterPod.Status.ContainerStatuses[0] = laterStatus
	later := planWorkloadOOMBump(block, "app", false, 0, false, nil, raw, []corev1.Pod{*laterPod, sibling}, now, consumed)
	require.Len(t, later.Stamps, 1)
	assert.Equal(t, 1, later.Stamps[0].Stamp.Count)
	assert.Equal(t, live100, later.Stamps[0].Stamp.Origin)
	assert.Equal(t, bump100, later.Stamps[0].bumpBytes)
	assert.NotEqual(t, 3, later.Stamps[0].Stamp.Count)
}

func TestPlanWorkloadOOMBump_HeldFloorReclampsToMaxAllowed(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	floor := qtyBytes(t, "512Mi")
	trigger := now.Add(-time.Hour)
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: floor, Floor: floor,
		OOMAt: trigger, Restart: 1, HoldUntil: now.Add(time.Hour),
	})
	require.NoError(t, err)
	pod := oomBumpPod("a", "app", "512Mi", oomKilledStatus(trigger, 1), raw, false)
	block := &attunev1alpha1.OOMBump{}

	cap256 := qtyBytes(t, "256Mi")
	clamped := planWorkloadOOMBump(block, "app", false, 0, false, &cap256, raw, []corev1.Pod{pod}, now, nil)
	assert.True(t, clamped.UsePublish)
	assert.Equal(t, cap256, clamped.PublishBytes)
	assert.True(t, clamped.QuietClamp)
	assert.Equal(t, floor, clamped.QuietClampFrom)
	assert.NotContains(t, clamped.MetricNow, oomBumpClamped)
	assert.Empty(t, clamped.Stamps)
	stored, ok := parseOOMBumpRecord(clamped.WorkloadValue)
	require.True(t, ok)
	assert.Equal(t, floor, stored.Floor)
	assert.Equal(t, raw, clamped.WorkloadValue)

	above := qtyBytes(t, "1Gi")
	under := planWorkloadOOMBump(block, "app", false, 0, false, &above, raw, []corev1.Pod{pod}, now, nil)
	assert.Equal(t, floor, under.PublishBytes)
	assert.False(t, under.QuietClamp)

	uncapped := planWorkloadOOMBump(block, "app", false, 0, false, nil, raw, []corev1.Pod{pod}, now, nil)
	assert.Equal(t, floor, uncapped.PublishBytes)
	assert.False(t, uncapped.QuietClamp)

	tied := planWorkloadOOMBump(block, "app", false, floor, true, &cap256, raw, []corev1.Pod{pod}, now, nil)
	assert.Equal(t, cap256, tied.PublishBytes)
	assert.True(t, tied.QuietClamp)
	assert.Equal(t, floor, tied.QuietClampFrom)

	higher := qtyBytes(t, "1Gi")
	left := planWorkloadOOMBump(block, "app", false, higher, true, &cap256, raw, []corev1.Pod{pod}, now, nil)
	assert.Equal(t, higher, left.PublishBytes, "a higher settled percentile is not a held-floor reclamp")
	assert.False(t, left.QuietClamp)

	plain := oomBumpPod("c", "app", "512Mi", nil, "", false)
	onlyPct := planWorkloadOOMBump(block, "app", false, floor, true, &cap256, "", []corev1.Pod{plain}, now, nil)
	assert.Equal(t, floor, onlyPct.PublishBytes)
	assert.False(t, onlyPct.QuietClamp)
	assert.False(t, onlyPct.Note)

	freshCap := qtyBytes(t, "256Mi")
	fresh := oomBumpPod("b", "app", "200Mi", oomKilledStatus(now, 1), "", false)
	stepped := planWorkloadOOMBump(block, "app", false, 0, false, &freshCap, "", []corev1.Pod{fresh}, now, nil)
	assert.Equal(t, freshCap, stepped.PublishBytes)
	assert.False(t, stepped.QuietClamp)
	require.Len(t, stepped.Stamps, 1)
	assert.Equal(t, oomBumpClamped, stepped.Stamps[0].Result)
	assert.Equal(t, freshCap, stepped.Stamps[0].Stamp.Floor)
	assert.NotContains(t, stepped.MetricNow, oomBumpClamped)

	combined := planWorkloadOOMBump(block, "app", false, 0, false, &freshCap, raw, []corev1.Pod{fresh}, now, nil)
	assert.Equal(t, freshCap, combined.PublishBytes)
	assert.False(t, combined.QuietClamp)
	require.NotEmpty(t, combined.Stamps)
	for _, st := range combined.Stamps {
		if st.AnnotationOnly {
			continue
		}
		assert.GreaterOrEqual(t, st.Stamp.Floor, freshCap)
	}
}

func TestStripClearedOOMBumps(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	key, ok := oomBumpKey("app")
	require.True(t, ok)
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: now.Add(-time.Minute), Restart: 1, HoldUntil: now.Add(time.Hour),
	})
	require.NoError(t, err)
	other, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 2, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "400Mi"),
		OOMAt: now, Restart: 2, HoldUntil: now.Add(time.Hour),
	})
	require.NoError(t, err)

	cleared := oomBumpPod("a", "app", "200Mi", nil, raw, false)
	cleared.Annotations["keep"] = "yes"
	sibling := oomBumpPod("b", "app", "300Mi", nil, raw, false)
	caller := []corev1.Pod{cleared, sibling}
	clear := oomBumpClear{
		namespace: cleared.Namespace,
		podName:   cleared.Name,
		container: "app",
		raw:       raw,
		oomAt:     now.Add(-time.Minute),
		restart:   1,
	}

	out, workloadRaw, consumed := stripClearedOOMBumps([]oomBumpClear{clear}, "app", caller, raw)
	assert.Equal(t, raw, caller[0].Annotations[key], "caller pod annotations stay")
	assert.Equal(t, "yes", caller[0].Annotations["keep"])
	assert.Equal(t, raw, workloadRaw, "sibling still holds the workload value")
	require.Len(t, consumed, 1)
	assert.True(t, consumed[0].OOMAt.Equal(clear.oomAt))
	assert.Equal(t, int32(1), consumed[0].Restart)
	_, dropped := out[0].Annotations[key]
	assert.False(t, dropped)
	assert.Equal(t, "yes", out[0].Annotations["keep"])
	assert.Equal(t, raw, out[1].Annotations[key])

	alone := []corev1.Pod{cleared}
	_, blanked, consumedAlone := stripClearedOOMBumps([]oomBumpClear{clear}, "app", alone, raw)
	assert.Empty(t, blanked)
	require.Len(t, consumedAlone, 1)
	assert.Equal(t, raw, alone[0].Annotations[key])

	_, kept, _ := stripClearedOOMBumps([]oomBumpClear{clear}, "app", alone, other)
	assert.Equal(t, other, kept)
}

func TestOOMBumpShouldClearAfterRevert(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	resizedAt := now.Add(-time.Minute)
	triggerAt := resizedAt.Add(-time.Second)
	raw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: triggerAt, Restart: 1, HoldUntil: now.Add(time.Hour),
	})
	require.NoError(t, err)
	pod := oomBumpPod("a", "app", "300Mi", oomKilledStatus(triggerAt, 1), raw, false)
	errPod := pod.DeepCopy()
	errPod.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 2,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", FinishedAt: metav1.NewTime(now),
		}},
	}
	expiredRaw, err := formatOOMBumpRecord(oomBumpRecord{
		Count: 1, Origin: qtyBytes(t, "200Mi"), Floor: qtyBytes(t, "300Mi"),
		OOMAt: triggerAt, Restart: 1, HoldUntil: now.Add(-time.Second),
	})
	require.NoError(t, err)
	expired := oomBumpPod("a", "app", "300Mi", oomKilledStatus(triggerAt, 1), expiredRaw, false)

	assert.True(t, oomBumpShouldClearAfterRevert(errPod, "app", "Error", resizedAt, now, 3))
	assert.True(t, oomBumpShouldClearAfterRevert(errPod, "app", "restart", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(&pod, "app", "oomkill", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(&pod, "app", "throttle", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(&pod, "app", "notready", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(&pod, "app", "slo:latency", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(errPod, "app", "re-fetch-failed", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(errPod, "app", "annotation-persist-failed", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(errPod, "app", "annotation-persist-conflict", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(&expired, "app", "Error", resizedAt, now, 3))
	assert.False(t, oomBumpShouldClearAfterRevert(nil, "app", "Error", resizedAt, now, 3))
}
