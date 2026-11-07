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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

// defaultMaxPodsInMetricsQuery caps how many pods are named in a metrics
// pod=~ regex when representative sampling is enabled (0 = unlimited regex).
const defaultMaxPodsInMetricsQuery = 100

const (
	// errCouldNotListPods is the status text when both the namespace
	// pod list and the per-workload list fail.
	errCouldNotListPods = "could not list pods"
	// errNoPodSelector is the status text when a workload has nothing
	// to match. Savings keeps one recommendation diff for that name.
	errNoPodSelector = "no pod selector"
)

// listPodsForWorkloads lists pods once per namespace and matches each workload's
// selector in memory. This avoids N List calls when many workloads share a
// namespace (common for large policies).
func (r *AttunePolicyReconciler) listPodsForWorkloads(ctx context.Context, workloads []client.Object) (map[string][]corev1.Pod, map[string]string) {
	logger := log.FromContext(ctx)
	out := make(map[string][]corev1.Pod, len(workloads))
	if len(workloads) == 0 {
		return out, nil
	}
	var unlisted map[string]string
	noteUnlisted := func(name, msg string) {
		if unlisted == nil {
			unlisted = map[string]string{}
		}
		if _, exists := unlisted[name]; exists {
			return
		}
		if msg == "" {
			msg = errCouldNotListPods
		}
		unlisted[name] = msg
	}

	// Group workloads by namespace.
	byNS := make(map[string][]client.Object)
	for _, w := range workloads {
		ns := w.GetNamespace()
		byNS[ns] = append(byNS[ns], w)
	}

	for ns, ws := range byNS {
		var podList corev1.PodList
		if err := r.List(ctx, &podList, client.InNamespace(ns)); err != nil {
			logger.Error(err, "Failed to list pods in namespace for workload matching", "namespace", ns)
			// Fall back to per-workload lists so a single NS failure does not
			// blank the entire map.
			for _, w := range ws {
				pods, err := r.getPodsForWorkload(ctx, w)
				if err != nil {
					logger.Error(err, "Failed to get pods for workload", "workload", w.GetName())
					noteUnlisted(w.GetName(), listOrSelectorMessage(err))
					continue
				}
				out[w.GetName()] = pods
			}
			continue
		}

		// Precompute selectors once.
		type matchSpec struct {
			name string
			sel  labels.Selector
		}
		specs := make([]matchSpec, 0, len(ws))
		for _, w := range ws {
			sel, err := r.podSelector(w)
			if err != nil || sel == nil {
				msg := errNoPodSelector
				logErr := err
				if err != nil {
					msg = trimStatusError("pod selector: " + err.Error())
				} else {
					logErr = fmt.Errorf("%s", errNoPodSelector)
				}
				logger.Error(logErr, "Skipping workload pod match",
					"workload", w.GetName(), "namespace", ns)
				noteUnlisted(w.GetName(), msg)
				continue
			}
			specs = append(specs, matchSpec{name: w.GetName(), sel: sel})
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			for _, s := range specs {
				if s.sel.Matches(labels.Set(pod.Labels)) {
					out[s.name] = append(out[s.name], *pod)
				}
			}
		}
	}
	return out, unlisted
}

