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

package e2e_go

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

const (
	oomBumpContainer  = "app"
	oomBumpAnnotation = "attune.io/oom-bump.app"

	// oomPhase1Poll is how often phase 1 reads the named pod.
	oomPhase1Poll = 2 * time.Second
	// oomPhase1Limit covers the stressor's 120s self-exit plus oomPhase1Grace.
	oomPhase1Limit = 150 * time.Second
	// oomPhase1Grace is the wait after the exec returns before a missing
	// OOMKilled is a harness failure. The 150s cap still applies when the
	// stream never returns.
	oomPhase1Grace = 30 * time.Second
	// oomPhase2Limit is the bump wait, measured from phase 1 success.
	oomPhase2Limit   = 4 * time.Minute
	oomExecTailBytes = 4 * 1024
)

// sinceExecFinish is how long the stressor exec has been finished.
// now is passed in so the grace boundary does not depend on the wall clock.
// A nil result means the exec is still running.
func sinceExecFinish(res *oomExecResult, now time.Time) time.Duration {
	if res == nil {
		return 0
	}
	return now.Sub(res.finished)
}

// phase1GiveUp reports whether phase 1 must fail closed.
// execFinished is true only after the stressor exec has returned.
func phase1GiveUp(execFinished bool, elapsed, sinceFinish time.Duration) bool {
	if elapsed >= oomPhase1Limit {
		return true
	}
	if !execFinished {
		return false
	}
	return sinceFinish >= oomPhase1Grace
}

type oomExecResult struct {
	err      error
	elapsed  time.Duration
	tail     string
	finished time.Time
}

// oomExecTail keeps the last 4KiB written by stdout and stderr together.
// StreamWithContext writes those streams concurrently, so Write is locked.
// Callers copy the tail out with snapshot only after the stream returns.
type oomExecTail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newOOMExecTail() *oomExecTail {
	return &oomExecTail{max: oomExecTailBytes}
}

func (w *oomExecTail) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) >= w.max {
		fresh := make([]byte, w.max)
		copy(fresh, p[len(p)-w.max:])
		w.buf = fresh
		return len(p), nil
	}
	combined := make([]byte, len(w.buf)+len(p))
	copy(combined, w.buf)
	copy(combined[len(w.buf):], p)
	if len(combined) > w.max {
		tail := make([]byte, w.max)
		copy(tail, combined[len(combined)-w.max:])
		combined = tail
	}
	w.buf = combined
	return len(p), nil
}

func (w *oomExecTail) snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func formatOOMExecStatus(res *oomExecResult, runningFor time.Duration) string {
	if res == nil {
		secs := int(runningFor.Round(time.Second).Seconds())
		if secs < 0 {
			secs = 0
		}
		return fmt.Sprintf("still running after %ds", secs)
	}
	elapsed := res.elapsed.Round(time.Millisecond)
	if res.err != nil {
		return fmt.Sprintf("error after %s: %v", elapsed, res.err)
	}
	return fmt.Sprintf("nil error after %s", elapsed)
}

func containerMemoryRequest(pod *corev1.Pod, name string) *resource.Quantity {
	return containerMemoryQuantity(pod, name, false)
}

func containerMemoryLimit(pod *corev1.Pod, name string) *resource.Quantity {
	return containerMemoryQuantity(pod, name, true)
}

func containerMemoryQuantity(pod *corev1.Pod, name string, limit bool) *resource.Quantity {
	if pod == nil {
		return nil
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != name {
			continue
		}
		list := pod.Spec.Containers[i].Resources.Requests
		if limit {
			list = pod.Spec.Containers[i].Resources.Limits
		}
		q, ok := list[corev1.ResourceMemory]
		if !ok {
			return nil
		}
		out := q.DeepCopy()
		return &out
	}
	return nil
}

func podOOMKilled(pod *corev1.Pod, name string) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != name {
			continue
		}
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			return true
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

func recommendationIsOOMBump(policy *attunev1alpha1.AttunePolicy, workload string, start resource.Quantity) bool {
	if policy == nil {
		return false
	}
	for _, rec := range policy.Status.Recommendations {
		if rec.Workload != workload {
			continue
		}
		for _, c := range rec.Containers {
			if c.Name != oomBumpContainer || c.Recommended.MemoryRequest.Cmp(start) <= 0 {
				continue
			}
			if c.Explanation != nil && c.Explanation.Memory != nil &&
				strings.Contains(c.Explanation.Memory.FinalAdjustment, "oomBump") {
				return true
			}
		}
	}
	return false
}

