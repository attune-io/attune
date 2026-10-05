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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

// excludeStartupHistory reports whether CPU samples collected during
// startup boost should be left out of the percentile. Nil and false
// keep today's percentile. Only an explicit true opts in.
func excludeStartupHistory(policy *attunev1alpha1.AttunePolicy) bool {
	if policy == nil || policy.Spec.CPU.StartupBoost == nil {
		return false
	}
	flag := policy.Spec.CPU.StartupBoost.ExcludeFromHistory
	return flag != nil && *flag
}

// startupHistoryFilter is the CPU sample slice after startup exclusion.
// Dropped is set when at least one in-scope point was removed.
// Skipped is set when at least one series was left unfiltered.
type startupHistoryFilter struct {
	Samples []rsmetrics.Sample
	Dropped bool
	Skipped bool
}

// startupExclusionStart is where CPU history exclusion begins for one
// live pod. A readable attune.io/startup-boost-at at or after creation
// is the start. A missing, malformed, or earlier stamp is ignored so a
// reused pod name does not lose samples from before this pod existed.
func startupExclusionStart(pod *corev1.Pod) time.Time {
	created := pod.CreationTimestamp.Time
	if pod.Annotations == nil {
		return created
	}
	raw := pod.Annotations[annotationStartupBoostAt]
	if raw == "" {
		return created
	}
	stamp, err := time.Parse(time.RFC3339, raw)
	if err != nil || stamp.Before(created) {
		return created
	}
	return stamp
}

// filterStartupCPUSamples drops CPU points that fall inside a live pod's
// startup window, then re-aggregates the surviving pod-labeled series.
//
// The window starts at startupExclusionStart. The cutoff is that start
// plus boost plus rateWindow. A point exactly at the cutoff stays.
// Samples older than CreationTimestamp stay. Sample timestamps are the
// end of rate(). Deleted pods have no creation time and no stamp, so
// their series stay until historyWindow. A series with an empty pod label
// is left unchanged and is not folded into Max or Avg. A numeric 0 after
// the cutoff stays. Pods with no surviving points are omitted.
func filterStartupCPUSamples(samples []rsmetrics.Sample, pods []corev1.Pod, boost time.Duration, rateWindow time.Duration, mode rsmetrics.PodAggregationMode) startupHistoryFilter {
	type podWindow struct {
		created time.Time
		start   time.Time
	}
	windows := make(map[string]podWindow, len(pods))
	for i := range pods {
		windows[pods[i].Name] = podWindow{
			created: pods[i].CreationTimestamp.Time,
			start:   startupExclusionStart(&pods[i]),
		}
	}

	byPod := make(map[string][]rsmetrics.Sample)
	var podOrder []string
	var unlabeled []rsmetrics.Sample
	dropped := false
	skipped := false

	for _, sample := range samples {
		if sample.Pod == "" {
			unlabeled = append(unlabeled, sample)
			skipped = true
			continue
		}
		win, live := windows[sample.Pod]
		if !live {
			// No live pod with this name. Keep the series. Do not mark
			// Skipped: deleted pods stay until historyWindow.
			if _, seen := byPod[sample.Pod]; !seen {
				podOrder = append(podOrder, sample.Pod)
			}
			byPod[sample.Pod] = append(byPod[sample.Pod], sample)
			continue
		}
		cutoff := win.start.Add(boost).Add(rateWindow)
		// A reused pod name keeps points older than this pod. A stamp
		// before creation is not a window, so those points stay.
		inThisPod := !sample.Timestamp.Before(win.created)
		inWindow := !sample.Timestamp.Before(win.start) && sample.Timestamp.Before(cutoff)
		if inThisPod && inWindow {
			dropped = true
			continue
		}
		if _, seen := byPod[sample.Pod]; !seen {
			podOrder = append(podOrder, sample.Pod)
		}
		byPod[sample.Pod] = append(byPod[sample.Pod], sample)
	}

	reduced := reduceStartupPodSeries(byPod, podOrder, mode)
	out := make([]rsmetrics.Sample, 0, len(reduced)+len(unlabeled))
	out = append(out, reduced...)
	out = append(out, unlabeled...)
	return startupHistoryFilter{
		Samples: out,
		Dropped: dropped,
		Skipped: skipped,
	}
}

// reduceStartupPodSeries aggregates surviving pod-labeled series.
// Max (and empty mode) takes the max value at each timestamp.
// Avg averages pods that still have a point at that timestamp.
// None concatenates the surviving labeled points.
func reduceStartupPodSeries(byPod map[string][]rsmetrics.Sample, podOrder []string, mode rsmetrics.PodAggregationMode) []rsmetrics.Sample {
	if len(byPod) == 0 {
		return nil
	}
	if mode == rsmetrics.PodAggregationNone {
		all := make([]rsmetrics.Sample, 0)
		for _, name := range podOrder {
			all = append(all, byPod[name]...)
		}
		sort.SliceStable(all, func(i, j int) bool {
			return all[i].Timestamp.Before(all[j].Timestamp)
		})
		return all
	}

	type bucket struct {
		ts    time.Time
		max   float64
		sum   float64
		count int
	}
	buckets := make(map[int64]*bucket)
	var keys []int64
	for _, name := range podOrder {
		for _, sample := range byPod[name] {
			key := sample.Timestamp.UnixNano()
			acc, ok := buckets[key]
			if !ok {
				acc = &bucket{ts: sample.Timestamp, max: sample.Value}
				buckets[key] = acc
				keys = append(keys, key)
			}
			if sample.Value > acc.max {
				acc.max = sample.Value
			}
			acc.sum += sample.Value
			acc.count++
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	avg := mode == rsmetrics.PodAggregationAvg
	out := make([]rsmetrics.Sample, 0, len(keys))
	for _, key := range keys {
		acc := buckets[key]
		value := acc.max
		if avg && acc.count > 0 {
			value = acc.sum / float64(acc.count)
		}
		out = append(out, rsmetrics.Sample{Timestamp: acc.ts, Value: value})
	}
	return out
}
