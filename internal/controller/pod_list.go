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

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/argorollout"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/textutil"
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
	r.claimPodsOnce(ctx, logger, workloads, out)
	return out, unlisted
}

// claimPodsOnce keeps each pod on one workload. Overlapping selectors
// otherwise add the same request gap twice and can resize the pod twice
// in one reconcile. The workload that owns the pod keeps it. When none
// of the matches own it, the name that sorts first does.
func (r *AttunePolicyReconciler) claimPodsOnce(ctx context.Context, logger logr.Logger, workloads []client.Object, out map[string][]corev1.Pod) {
	if len(out) < 2 {
		return
	}
	byName := make(map[string]client.Object, len(workloads))
	for _, w := range workloads {
		byName[w.GetName()] = w
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)
	grouped := make(map[string][]podCand)
	var keys []string
	for _, name := range names {
		for i := range out[name] {
			pod := out[name][i]
			key := podClaimKey(&pod)
			if _, ok := grouped[key]; !ok {
				keys = append(keys, key)
			}
			grouped[key] = append(grouped[key], podCand{name: name, pod: pod})
		}
		out[name] = []corev1.Pod{}
	}
	var rsOwner, jobOwner map[types.UID]string
	if podClaimOverlaps(grouped) {
		rsOwner = r.replicaSetOwners(ctx, logger, workloads)
		jobOwner = r.jobCronJobOwners(ctx, logger, workloads)
	}
	for _, key := range keys {
		cands := grouped[key]
		winner := cands[0].name
		pod := cands[0].pod
		if ref, ok := controllerOwner(&pod); ok {
			switch ref.Kind {
			case "ReplicaSet":
				// A Deployment or Rollout owns the pod through its ReplicaSet.
				// A standalone ReplicaSet policy has no such parent, so the
				// ReplicaSet name itself is the owner.
				if name := rsOwner[ref.UID]; name != "" && candHas(cands, name) {
					winner = name
				} else if name, ok := directOwner(cands, byName, ref); ok {
					winner = name
				}
			case "Job":
				// A CronJob pod's controller is the Job. Prefer that CronJob
				// when it is one of the matches. Otherwise a Job policy still
				// matches the Job by name.
				if name := jobOwner[ref.UID]; name != "" && candHas(cands, name) {
					winner = name
				} else if name, ok := directOwner(cands, byName, ref); ok {
					winner = name
				}
			default:
				if name, ok := directOwner(cands, byName, ref); ok {
					winner = name
				}
			}
		}
		for _, c := range cands {
			if c.name == winner {
				pod = c.pod
				break
			}
		}
		out[winner] = append(out[winner], pod)
		for _, c := range cands {
			if c.name == winner {
				continue
			}
			logger.V(1).Info("pod matches more than one workload; counting it once",
				"pod", pod.Namespace+"/"+pod.Name,
				"workload", winner,
				"skipped", c.name)
		}
	}
}

type podCand struct {
	name string
	pod  corev1.Pod
}

func podClaimOverlaps(grouped map[string][]podCand) bool {
	for _, cands := range grouped {
		if len(cands) > 1 {
			return true
		}
	}
	return false
}

func directOwner(cands []podCand, byName map[string]client.Object, ref metav1.OwnerReference) (string, bool) {
	for _, c := range cands {
		w := byName[c.name]
		if w != nil && objectKind(w) == ref.Kind && ref.Name == c.name {
			return c.name, true
		}
	}
	return "", false
}

func candHas(cands []podCand, name string) bool {
	for _, c := range cands {
		if c.name == name {
			return true
		}
	}
	return false
}

func controllerOwner(pod *corev1.Pod) (metav1.OwnerReference, bool) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Name != "" {
			return ref, true
		}
	}
	return metav1.OwnerReference{}, false
}

