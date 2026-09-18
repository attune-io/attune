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
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/testutil/kubeletsim"
)

// replayEvent is one compact row of the recommend → resize → observe trace.
type replayEvent struct {
	At         time.Time
	UsageCPU   float64
	RequestCPU string
	RecCPU     string
	Action     string
}

func (e replayEvent) String() string {
	return fmt.Sprintf("%s usage=%.3f req=%s rec=%s action=%s",
		e.At.UTC().Format(time.RFC3339), e.UsageCPU, e.RequestCPU, e.RecCPU, e.Action)
}

type usageSeries func(t time.Time) (cpuCores, memBytes float64)

func sineUsage(epoch time.Time, period time.Duration, cpuMid, cpuAmp, memMid, memAmp float64) usageSeries {
	return func(t time.Time) (float64, float64) {
		phase := 2 * math.Pi * float64(t.Sub(epoch)) / float64(period)
		s := math.Sin(phase)
		return cpuMid + cpuAmp*s, memMid + memAmp*s
	}
}

func stepUsage(at time.Time, beforeCPU, afterCPU, beforeMem, afterMem float64) usageSeries {
	return func(t time.Time) (float64, float64) {
		if t.Before(at) {
			return beforeCPU, beforeMem
		}
		return afterCPU, afterMem
	}
}

func spikeUsage(start, end time.Time, baseCPU, spikeCPU, baseMem, spikeMem float64) usageSeries {
	return func(t time.Time) (float64, float64) {
		if !t.Before(start) && t.Before(end) {
			return spikeCPU, spikeMem
		}
		return baseCPU, baseMem
	}
}

func seriesSamples(start, end time.Time, step time.Duration, val func(time.Time) float64) []rsmetrics.Sample {
	if step <= 0 {
		step = 5 * time.Minute
	}
	n := int(end.Sub(start)/step) + 1
	if n < 1 {
		n = 1
	}
	out := make([]rsmetrics.Sample, 0, n)
	for ts := start; !ts.After(end); ts = ts.Add(step) {
		out = append(out, rsmetrics.Sample{Timestamp: ts, Value: val(ts)})
	}
	return out
}

type replayOpts struct {
	horizon      time.Duration
	step         time.Duration
	cooldown     time.Duration
	observe      time.Duration
	series       usageSeries
	startCPU     string
	startMem     string
	minCPU       string
	maxCPU       string
	minMem       string
	maxMem       string
	cpuBudget    string
	maxChangeCPU int32
}

type replayResult struct {
	events   []replayEvent
	resizes  []replayEvent
	requests []resource.Quantity
	policy   attunev1alpha1.AttunePolicy
}

