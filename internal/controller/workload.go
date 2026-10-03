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
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/argorollout"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
)

// discoverWorkloads finds workloads matching the policy's targetRef.
func (r *AttunePolicyReconciler) discoverWorkloads(ctx context.Context, policy *attunev1alpha1.AttunePolicy) ([]client.Object, error) {
	targetRef := policy.Spec.TargetRef
	if targetRef.Kind == argorollout.Kind {
		missing, err := r.rolloutCRDMissing()
		if err != nil {
			return nil, err
		}
		if missing {
			return nil, errRolloutCRDMissing
		}
	}
	namespace := policy.Namespace

	// If a specific name is set, get that workload directly.
	if targetRef.Name != nil && *targetRef.Name != "" {
		workload, err := r.getWorkloadByName(ctx, namespace, targetRef.Kind, *targetRef.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return []client.Object{workload}, nil
	}

	// Otherwise, list workloads matching the label selector.
	if targetRef.Selector != nil {
		return r.listWorkloadsBySelector(ctx, namespace, targetRef.Kind, targetRef.Selector)
	}

	return nil, fmt.Errorf("targetRef must specify either name or selector")
}

// getWorkloadByName fetches a specific workload by kind and name.
func (r *AttunePolicyReconciler) getWorkloadByName(ctx context.Context, namespace, kind, name string) (client.Object, error) {
	wk, ok := workloadKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported workload kind: %s; supported kinds are: %s", kind, attunev1alpha1.SupportedTargetKindsCSV)
	}
	obj := wk.newObject()
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		return nil, err
	}
	// Reject owned ReplicaSets to prevent double-resizing.
	if kind == "ReplicaSet" && isDeploymentOwned(obj) {
		return nil, fmt.Errorf("ReplicaSet %s/%s is owned by a Deployment; target the Deployment instead", namespace, name)
	}
	if kind == "ReplicaSet" && isRolloutOwned(obj) {
		return nil, fmt.Errorf("ReplicaSet %s/%s is owned by a Rollout; target the Rollout instead", namespace, name)
	}
	return obj, nil
}

// listWorkloadsBySelector lists workloads matching a label selector.
func (r *AttunePolicyReconciler) listWorkloadsBySelector(ctx context.Context, namespace, kind string, selector *metav1.LabelSelector) ([]client.Object, error) {
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("parsing label selector: %w", err)
	}

	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labelSelector},
	}

	wk, ok := workloadKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported workload kind: %s; supported kinds are: %s", kind, attunev1alpha1.SupportedTargetKindsCSV)
	}

	list := wk.newList()
	if err := r.List(ctx, list, listOpts...); err != nil {
		return nil, err
	}
	extracted := wk.extract(list)
	// Filter out ReplicaSets owned by a Deployment or a Rollout.
	if kind == "ReplicaSet" {
		extracted = filterStandaloneReplicaSets(extracted)
	}
	return extracted, nil
}

// getPodsForWorkload returns the pods managed by a workload by matching
// the workload's pod template selector labels.
func (r *AttunePolicyReconciler) getPodsForWorkload(ctx context.Context, workload client.Object) ([]corev1.Pod, error) {
	sel, err := r.podSelector(workload)
	if err != nil {
		return nil, fmt.Errorf("parsing pod selector for workload %s: %w", workload.GetName(), err)
	}
	if sel == nil {
		return nil, fmt.Errorf("workload %s/%s has no pod selector labels", workload.GetNamespace(), workload.GetName())
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(workload.GetNamespace()),
		client.MatchingLabelsSelector{Selector: sel},
	); err != nil {
		return nil, fmt.Errorf("listing pods for workload %s: %w", workload.GetName(), err)
	}

	return podList.Items, nil
}

// podSelector returns the workload pod selector, including MatchExpressions.
func (r *AttunePolicyReconciler) podSelector(workload client.Object) (labels.Selector, error) {
	if a := newWorkloadAdapter(workload); a != nil {
		return a.PodSelector()
	}
	return nil, nil
}

// getPodSelectorLabels extracts the pod selector labels from a workload.
func (r *AttunePolicyReconciler) getPodSelectorLabels(workload client.Object) map[string]string {
	if a := newWorkloadAdapter(workload); a != nil {
		return a.PodSelectorLabels()
	}
	return nil
}