// oomBumpReady reports whether the OOM bump product conditions hold.
// The workload name is the policy target, which the live test sets to the
// Deployment name stored on Recommendation.Workload.
func oomBumpReady(pods []corev1.Pod, status appsv1.DeploymentStatus, policy *attunev1alpha1.AttunePolicy, start resource.Quantity, replicas int32) bool {
	if status.UpdatedReplicas != replicas || status.ReadyReplicas < replicas-1 {
		return false
	}
	workload := ""
	if policy != nil && policy.Spec.TargetRef.Name != nil {
		workload = *policy.Spec.TargetRef.Name
	}
	oomRaised := false
	siblingRaised := false
	for i := range pods {
		pod := &pods[i]
		mem := containerMemoryRequest(pod, oomBumpContainer)
		if mem == nil || mem.Cmp(start) <= 0 {
			continue
		}
		if podOOMKilled(pod, oomBumpContainer) {
			raw := pod.Annotations[oomBumpAnnotation]
			if strings.HasPrefix(raw, "count=1,") {
				oomRaised = true
			}
			continue
		}
		siblingRaised = true
	}
	return oomRaised && siblingRaised && recommendationIsOOMBump(policy, workload, start)
}

func containerStateSummary(st corev1.ContainerState) string {
	switch {
	case st.Terminated != nil:
		return fmt.Sprintf("terminated reason=%s exit=%d", st.Terminated.Reason, st.Terminated.ExitCode)
	case st.Waiting != nil:
		return fmt.Sprintf("waiting reason=%s", st.Waiting.Reason)
	case st.Running != nil:
		return "running"
	default:
		return "empty"
	}
}

func stressorPodDetail(pod *corev1.Pod) string {
	if pod == nil {
		return "request=unknown limit=unknown phase=unknown state=unknown lastState=unknown restartCount=unknown"
	}
	state, last, restarts := "unknown", "unknown", "unknown"
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != oomBumpContainer {
			continue
		}
		state = containerStateSummary(cs.State)
		last = containerStateSummary(cs.LastTerminationState)
		restarts = fmt.Sprintf("%d", cs.RestartCount)
		break
	}
	req, lim := "none", "none"
	if q := containerMemoryRequest(pod, oomBumpContainer); q != nil {
		req = q.String()
	}
	if q := containerMemoryLimit(pod, oomBumpContainer); q != nil {
		lim = q.String()
	}
	return fmt.Sprintf("request=%s limit=%s phase=%s state=%s lastState=%s restartCount=%s",
		req, lim, pod.Status.Phase, state, last, restarts)
}

func stressorMissMessage(podName string, pod *corev1.Pod, exec *oomExecResult, runningFor time.Duration, notFound bool) string {
	msg := fmt.Sprintf("stressor did not OOM-kill pod %s: %s exec=%s",
		podName, stressorPodDetail(pod), formatOOMExecStatus(exec, runningFor))
	if notFound {
		msg += "; pod NotFound"
	}
	if exec != nil {
		msg += "\noutput tail:\n" + exec.tail
	}
	return msg
}