func runReplay(t *testing.T, opts replayOpts) replayResult {
	t.Helper()
	epoch := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	now := epoch

	policy := newTestPolicy("replay-policy", "default")
	policy.Finalizers = []string{finalizerName}
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: opts.cooldown}
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: opts.observe}
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)
	// Change-filter Current is the pod template. Persist after resize so
	// the next cycle can keep climbing instead of re-capping from start.
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceAfterSuccessfulResize,
	}
	policy.Spec.MetricsSource.MinimumDataPoints = int32Ptr(12)
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 6 * time.Hour}
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 5 * time.Minute}
	policy.Spec.CPU.Percentile = 95
	policy.Spec.CPU.Overhead = "20"
	policy.Spec.CPU.MinAllowed = quantityPtr(opts.minCPU)
	policy.Spec.CPU.MaxAllowed = quantityPtr(opts.maxCPU)
	if opts.maxChangeCPU == 0 {
		opts.maxChangeCPU = 50
	}
	policy.Spec.CPU.MaxChangePercent = int32Ptr(opts.maxChangeCPU)
	policy.Spec.Memory.Percentile = 99
	policy.Spec.Memory.Overhead = "30"
	policy.Spec.Memory.MinAllowed = quantityPtr(opts.minMem)
	policy.Spec.Memory.MaxAllowed = quantityPtr(opts.maxMem)
	policy.Spec.Memory.MaxChangePercent = int32Ptr(30)
	if opts.cpuBudget != "" {
		q := resource.MustParse(opts.cpuBudget)
		policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &q
	}

	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	deploy.Spec.Replicas = int32Ptr(1)
	cpuQ := resource.MustParse(opts.startCPU)
	memQ := resource.MustParse(opts.startMem)
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = cpuQ
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = memQ
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4000m"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}

	pod := newResizePod("api-server", opts.startCPU, opts.startMem, "4000m", "8Gi")
	pod.Labels = map[string]string{"app": "api-server"}
	pod.Spec.Containers[0].Name = "main"
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "main",
		Ready:        true,
		RestartCount: 0,
	}}

	mc := &mockCollector{
		queryRangeFunc: func(_ context.Context, query string, start, end time.Time, step time.Duration) ([]rsmetrics.Sample, error) {
			// processWorkloads builds the Prom window from time.Now.
			// Shift it so the window ends at the fake clock.
			window := end.Sub(start)
			if window <= 0 {
				window = 6 * time.Hour
			}
			fakeEnd := now
			fakeStart := fakeEnd.Add(-window)
			switch {
			case strings.Contains(query, "container_cpu_usage"):
				return seriesSamples(fakeStart, fakeEnd, step, func(ts time.Time) float64 {
					cpu, _ := opts.series(ts)
					return cpu
				}), nil
			case strings.Contains(query, "container_memory"):
				return seriesSamples(fakeStart, fakeEnd, step, func(ts time.Time) float64 {
					_, mem := opts.series(ts)
					return mem
				}), nil
			default:
				return nil, nil
			}
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ensureTestNamespaces([]client.Object{policy, deploy, pod})...).
		WithStatusSubresource(&attunev1alpha1.AttunePolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cw client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if p, ok := obj.(*corev1.Pod); ok {
					var cur corev1.Pod
					if err := cw.Get(ctx, client.ObjectKeyFromObject(p), &cur); err == nil {
						p.SetResourceVersion(cur.ResourceVersion)
						p.SetUID(cur.UID)
					}
				}
				return cw.Update(ctx, obj, opts...)
			},
		}).
		Build()
	cs := kubefake.NewSimpleClientset(pod.DeepCopy())
	kubeletsim.Install(cs, kubeletsim.Options{
		Outcome: kubeletsim.Accepted,
		Now:     func() time.Time { return now },
	})

	r := newReconcilerForReconcileWithClient(mc, c, scheme)
	r.Clientset = cs
	r.SetNowFunc(func() time.Time { return now })

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "replay-policy", Namespace: "default"}}
	var out replayResult
	prevCPU := opts.startCPU
	deadline := epoch.Add(opts.horizon)

	for now.Before(deadline) {
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err, "reconcile at %s", now.UTC().Format(time.RFC3339))

		live, err := cs.CoreV1().Pods("default").Get(context.Background(), pod.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, syncReplayPod(c, cs, live))

		var updated attunev1alpha1.AttunePolicy
		require.NoError(t, c.Get(context.Background(), req.NamespacedName, &updated))

		cur := live.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
		recCPU := ""
		if len(updated.Status.Recommendations) > 0 && len(updated.Status.Recommendations[0].Containers) > 0 {
			recCPU = updated.Status.Recommendations[0].Containers[0].Recommended.CPURequest.String()
		}
		action := "none"
		if cur.String() != prevCPU {
			action = "resize"
		}
		cpu, _ := opts.series(now)
		ev := replayEvent{
			At:         now,
			UsageCPU:   cpu,
			RequestCPU: cur.String(),
			RecCPU:     recCPU,
			Action:     action,
		}
		out.events = append(out.events, ev)
		if action == "resize" {
			out.resizes = append(out.resizes, ev)
			out.requests = append(out.requests, cur)
			prevCPU = cur.String()
		}

		now = now.Add(opts.step)
		r.SetNowFunc(func() time.Time { return now })
	}
	_ = c.Get(context.Background(), req.NamespacedName, &out.policy)
	return out
}