// getContainers returns the container specs from a workload's pod template,
// including native sidecar containers (init containers with restartPolicy=Always).
func (r *AttunePolicyReconciler) getContainers(workload client.Object) []corev1.Container {
	a := newWorkloadAdapter(workload)
	if a == nil {
		return nil
	}
	spec := a.PodSpec()
	if spec == nil {
		return nil
	}
	containers := nativeSidecars(spec.InitContainers)
	return append(containers, spec.Containers...)
}

// nativeSidecars returns init containers that have restartPolicy=Always,
// which makes them run for the pod's lifetime (KEP-753, stable since K8s 1.29).
func nativeSidecars(initContainers []corev1.Container) []corev1.Container {
	var sidecars []corev1.Container
	for _, c := range initContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars = append(sidecars, c)
		}
	}
	return sidecars
}

// isBatchWorkload returns true for Job and CronJob workloads. These only
// support Observe/Recommend modes; in-place resize is not applicable.
func isBatchWorkload(workload client.Object) bool {
	if a := newWorkloadAdapter(workload); a != nil {
		return a.IsBatch()
	}
	return false
}

// isRollingOut is the whole-workload resize skip. Recommendations still run.
// Nil spec.replicas is not a rollout. Zero generations are not stale.
func (r *AttunePolicyReconciler) isRollingOut(workload client.Object) bool {
	if a := newWorkloadAdapter(workload); a != nil {
		return a.IsRollingOut()
	}
	return false
}

// getPodRegex returns a PromQL regex that matches pods belonging to the given
// workload. It uses kind-specific suffix patterns to avoid matching pods from
// similarly-named workloads (e.g., "my-app" vs "my-app-v2").
//
// Patterns by kind:
//   - Deployment: <name>-<replicaset-hash>-<pod-hash>
//   - Rollout: same suffix as Deployment (pods are owned by ReplicaSets)
//   - StatefulSet: <name>-<ordinal>
//   - DaemonSet: <name>-<pod-hash>
//   - Job: <name>-<pod-hash>
//   - CronJob: <name>-<timestamp>-<pod-hash>
func (r *AttunePolicyReconciler) getPodRegex(workload client.Object) string {
	name := rsmetrics.EscapePromQLRegex(workload.GetName())
	if a := newWorkloadAdapter(workload); a != nil {
		return name + a.PodNameRegexSuffix()
	}
	// Unknown kinds: fall back to prefix match.
	return name + ".*"
}

const (
	// podTemplateGenerationAnnotation is the legacy DaemonSet revision
	// identity used when controller-revision-hash is empty.
	podTemplateGenerationAnnotation = "pod-template-generation"
	deploymentRevisionAnnotation    = "deployment.kubernetes.io/revision"
)

// generationStale is true only when the spec generation is ahead of the
// status. Both zero is not stale: fixtures and fresh objects use that.
func generationStale(generation, observed int64) bool {
	return generation > observed
}

func deploymentUsesRollingUpdate(d *appsv1.Deployment) bool {
	if d == nil {
		return false
	}
	t := d.Spec.Strategy.Type
	return t == "" || t == appsv1.RollingUpdateDeploymentStrategyType
}

func statefulSetUsesRollingUpdate(s *appsv1.StatefulSet) bool {
	if s == nil {
		return false
	}
	t := s.Spec.UpdateStrategy.Type
	return t == "" || t == appsv1.RollingUpdateStatefulSetStrategyType
}

func daemonSetUsesRollingUpdate(d *appsv1.DaemonSet) bool {
	if d == nil {
		return false
	}
	t := d.Spec.UpdateStrategy.Type
	return t == "" || t == appsv1.RollingUpdateDaemonSetStrategyType
}

// templatePersistenceBlockedByRollout is the template-write skip. It is
// stricter than IsRollingOut for paused Deployments and for StatefulSet
// revisions, and it is not the per-pod resize helper.
func templatePersistenceBlockedByRollout(w client.Object) bool {
	switch o := w.(type) {
	case *appsv1.Deployment:
		return deploymentTemplateMidReplacement(o)
	case *appsv1.StatefulSet:
		return statefulSetTemplateMidReplacement(o)
	default:
		return false
	}
}

