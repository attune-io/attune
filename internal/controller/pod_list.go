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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

// defaultMaxPodsInMetricsQuery caps how many pods are named in a metrics
// pod=~ regex when representative sampling is enabled (0 = unlimited regex).
const defaultMaxPodsInMetricsQuery = 100

// listPodsForWorkloads lists pods once per namespace and matches each workload's
// selector in memory. This avoids N List calls when many workloads share a
// namespace (common for large policies).
func (r *AttunePolicyReconciler) listPodsForWorkloads(ctx context.Context, workloads []client.Object) map[string][]corev1.Pod {
	logger := log.FromContext(ctx)
	out := make(map[string][]corev1.Pod, len(workloads))
	if len(workloads) == 0 {
		return out
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
					continue
				}
				out[w.GetName()] = pods
			}
			continue
		}

		// Precompute selectors once.
		type matchSpec struct {
			name   string
			labels map[string]string
		}
		specs := make([]matchSpec, 0, len(ws))
		for _, w := range ws {
			sel := r.getPodSelectorLabels(w)
			if len(sel) == 0 {
				logger.Error(fmt.Errorf("no pod selector"), "Skipping workload pod match",
					"workload", w.GetName(), "namespace", ns)
				continue
			}
			specs = append(specs, matchSpec{name: w.GetName(), labels: sel})
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			for _, s := range specs {
				if labelsMatch(pod.Labels, s.labels) {
					out[s.name] = append(out[s.name], *pod)
				}
			}
		}
	}
	return out
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