func syncReplayPod(c client.Client, cs *kubefake.Clientset, live *corev1.Pod) error {
	var cached corev1.Pod
	key := types.NamespacedName{Name: live.Name, Namespace: live.Namespace}
	if err := c.Get(context.Background(), key, &cached); err != nil {
		return err
	}
	cached.Spec = live.Spec
	cached.Status = live.Status
	if live.Annotations != nil {
		if cached.Annotations == nil {
			cached.Annotations = map[string]string{}
		}
		for k, v := range live.Annotations {
			cached.Annotations[k] = v
		}
	}
	if live.Labels != nil {
		if cached.Labels == nil {
			cached.Labels = map[string]string{}
		}
		for k, v := range live.Labels {
			cached.Labels[k] = v
		}
	}
	if err := c.Update(context.Background(), &cached); err != nil {
		return err
	}
	if err := c.Get(context.Background(), key, &cached); err != nil {
		return err
	}
	// persistResizeAnnotations Updates the controller-runtime client with the
	// Clientset resourceVersion. Keep the two stores on the same RV.
	return cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), cached.DeepCopy(), cached.Namespace)
}

func dumpReplay(t *testing.T, res replayResult) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "resizes=%d\n", len(res.resizes))
	for _, ev := range res.resizes {
		b.WriteString(ev.String())
		b.WriteByte('\n')
	}
	if n := len(res.events); n > 8 {
		b.WriteString("tail:\n")
		for _, ev := range res.events[n-8:] {
			b.WriteString(ev.String())
			b.WriteByte('\n')
		}
	}
	t.Log(b.String())
}

func assertRequestsInBounds(t *testing.T, res replayResult, minS, maxS string) {
	t.Helper()
	minQ := resource.MustParse(minS)
	maxQ := resource.MustParse(maxS)
	for _, q := range res.requests {
		require.False(t, q.Cmp(minQ) < 0, "request %s below minAllowed %s", q.String(), minS)
		require.False(t, q.Cmp(maxQ) > 0, "request %s above maxAllowed %s", q.String(), maxS)
	}
}

func assertNoImmediateReversal(t *testing.T, res replayResult, window time.Duration) {
	t.Helper()
	if len(res.resizes) < 2 {
		return
	}
	var lastUpAt time.Time
	var lastUp resource.Quantity
	haveUp := false
	prev := resource.MustParse(res.resizes[0].RequestCPU)
	for i := 1; i < len(res.resizes); i++ {
		cur := resource.MustParse(res.resizes[i].RequestCPU)
		if cur.Cmp(prev) > 0 {
			lastUpAt = res.resizes[i].At
			lastUp = cur
			haveUp = true
		} else if haveUp && cur.Cmp(lastUp) < 0 && res.resizes[i].At.Sub(lastUpAt) <= window {
			require.Failf(t, "immediate reversal",
				"up then down within %s:\n  %s\n  %s",
				window, res.resizes[i-1], res.resizes[i])
		}
		prev = cur
	}
}

func TestReplay_SineWaveBoundedNoFlap(t *testing.T) {
	epoch := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	cooldown := 1 * time.Hour
	observe := time.Minute
	res := runReplay(t, replayOpts{
		horizon:  7 * 24 * time.Hour,
		step:     15 * time.Minute,
		cooldown: cooldown,
		observe:  observe,
		series:   sineUsage(epoch, 24*time.Hour, 0.30, 0.15, 256<<20, 32<<20),
		startCPU: "500m",
		startMem: "512Mi",
		minCPU:   "50m",
		maxCPU:   "2000m",
		minMem:   "64Mi",
		maxMem:   "4Gi",
	})
	t.Logf("sine resizes=%d", len(res.resizes))
	if len(res.resizes) > 96 {
		dumpReplay(t, res)
		t.Fatalf("sine resize count %d exceeds bound 96", len(res.resizes))
	}
	require.NotEmpty(t, res.resizes, "sine series should produce at least one resize from 500m")
	assertRequestsInBounds(t, res, "50m", "2000m")
	assertNoImmediateReversal(t, res, cooldown+observe)
}