func deploymentTemplateMidReplacement(d *appsv1.Deployment) bool {
	if d == nil || !deploymentUsesRollingUpdate(d) {
		return false
	}
	if generationStale(d.Generation, d.Status.ObservedGeneration) {
		return true
	}
	return d.Status.Replicas > d.Status.UpdatedReplicas
}

func statefulSetTemplateMidReplacement(s *appsv1.StatefulSet) bool {
	if s == nil || !statefulSetUsesRollingUpdate(s) {
		return false
	}
	if generationStale(s.Generation, s.Status.ObservedGeneration) {
		return true
	}
	if s.Status.UpdateRevision == "" || s.Status.CurrentRevision == s.Status.UpdateRevision {
		return false
	}
	if s.Spec.Replicas == nil {
		return false
	}
	var partition int32
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		partition = *ru.Partition
	}
	target := int64(*s.Spec.Replicas) - int64(partition)
	return int64(s.Status.UpdatedReplicas) < target
}

func (r *AttunePolicyReconciler) emitRolloutInProgress(policy *attunev1alpha1.AttunePolicy, workload client.Object) {
	name := workload.GetName()
	messageFmt := "Resize deferred for workload %s: rollout in progress"
	args := []any{name}
	if ro, ok := workload.(*argorollout.Rollout); ok {
		if ro.Status.Abort {
			messageFmt = "Resize deferred for workload %s: rollout in progress (phase %s, abort true)"
			args = []any{name, ro.Status.Phase}
		} else {
			messageFmt = "Resize deferred for workload %s: rollout in progress (phase %s)"
			args = []any{name, ro.Status.Phase}
		}
	}
	r.emitEventOnce(policy, corev1.EventTypeNormal, "RolloutInProgress", "resize", messageFmt, args...)
}

// podSkippedForRollout reports a per-pod resize skip. currentHash is the
// DaemonSet ControllerRevision name, or the paused Deployment's current
// pod-template-hash. An empty hash means the lookup does not apply, or it
// failed for a kind other than a RollingUpdate DaemonSet. That empty hash
// does not skip the pod. A RollingUpdate DaemonSet whose ControllerRevision
// list fails is handled in filterRolloutPods and skips every pod.
func (r *AttunePolicyReconciler) podSkippedForRollout(workload client.Object, pod *corev1.Pod, currentHash string) bool {
	if pod == nil {
		return false
	}
	switch w := workload.(type) {
	case *appsv1.StatefulSet:
		return statefulSetPodSkipped(w, pod)
	case *appsv1.DaemonSet:
		return daemonSetPodSkipped(w, pod, currentHash)
	case *appsv1.Deployment:
		return pausedDeploymentPodSkipped(w, pod, currentHash)
	default:
		return false
	}
}

func statefulSetPodSkipped(sts *appsv1.StatefulSet, pod *corev1.Pod) bool {
	if !statefulSetUsesRollingUpdate(sts) {
		return false
	}
	if generationStale(sts.Generation, sts.Status.ObservedGeneration) {
		return true
	}
	if sts.Status.UpdateRevision == "" || sts.Status.CurrentRevision == sts.Status.UpdateRevision {
		return false
	}
	return podLabel(pod, appsv1.ControllerRevisionHashLabelKey) != sts.Status.UpdateRevision
}

func daemonSetPodSkipped(ds *appsv1.DaemonSet, pod *corev1.Pod, currentHash string) bool {
	if !daemonSetUsesRollingUpdate(ds) || currentHash == "" {
		return false
	}
	return daemonSetPodRevision(pod) != currentHash
}

func daemonSetPodRevision(pod *corev1.Pod) string {
	if h := podLabel(pod, appsv1.ControllerRevisionHashLabelKey); h != "" {
		return h
	}
	if pod.Annotations == nil {
		return ""
	}
	return pod.Annotations[podTemplateGenerationAnnotation]
}

func pausedDeploymentPodSkipped(dep *appsv1.Deployment, pod *corev1.Pod, currentHash string) bool {
	if dep == nil || !dep.Spec.Paused || currentHash == "" {
		return false
	}
	return podLabel(pod, appsv1.DefaultDeploymentUniqueLabelKey) != currentHash
}

func podLabel(pod *corev1.Pod, key string) string {
	if pod == nil || pod.Labels == nil {
		return ""
	}
	return pod.Labels[key]
}