// unlistedWorkloadNames returns workloads whose pods could not be matched,
// in stable name order. The map value is the status error text.
func unlistedWorkloadNames(unlisted map[string]string) []string {
	if len(unlisted) == 0 {
		return nil
	}
	names := make([]string, 0, len(unlisted))
	for name := range unlisted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// listOrSelectorMessage maps a per-workload pod lookup error to a short
// status string. A selector problem stays distinct from a list failure
// so savings can keep one recommendation diff either way.
func listOrSelectorMessage(err error) string {
	if err == nil {
		return errCouldNotListPods
	}
	text := err.Error()
	if strings.Contains(text, "no pod selector") {
		return errNoPodSelector
	}
	if strings.Contains(text, "parsing pod selector") {
		return trimStatusError("pod selector: " + text)
	}
	return errCouldNotListPods
}

func trimStatusError(msg string) string {
	const max = 160
	if len(msg) <= max {
		return msg
	}
	return cutUTF8Bytes(msg, max)
}

// cutUTF8Bytes keeps at most n bytes and does not end inside a multi-byte
// character. A string that is already within n is returned unchanged.
func cutUTF8Bytes(msg string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(msg) <= n {
		return msg
	}
	i := n
	for i > 0 && !utf8.RuneStart(msg[i]) {
		i--
	}
	if i < n {
		_, size := utf8.DecodeRuneInString(msg[i:])
		if i+size > n {
			return msg[:i]
		}
	}
	return msg[:i]
}

// labelsMatch reports whether podLabels contain every key/value in selector.
func labelsMatch(podLabels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// samplePodsForMetrics returns at most maxN pods with deterministic selection
// (sorted by name, then evenly spaced indices for stable coverage).
// When maxN <= 0 or len(pods) <= maxN, returns pods unchanged.
func samplePodsForMetrics(pods []corev1.Pod, maxN int) []corev1.Pod {
	if maxN <= 0 || len(pods) <= maxN {
		return pods
	}
	// Sort for stable input order.
	sorted := make([]corev1.Pod, len(pods))
	copy(sorted, pods)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	// Evenly spaced sample across the sorted list.
	out := make([]corev1.Pod, 0, maxN)
	for i := 0; i < maxN; i++ {
		idx := i * len(sorted) / maxN
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		out = append(out, sorted[idx])
	}
	// Dedupe in case of small N rounding collisions.
	seen := make(map[string]bool, len(out))
	deduped := out[:0]
	for _, p := range out {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		deduped = append(deduped, p)
	}
	return deduped
}

// podRegexFromNames builds a PromQL-safe alternation regex for exact pod names.
func podRegexFromNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	escaped := make([]string, len(names))
	for i, n := range names {
		escaped[i] = rsmetrics.EscapePromQLRegex(n)
	}
	sort.Strings(escaped)
	return strings.Join(escaped, "|")
}

// metricsPodRegex chooses the workload regex or a sampled exact-name regex
// when the pod set is large and MaxPodsInMetricsQuery is set.
func (r *AttunePolicyReconciler) metricsPodRegex(workload client.Object, pods []corev1.Pod) string {
	maxN := r.maxPodsInMetricsQuery()
	if maxN <= 0 || len(pods) == 0 || len(pods) <= maxN {
		return r.getPodRegex(workload)
	}
	sampled := samplePodsForMetrics(pods, maxN)
	names := make([]string, len(sampled))
	for i := range sampled {
		names[i] = sampled[i].Name
	}
	if re := podRegexFromNames(names); re != "" {
		return re
	}
	return r.getPodRegex(workload)
}

func (r *AttunePolicyReconciler) maxPodsInMetricsQuery() int {
	if r.MaxPodsInMetricsQuery < 0 {
		return 0 // unlimited
	}
	if r.MaxPodsInMetricsQuery > 0 {
		return r.MaxPodsInMetricsQuery
	}
	return defaultMaxPodsInMetricsQuery
}

// shouldRefreshBlockers returns true when Deferred/Infeasible status should be
// recomputed. Always true when actively resizing (needPods). When only listing
// for status UX, respects BlockerRefreshInterval to cut work under many policies.
func (r *AttunePolicyReconciler) shouldRefreshBlockers(policy *attunev1alpha1.AttunePolicy, needPods bool) bool {
	if needPods || r.BlockerRefreshInterval <= 0 {
		return true
	}
	key := types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}
	if v, ok := r.lastBlockerRefresh.Load(key); ok {
		if last, ok := v.(time.Time); ok && r.now().Sub(last) < r.BlockerRefreshInterval {
			return false
		}
	}
	return true
}

func (r *AttunePolicyReconciler) markBlockersRefreshed(policy *attunev1alpha1.AttunePolicy) {
	if r.BlockerRefreshInterval <= 0 {
		return
	}
	key := types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}
	r.lastBlockerRefresh.Store(key, r.now())
}
