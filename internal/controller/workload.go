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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardif/kube-rightsize/internal/metrics"
	"github.com/SebTardif/kube-rightsize/internal/operatormetrics"
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
	key := types.NamespacedName{Namespace: namespace, Name: name}

	switch kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj, nil
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj, nil
	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj, nil
	default:
		return nil, fmt.Errorf("unsupported workload kind: %s", kind)
	}
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

	var result []client.Object

	switch kind {
	case "Deployment":
		var list appsv1.DeploymentList
		if err := r.List(ctx, &list, listOpts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	case "StatefulSet":
		var list appsv1.StatefulSetList
		if err := r.List(ctx, &list, listOpts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	case "DaemonSet":
		var list appsv1.DaemonSetList
		if err := r.List(ctx, &list, listOpts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
	default:
		return nil, fmt.Errorf("unsupported workload kind: %s", kind)
	}

	return result, nil
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
	switch w := workload.(type) {
	case *appsv1.Deployment:
		if w.Spec.Selector != nil {
			return w.Spec.Selector.MatchLabels
		}
	case *appsv1.StatefulSet:
		if w.Spec.Selector != nil {
			return w.Spec.Selector.MatchLabels
		}
	case *appsv1.DaemonSet:
		if w.Spec.Selector != nil {
			return w.Spec.Selector.MatchLabels
		}
	}
	return nil
}

// getContainers returns the container specs from a workload's pod template.
func (r *RightSizePolicyReconciler) getContainers(workload client.Object) []corev1.Container {
	switch w := workload.(type) {
	case *appsv1.Deployment:
		return w.Spec.Template.Spec.Containers
	case *appsv1.StatefulSet:
		return w.Spec.Template.Spec.Containers
	case *appsv1.DaemonSet:
		return w.Spec.Template.Spec.Containers
	}
	return nil
}

// isRollingOut checks if a workload is currently in the middle of a rollout.
func (r *RightSizePolicyReconciler) isRollingOut(workload client.Object) bool {
	switch w := workload.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil && w.Status.UpdatedReplicas < *w.Spec.Replicas {
			return true
		}
		if w.Spec.Replicas != nil && w.Status.AvailableReplicas < *w.Spec.Replicas {
			return true
		}
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil && w.Status.UpdatedReplicas < *w.Spec.Replicas {
			return true
		}
	case *appsv1.DaemonSet:
		if w.Status.UpdatedNumberScheduled < w.Status.DesiredNumberScheduled {
			return true
		}
	}
	return false
}

// getPodPrefix derives the pod name prefix from a workload.
func (r *RightSizePolicyReconciler) getPodPrefix(workload client.Object) string {
	return workload.GetName()
}

// queryMetrics queries Prometheus for the given metric type, falling back
// to pod-level metrics if the container-specific query returns no data.
func queryMetrics(ctx context.Context, collector rsmetrics.MetricsCollector, namespace, podPrefix, container, metric string, start, end time.Time, step time.Duration) []rsmetrics.Sample {
	query := buildPrometheusQuery(namespace, podPrefix, container, metric)

	queryStart := time.Now()
	samples, err := collector.QueryRange(ctx, query, start, end, step)
	operatormetrics.PrometheusQueryDuration.WithLabelValues().Observe(time.Since(queryStart).Seconds())

	if err != nil {
		operatormetrics.PrometheusQueryErrors.Inc()
		log.FromContext(ctx).Error(err, "Failed to query metrics", "metric", metric, "container", container)
		samples = nil
	}
	if len(samples) == 0 && container != "" {
		fallback := buildPrometheusQuery(namespace, podPrefix, "", metric)
		queryStart = time.Now()
		samples, err = collector.QueryRange(ctx, fallback, start, end, step)
		operatormetrics.PrometheusQueryDuration.WithLabelValues().Observe(time.Since(queryStart).Seconds())
		if err != nil {
			operatormetrics.PrometheusQueryErrors.Inc()
			log.FromContext(ctx).Error(err, "Failed to query fallback metrics", "metric", metric)
		}
	}
	return samples
}

// buildPrometheusQuery generates a PromQL query for the given metric type.
// If container is empty, the query matches pod-level metrics (no container filter).
func buildPrometheusQuery(namespace, podPrefix, container, metric string) string {
	ns := rsmetrics.EscapePromQL(namespace)
	pp := rsmetrics.EscapePromQLRegex(podPrefix)

	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(`,container="%s"`, rsmetrics.EscapePromQL(container))
	}

	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s.*"%s}[5m])`,
			ns, pp, containerFilter,
		)
	case "memory":
		return fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s",pod=~"%s.*"%s}`,
			ns, pp, containerFilter,
		)
	default:
		return ""
	}
}