// filterRolloutPods drops pods that must not be resized yet. A skip emits
// RolloutInProgress once, including when every selected pod is skipped.
// No skip emits nothing. A whole-workload skip is handled by the caller.
func (r *AttunePolicyReconciler) filterRolloutPods(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	workloadName string,
	pods []corev1.Pod,
) []corev1.Pod {
	hash, err := r.rolloutRevisionHash(ctx, workload)
	if err != nil {
		if ds, ok := workload.(*appsv1.DaemonSet); ok && daemonSetUsesRollingUpdate(ds) {
			log.FromContext(ctx).Error(err, "Failed to list DaemonSet controllerrevisions; skipping resize", "workload", workloadName)
			if policy != nil {
				r.emitEventOnce(policy, corev1.EventTypeWarning, "DaemonSetRevisionUnavailable", "resize",
					"Resize skipped for DaemonSet %s: cannot list controllerrevisions", workloadName)
			}
			return nil
		}
		log.FromContext(ctx).Error(err, "Failed to resolve rollout revision; not skipping pods", "workload", workloadName)
		hash = ""
	}
	kept := make([]corev1.Pod, 0, len(pods))
	skipped := 0
	for i := range pods {
		if r.podSkippedForRollout(workload, &pods[i], hash) {
			skipped++
			log.FromContext(ctx).V(1).Info("Skipping pod on previous rollout revision",
				"workload", workloadName, "pod", pods[i].Name)
			continue
		}
		kept = append(kept, pods[i])
	}
	if skipped > 0 {
		r.emitRolloutInProgress(policy, workload)
	}
	return kept
}

func (r *AttunePolicyReconciler) rolloutRevisionHash(ctx context.Context, workload client.Object) (string, error) {
	switch w := workload.(type) {
	case *appsv1.DaemonSet:
		if !daemonSetUsesRollingUpdate(w) {
			return "", nil
		}
		return r.currentDaemonSetRevisionName(ctx, w)
	case *appsv1.Deployment:
		if !w.Spec.Paused {
			return "", nil
		}
		return r.currentPausedDeploymentHash(ctx, w)
	default:
		return "", nil
	}
}

func (r *AttunePolicyReconciler) currentDaemonSetRevisionName(ctx context.Context, ds *appsv1.DaemonSet) (string, error) {
	if r.Client == nil {
		return "", fmt.Errorf("listing ControllerRevisions: client is nil")
	}
	var list appsv1.ControllerRevisionList
	if err := r.List(ctx, &list, client.InNamespace(ds.Namespace)); err != nil {
		return "", err
	}
	var best *appsv1.ControllerRevision
	for i := range list.Items {
		rev := &list.Items[i]
		if !objectOwnedBy(rev, ds.UID) {
			continue
		}
		if best == nil || rev.Revision > best.Revision {
			best = rev
		}
	}
	if best == nil {
		return "", nil
	}
	return best.Name, nil
}

func (r *AttunePolicyReconciler) currentPausedDeploymentHash(ctx context.Context, dep *appsv1.Deployment) (string, error) {
	if r.Client == nil {
		return "", fmt.Errorf("listing ReplicaSets: client is nil")
	}
	var list appsv1.ReplicaSetList
	if err := r.List(ctx, &list, client.InNamespace(dep.Namespace)); err != nil {
		return "", err
	}
	var best *appsv1.ReplicaSet
	var bestRev int64 = -1
	for i := range list.Items {
		rs := &list.Items[i]
		if !objectOwnedBy(rs, dep.UID) {
			continue
		}
		rev := parseDeploymentRevision(rs.Annotations)
		if best == nil || rev > bestRev {
			best = rs
			bestRev = rev
		}
	}
	if best == nil || best.Labels == nil {
		return "", nil
	}
	return best.Labels[appsv1.DefaultDeploymentUniqueLabelKey], nil
}