func TestReplay_StepChangeSettles(t *testing.T) {
	epoch := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	stepAt := epoch.Add(36 * time.Hour)
	cooldown := 30 * time.Minute
	observe := time.Minute
	res := runReplay(t, replayOpts{
		horizon:  5 * 24 * time.Hour,
		step:     15 * time.Minute,
		cooldown: cooldown,
		observe:  observe,
		series:   stepUsage(stepAt, 0.10, 0.80, 200<<20, 400<<20),
		startCPU: "500m",
		startMem: "512Mi",
		minCPU:   "50m",
		maxCPU:   "2000m",
		minMem:   "64Mi",
		maxMem:   "4Gi",
	})
	t.Logf("step resizes=%d", len(res.resizes))
	if len(res.resizes) > 16 {
		dumpReplay(t, res)
		t.Fatalf("step resize count %d exceeds bound 16", len(res.resizes))
	}
	require.NotEmpty(t, res.resizes)
	assertRequestsInBounds(t, res, "50m", "2000m")
	assertNoImmediateReversal(t, res, cooldown+observe)

	cutoff := epoch.Add(36*time.Hour + 12*time.Hour)
	for _, ev := range res.resizes {
		if ev.At.After(cutoff) {
			dumpReplay(t, res)
			t.Fatalf("resize after step settled: %s", ev)
		}
	}
}

func TestReplay_SpikeDoesNotOscillate(t *testing.T) {
	epoch := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	spikeStart := epoch.Add(12 * time.Hour)
	spikeEnd := spikeStart.Add(20 * time.Minute)
	cooldown := 30 * time.Minute
	observe := time.Minute
	res := runReplay(t, replayOpts{
		horizon:  3 * 24 * time.Hour,
		step:     15 * time.Minute,
		cooldown: cooldown,
		observe:  observe,
		series:   spikeUsage(spikeStart, spikeEnd, 0.20, 1.50, 200<<20, 300<<20),
		startCPU: "300m",
		startMem: "256Mi",
		minCPU:   "50m",
		maxCPU:   "2000m",
		minMem:   "64Mi",
		maxMem:   "4Gi",
	})
	t.Logf("spike resizes=%d", len(res.resizes))
	if len(res.resizes) > 8 {
		dumpReplay(t, res)
		t.Fatalf("short spike produced %d resizes, bound is 8", len(res.resizes))
	}
	assertRequestsInBounds(t, res, "50m", "2000m")
	assertNoImmediateReversal(t, res, cooldown+observe)
}

func TestReplay_IncreaseBudgetHonoured(t *testing.T) {
	epoch := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	cooldown := 30 * time.Minute
	observe := time.Minute
	budget := resource.MustParse("100m")
	res := runReplay(t, replayOpts{
		horizon:      8 * time.Hour,
		step:         15 * time.Minute,
		cooldown:     cooldown,
		observe:      observe,
		series:       stepUsage(epoch.Add(time.Hour), 0.15, 1.20, 200<<20, 400<<20),
		startCPU:     "200m",
		startMem:     "256Mi",
		minCPU:       "50m",
		maxCPU:       "2000m",
		minMem:       "64Mi",
		maxMem:       "4Gi",
		cpuBudget:    "100m",
		maxChangeCPU: 25,
	})
	t.Logf("budget resizes=%d", len(res.resizes))
	if len(res.resizes) < 3 {
		dumpReplay(t, res)
		for _, h := range res.policy.Status.ResizeHistory {
			t.Logf("history %s %s %s %s %s->%s %s %s", h.Workload, h.Container, h.Resource, h.Result, h.From, h.To, h.Method, h.Reason)
		}
	}
	assertRequestsInBounds(t, res, "50m", "2000m")
	assertNoImmediateReversal(t, res, cooldown+observe)

	prev := resource.MustParse("200m")
	increases := 0
	for _, ev := range res.resizes {
		cur := resource.MustParse(ev.RequestCPU)
		if cur.Cmp(prev) > 0 {
			increases++
			delta := cur.DeepCopy()
			delta.Sub(prev)
			require.False(t, delta.Cmp(budget) > 0,
				"increase %s exceeds cycle budget 100m at %s (%s -> %s)",
				delta.String(), ev.At.UTC().Format(time.RFC3339), prev.String(), cur.String())
		}
		prev = cur
	}
	require.GreaterOrEqual(t, increases, 3, "sustained high usage must climb in budget-sized steps")
}