func TestOOMBumpDecision(t *testing.T) {
	t.Parallel()

	t.Run("deadlines", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, 2*time.Second, oomPhase1Poll)
		require.Equal(t, 150*time.Second, oomPhase1Limit)
		require.Equal(t, 30*time.Second, oomPhase1Grace)
		require.Equal(t, 4*time.Minute, oomPhase2Limit)
		require.Equal(t, 4*1024, oomExecTailBytes)

		require.False(t, phase1GiveUp(false, oomPhase1Limit-time.Nanosecond, 0))
		require.True(t, phase1GiveUp(false, oomPhase1Limit, 0))
		require.False(t, phase1GiveUp(true, 10*time.Second, oomPhase1Grace-time.Nanosecond))
		require.True(t, phase1GiveUp(true, 10*time.Second, oomPhase1Grace))
		require.True(t, phase1GiveUp(true, oomPhase1Limit, time.Nanosecond))

		now := time.Unix(1_700_000_000, 0)
		require.Equal(t, time.Duration(0), sinceExecFinish(nil, now))
		early := &oomExecResult{finished: now.Add(-oomPhase1Grace + time.Nanosecond)}
		require.False(t, phase1GiveUp(true, 10*time.Second, sinceExecFinish(early, now)))
		due := &oomExecResult{finished: now.Add(-oomPhase1Grace)}
		require.True(t, phase1GiveUp(true, 10*time.Second, sinceExecFinish(due, now)))
	})

	t.Run("phase 1 OOMKilled", func(t *testing.T) {
		t.Parallel()
		last := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 oomBumpContainer,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
		}}}}
		require.True(t, podOOMKilled(last, oomBumpContainer), "lastState OOMKilled counts")

		current := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  oomBumpContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
		}}}}
		require.True(t, podOOMKilled(current, oomBumpContainer), "current terminated OOMKilled counts")

		running := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  oomBumpContainer,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}}}
		require.False(t, podOOMKilled(running, oomBumpContainer), "running with empty lastState does not count")

		other := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "sidecar",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
		}}}}
		require.False(t, podOOMKilled(other, oomBumpContainer), "another container does not count")
		require.False(t, podOOMKilled(nil, oomBumpContainer), "a missing pod does not count")
	})

	t.Run("phase 2", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			oomMem  string
			oomAnn  string
			sibMem  string
			adjust  string
			updated int32
			ready   int32
			want    bool
		}{
			{
				name: "request raised, count=1, sibling raised, oomBump", oomMem: "128Mi",
				oomAnn: "count=1,origin=observed", sibMem: "128Mi", adjust: "oomBump",
				updated: 3, ready: 2, want: true,
			},
			{
				name: "request still 64Mi", oomMem: "64Mi",
				oomAnn: "count=1,origin=observed", sibMem: "128Mi", adjust: "oomBump",
				updated: 3, ready: 2, want: false,
			},
			{
				name: "annotation missing", oomMem: "128Mi",
				sibMem: "128Mi", adjust: "oomBump",
				updated: 3, ready: 2, want: false,
			},
			{
				name: "count=2", oomMem: "128Mi",
				oomAnn: "count=2,origin=observed", sibMem: "128Mi", adjust: "oomBump",
				updated: 3, ready: 2, want: false,
			},
			{
				name: "sibling request still 64Mi", oomMem: "128Mi",
				oomAnn: "count=1,origin=observed", sibMem: "64Mi", adjust: "oomBump",
				updated: 3, ready: 2, want: false,
			},
			{
				name: "adjustment is usage only", oomMem: "128Mi",
				oomAnn: "count=1,origin=observed", sibMem: "128Mi",
				adjust:  "podAggregation=Max; burstSensitivity=0.1",
				updated: 3, ready: 2, want: false,
			},
			{
				name: "updated replicas are not 3", oomMem: "128Mi",
				oomAnn: "count=1,origin=observed", sibMem: "128Mi", adjust: "oomBump",
				updated: 2, ready: 2, want: false,
			},
			{
				name: "ready replicas below 2", oomMem: "128Mi",
				oomAnn: "count=1,origin=observed", sibMem: "128Mi", adjust: "oomBump",
				updated: 3, ready: 1, want: false,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				start := mustQuantity(t, "64Mi")
				pods := []corev1.Pod{
					testPod(t, "oom", tc.oomMem, true, tc.oomAnn),
					testPod(t, "sib", tc.sibMem, false, ""),
				}
				status := appsv1.DeploymentStatus{UpdatedReplicas: tc.updated, ReadyReplicas: tc.ready}
				got := oomBumpReady(pods, status, decisionPolicy(t, "128Mi", tc.adjust), start, 3)
				require.Equal(t, tc.want, got)
			})
		}
	})

	t.Run("exec tail and status", func(t *testing.T) {
		t.Parallel()
		w := newOOMExecTail()
		n, err := w.Write(nil)
		require.NoError(t, err)
		require.Equal(t, 0, n)

		first := bytes.Repeat([]byte("a"), 3000)
		second := bytes.Repeat([]byte("b"), 3000)
		_, err = w.Write(first)
		require.NoError(t, err)
		_, err = w.Write(second)
		require.NoError(t, err)
		got := w.snapshot()
		require.Equal(t, oomExecTailBytes, len(got))
		require.Equal(t, bytes.Repeat([]byte("a"), oomExecTailBytes-3000), []byte(got[:oomExecTailBytes-3000]))
		require.Equal(t, second, []byte(got[len(got)-len(second):]))

		big := newOOMExecTail()
		payload := bytes.Repeat([]byte("c"), oomExecTailBytes+50)
		n, err = big.Write(payload)
		require.NoError(t, err)
		require.Equal(t, len(payload), n)
		require.Equal(t, payload[len(payload)-oomExecTailBytes:], []byte(big.snapshot()))

		shared := newOOMExecTail()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = shared.Write(bytes.Repeat([]byte("o"), 100))
		}()
		go func() {
			defer wg.Done()
			_, _ = shared.Write(bytes.Repeat([]byte("e"), 100))
		}()
		wg.Wait()
		merged := shared.snapshot()
		require.Equal(t, 200, len(merged))
		require.Equal(t, 100, strings.Count(merged, "o"))
		require.Equal(t, 100, strings.Count(merged, "e"))

		require.Equal(t, "still running after 150s", formatOOMExecStatus(nil, 150*time.Second))
		require.Equal(t, "nil error after 1.5s", formatOOMExecStatus(&oomExecResult{elapsed: time.Second + 500*time.Millisecond}, 0))
		require.Equal(t, "error after 12s: stream closed", formatOOMExecStatus(&oomExecResult{
			err:     fmt.Errorf("stream closed"),
			elapsed: 12 * time.Second,
		}, 0))
	})

	t.Run("harness message", func(t *testing.T) {
		t.Parallel()
		bare := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: oomBumpContainer}}}}
		require.Nil(t, containerMemoryRequest(bare, oomBumpContainer))
		require.Nil(t, containerMemoryLimit(bare, oomBumpContainer))
		require.Nil(t, containerMemoryRequest(nil, oomBumpContainer))
		require.Contains(t, stressorPodDetail(nil), "request=unknown")
		require.Contains(t, stressorPodDetail(bare), "request=none")
		require.Contains(t, stressorPodDetail(bare), "limit=none")

		pod := testPod(t, "oom-1", "64Mi", false, "")
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: mustQuantity(t, "512Mi"),
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  oomBumpContainer,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		running := stressorMissMessage("oom-1", &pod, nil, 150*time.Second, false)
		require.Contains(t, running, "stressor did not OOM-kill pod oom-1")
		require.Contains(t, running, "request=64Mi")
		require.Contains(t, running, "limit=512Mi")
		require.Contains(t, running, "phase=Running")
		require.Contains(t, running, "state=running")
		require.Contains(t, running, "lastState=empty")
		require.Contains(t, running, "restartCount=0")
		require.Contains(t, running, "still running after 150s")
		require.NotContains(t, running, "output tail")
		require.NotContains(t, running, "OOM bump")
		require.NotContains(t, running, "oom bump")

		done := &oomExecResult{err: fmt.Errorf("broken pipe"), elapsed: 2 * time.Second, tail: "alloc failed"}
		finished := stressorMissMessage("oom-1", &pod, done, 0, true)
		require.Contains(t, finished, "error after 2s: broken pipe")
		require.Contains(t, finished, "output tail:\nalloc failed")
		require.Contains(t, finished, "pod NotFound")
		require.Contains(t, finished, "request=64Mi")
		require.Contains(t, finished, "limit=512Mi")
		require.NotContains(t, finished, "OOM bump")
	})
}

