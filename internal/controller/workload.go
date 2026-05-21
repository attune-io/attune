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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

// discoverWorkloads finds workloads matching the policy's targetRef.
func (r *RightSizePolicyReconciler) discoverWorkloads(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) ([]client.Object, error) {
	targetRef := policy.Spec.TargetRef
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
func (r *RightSizePolicyReconciler) getWorkloadByName(ctx context.Context, namespace, kind, name string) (client.Object, error) {
	wk, ok := workloadKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported workload kind: %s; supported kinds are: %s", kind, rightsizev1alpha1.SupportedTargetKindsCSV)
	}
	obj := wk.newObject()
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// listWorkloadsBySelector lists workloads matching a label selector.
func (r *RightSizePolicyReconciler) listWorkloadsBySelector(ctx context.Context, namespace, kind string, selector *metav1.LabelSelector) ([]client.Object, error) {
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
		return nil, fmt.Errorf("unsupported workload kind: %s; supported kinds are: %s", kind, rightsizev1alpha1.SupportedTargetKindsCSV)
	}

	list := wk.newList()
	if err := r.List(ctx, list, listOpts...); err != nil {
		return nil, err
	}
	return wk.extract(list), nil
}

// getPodsForWorkload returns the pods managed by a workload by matching
// the workload's pod template selector labels.
func (r *RightSizePolicyReconciler) getPodsForWorkload(ctx context.Context, workload client.Object) ([]corev1.Pod, error) {
	selectorLabels := r.getPodSelectorLabels(workload)
	if len(selectorLabels) == 0 {
		return nil, fmt.Errorf("workload %s/%s has no pod selector labels", workload.GetNamespace(), workload.GetName())
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(workload.GetNamespace()),
		client.MatchingLabels(selectorLabels),
	); err != nil {
		return nil, fmt.Errorf("listing pods for workload %s: %w", workload.GetName(), err)
	}

	return podList.Items, nil
}

// getPodSelectorLabels extracts the pod selector labels from a workload.
func (r *RightSizePolicyReconciler) getPodSelectorLabels(workload client.Object) map[string]string {
	if a := newWorkloadAdapter(workload); a != nil {
		return a.PodSelectorLabels()
	}
	return nil
}

// getContainers returns the container specs from a workload's pod template,
// including native sidecar containers (init containers with restartPolicy=Always).
func (r *RightSizePolicyReconciler) getContainers(workload client.Object) []corev1.Container {
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

// isRollingOut checks if a workload is currently in the middle of a rollout.
func (r *RightSizePolicyReconciler) isRollingOut(workload client.Object) bool {
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
//   - StatefulSet: <name>-<ordinal>
//   - DaemonSet: <name>-<pod-hash>
//   - Job: <name>-<pod-hash>
//   - CronJob: <name>-<timestamp>-<pod-hash>
func (r *RightSizePolicyReconciler) getPodRegex(workload client.Object) string {
	name := rsmetrics.EscapePromQLRegex(workload.GetName())
	if a := newWorkloadAdapter(workload); a != nil {
		return name + a.PodNameRegexSuffix()
	}
	// Unknown kinds: fall back to prefix match.
	return name + ".*"
}

// queryMetricsGrouped queries Prometheus once per metric for the whole
// workload, preserving the `container` label so callers can split samples
// by container client-side. If the grouped query returns no labeled series,
// it falls back to the pod-level query and returns samples under the empty
// string key.
func queryMetricsGrouped(ctx context.Context, collector rsmetrics.MetricsCollector, namespace, podRegex, metric string, start, end time.Time, step, rateWindow time.Duration) (map[string][]rsmetrics.Sample, bool) {
	logger := log.FromContext(ctx)
	v1Logger := logger.V(1)
	v2Logger := logger.V(2)
	query := buildPrometheusQuery(namespace, podRegex, "", metric, rateWindow)

	if v1Logger.Enabled() {
		v1Logger.Info("Querying Prometheus",
			"metric", metric, "query", query,
			"start", start.Format(time.RFC3339), "end", end.Format(time.RFC3339),
			"step", step)
	}

	queryType := metric + "_grouped"
	queryStart := time.Now()
	grouped, err := collector.QueryRangeGrouped(ctx, query, start, end, step)
	operatormetrics.PrometheusQueryDuration.WithLabelValues(queryType).Observe(time.Since(queryStart).Seconds())
	if err != nil {
		operatormetrics.PrometheusQueryErrors.WithLabelValues(namespace, queryType).Inc()
		logger.Error(err, "Failed to query grouped metrics", "metric", metric, "query", query)
		return map[string][]rsmetrics.Sample{}, true
	}

	if v1Logger.Enabled() {
		// V(1): log when query succeeds but returns no data.
		totalSamples := 0
		for _, samples := range grouped {
			totalSamples += len(samples)
		}
		if totalSamples == 0 {
			v1Logger.Info("Prometheus query returned no data",
				"metric", metric, "query", query)
		}
	}
	if v2Logger.Enabled() {
		// V(2): log per-container sample counts.
		for container, samples := range grouped {
			v2Logger.Info("Prometheus query samples",
				"metric", metric, "container", container,
				"sampleCount", len(samples))
		}
	}

	return grouped, false
}

// buildPrometheusQuery generates a PromQL query for the given metric type.
// podRegex is a pre-built PromQL regex (already escaped) that matches pod names.
// If container is empty, the query matches pod-level metrics (no container filter).
func buildPrometheusQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	ns := rsmetrics.EscapePromQL(namespace)

	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(`,container="%s"`, rsmetrics.EscapePromQL(container))
	}

	// Format rate window for PromQL (e.g., "5m", "15m", "2m30s").
	rw := formatPromDuration(rateWindow)

	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s"%s}[%s])`,
			ns, podRegex, containerFilter, rw,
		)
	case "memory":
		return fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s",pod=~"%s"%s}`,
			ns, podRegex, containerFilter,
		)
	default:
		return ""
	}
}

// formatPromDuration formats a Go duration as a PromQL duration string.
// PromQL accepts "Nm" for minutes, "Ns" for seconds, "Nh" for hours.
func formatPromDuration(d time.Duration) string {
	if d <= 0 {
		return "5m"
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