func objectKind(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *appsv1.ReplicaSet:
		return "ReplicaSet"
	case *batchv1.Job:
		return "Job"
	case *batchv1.CronJob:
		return "CronJob"
	case *argorollout.Rollout:
		return "Rollout"
	default:
		return ""
	}
}

// replicaSetOwners maps a ReplicaSet UID to the Deployment or Rollout
// name in this policy that controls it. A list error leaves the map
// empty and claimPodsOnce falls back to alphabetical names.
func (r *AttunePolicyReconciler) replicaSetOwners(ctx context.Context, logger logr.Logger, workloads []client.Object) map[types.UID]string {
	out := map[types.UID]string{}
	if r == nil || r.Client == nil {
		return out
	}
	// Key is namespace, controller kind, and name. A Deployment and a
	// Rollout can share a name. Matching the name alone would credit
	// the other kind's pods to this policy.
	type ownerKey struct {
		kind string
		name string
	}
	byNS := map[string]map[ownerKey]struct{}{}
	for _, w := range workloads {
		kind := objectKind(w)
		if kind != "Deployment" && kind != "Rollout" {
			continue
		}
		ns := w.GetNamespace()
		if byNS[ns] == nil {
			byNS[ns] = map[ownerKey]struct{}{}
		}
		byNS[ns][ownerKey{kind: kind, name: w.GetName()}] = struct{}{}
	}
	for ns, owners := range byNS {
		var list appsv1.ReplicaSetList
		if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
			logger.Error(err, "Failed to list ReplicaSets for pod ownership", "namespace", ns)
			continue
		}
		for i := range list.Items {
			rs := &list.Items[i]
			for _, ref := range rs.OwnerReferences {
				if ref.Controller == nil || !*ref.Controller || ref.Name == "" {
					continue
				}
				if _, ok := owners[ownerKey{kind: ref.Kind, name: ref.Name}]; !ok {
					continue
				}
				if rs.UID != "" {
					out[rs.UID] = ref.Name
				}
			}
		}
	}
	return out
}

// jobCronJobOwners maps a Job UID to the CronJob name in this policy that
// controls it. A CronJob pod's controller is the Job, not the CronJob.
// A list error leaves the map empty and claimPodsOnce falls back to a
// direct Job name, then to alphabetical names.
func (r *AttunePolicyReconciler) jobCronJobOwners(ctx context.Context, logger logr.Logger, workloads []client.Object) map[types.UID]string {
	out := map[types.UID]string{}
	if r == nil || r.Client == nil {
		return out
	}
	type ownerKey struct {
		kind string
		name string
	}
	byNS := map[string]map[ownerKey]struct{}{}
	for _, w := range workloads {
		if objectKind(w) != "CronJob" {
			continue
		}
		ns := w.GetNamespace()
		if byNS[ns] == nil {
			byNS[ns] = map[ownerKey]struct{}{}
		}
		byNS[ns][ownerKey{kind: "CronJob", name: w.GetName()}] = struct{}{}
	}
	for ns, owners := range byNS {
		var list batchv1.JobList
		if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
			logger.Error(err, "Failed to list Jobs for pod ownership", "namespace", ns)
			continue
		}
		for i := range list.Items {
			job := &list.Items[i]
			for _, ref := range job.OwnerReferences {
				if ref.Controller == nil || !*ref.Controller || ref.Name == "" {
					continue
				}
				if ref.Kind != "CronJob" {
					continue
				}
				if ref.APIVersion != "batch/v1" && ref.APIVersion != "batch/v1beta1" {
					continue
				}
				if _, ok := owners[ownerKey{kind: ref.Kind, name: ref.Name}]; !ok {
					continue
				}
				if job.UID != "" {
					out[job.UID] = ref.Name
				}
			}
		}
	}
	return out
}

func podClaimKey(pod *corev1.Pod) string {
	if pod.UID != "" {
		return string(pod.UID)
	}
	return pod.Namespace + "/" + pod.Name
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
	return textutil.CutUTF8Bytes(msg, 160)
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