func mustQuantity(t *testing.T, raw string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(raw)
	require.NoError(t, err)
	return q
}

func testPod(t *testing.T, name, mem string, oomKilled bool, ann string) corev1.Pod {
	t.Helper()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: oomBumpContainer,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: mustQuantity(t, mem)},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ann != "" {
		pod.Annotations = map[string]string{oomBumpAnnotation: ann}
	}
	if oomKilled {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: oomBumpContainer,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
			},
		}}
	}
	return pod
}

func decisionPolicy(t *testing.T, mem, adjustment string) *attunev1alpha1.AttunePolicy {
	t.Helper()
	name := "oom-bump-app"
	return &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Name: &name},
		},
		Status: attunev1alpha1.AttunePolicyStatus{
			Recommendations: []attunev1alpha1.WorkloadRecommendation{{
				Workload: name,
				Containers: []attunev1alpha1.ContainerRecommendation{{
					Name:        oomBumpContainer,
					Recommended: attunev1alpha1.ResourceValues{MemoryRequest: mustQuantity(t, mem)},
					Explanation: &attunev1alpha1.ContainerRecommendationExplanation{
						Memory: &attunev1alpha1.ResourceRecommendationExplanation{FinalAdjustment: adjustment},
					},
				}},
			}},
		},
	}
}