func objectOwnedBy(obj metav1.Object, uid types.UID) bool {
	if uid == "" {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

func parseDeploymentRevision(annotations map[string]string) int64 {
	if annotations == nil {
		return 0
	}
	v, err := strconv.ParseInt(annotations[deploymentRevisionAnnotation], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// queryMetricsGrouped queries the configured metrics backend once per metric
// for the whole workload, preserving the `container` label so callers can
// split samples by container client-side. The QueryBuilder produces a
// backend-specific query string (PromQL, Datadog query, or CloudWatch spec).
// If qb is nil, it defaults to PromQL for backward compatibility.
// Returns (samples, hardError, seriesCapped).
func queryMetricsGrouped(ctx context.Context, collector rsmetrics.MetricsCollector, qb rsmetrics.QueryBuilder, namespace, podRegex, metric string, start, end time.Time, step, rateWindow time.Duration) (map[string][]rsmetrics.Sample, bool, bool) {
	logger := log.FromContext(ctx)
	v1Logger := logger.V(1)
	v2Logger := logger.V(2)
	if qb == nil {
		qb = &rsmetrics.PromQLQueryBuilder{}
	}
	query := qb.BuildQuery(namespace, podRegex, "", metric, rateWindow)

	if v1Logger.Enabled() {
		v1Logger.Info("Querying metrics backend",
			"metric", metric,
			"start", start.Format(time.RFC3339), "end", end.Format(time.RFC3339),
			"step", step)
	}
	if v2Logger.Enabled() {
		v2Logger.Info("Querying metrics backend",
			"metric", metric, "query", query,
			"start", start.Format(time.RFC3339), "end", end.Format(time.RFC3339),
			"step", step)
	}

	queryType := metric + "_grouped"
	queryStart := time.Now()
	grouped, err := collector.QueryRangeGrouped(ctx, query, start, end, step)
	operatormetrics.PrometheusQueryDuration.WithLabelValues(queryType).Observe(time.Since(queryStart).Seconds())
	if err != nil {
		// Soft error: series cap still returns usable partial data.
		if errors.Is(err, rsmetrics.ErrSeriesCapped) {
			logger.Info("Prometheus series capped; using partial data",
				"metric", metric, "err", err)
			v2Logger.Info("Prometheus series capped; using partial data",
				"metric", metric, "query", query)
			return grouped, false, true
		}
		operatormetrics.PrometheusQueryErrors.WithLabelValues(namespace, queryType).Inc()
		logger.Error(err, "Failed to query grouped metrics", "metric", metric)
		v2Logger.Info("Failed to query grouped metrics", "metric", metric, "query", query)
		return map[string][]rsmetrics.Sample{}, true, false
	}

	if v1Logger.Enabled() {
		// V(1): log when query succeeds but returns no data.
		totalSamples := 0
		for _, samples := range grouped {
			totalSamples += len(samples)
		}
		if totalSamples == 0 {
			v1Logger.Info("Metrics query returned no data",
				"metric", metric)
		}
	}
	if v2Logger.Enabled() {
		v2Logger.Info("Metrics query details",
			"metric", metric, "query", query)
		// V(2): log per-container sample counts.
		for container, samples := range grouped {
			v2Logger.Info("Metrics query samples",
				"metric", metric, "container", container,
				"sampleCount", len(samples))
		}
	}

	return grouped, false, false
}

// isDeploymentOwned returns true if the object has an ownerReference with
// kind=Deployment in the apps/v1 group. Controller is not required.
func isDeploymentOwned(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "Deployment" && (ref.APIVersion == "apps/v1" || ref.APIVersion == "apps/v1beta1" || ref.APIVersion == "apps/v1beta2") {
			return true
		}
	}
	return false
}

// isRolloutOwned returns true when the controller owner is an
// argoproj.io/v1alpha1 Rollout. A non-controller reference is kept.
func isRolloutOwned(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind == argorollout.Kind && ref.APIVersion == argorollout.Group+"/"+argorollout.Version {
			return true
		}
	}
	return false
}

// filterStandaloneReplicaSets removes ReplicaSets owned by a Deployment
// or a Rollout. A standalone ReplicaSet is kept.
func filterStandaloneReplicaSets(objects []client.Object) []client.Object {
	result := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		if isDeploymentOwned(obj) || isRolloutOwned(obj) {
			continue
		}
		result = append(result, obj)
	}
	return result
}

// buildPrometheusQuery is a convenience wrapper around PromQLQueryBuilder for
// backward compatibility. Used by benchmarks and tests that don't need
// multi-backend support.
func buildPrometheusQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	return (&rsmetrics.PromQLQueryBuilder{}).BuildQuery(namespace, podRegex, container, metric, rateWindow)
}
