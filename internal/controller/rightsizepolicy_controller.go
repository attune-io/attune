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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardif/kube-rightsize/internal/metrics"
	"github.com/SebTardif/kube-rightsize/internal/recommendation"
)

const (
	// optOutAnnotation is the annotation key used to skip a workload.
	optOutAnnotation = "rightsize.io/skip"

	// defaultHistoryWindow is the default history window if not specified.
	defaultHistoryWindow = 7 * 24 * time.Hour

	// defaultCooldown is the default cooldown between reconciliation cycles.
	defaultCooldown = 1 * time.Hour

	// defaultMinimumDataPoints is the minimum number of data points required.
	defaultMinimumDataPoints int32 = 168

	// defaultPrometheusStep is the step interval for Prometheus range queries.
	defaultPrometheusStep = 5 * time.Minute

	// conditionTypeReady is the condition type for overall health.
	conditionTypeReady = "Ready"
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch

// RightSizePolicyReconciler reconciles a RightSizePolicy object.
type RightSizePolicyReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	MetricsFactory MetricsCollectorFactory
}

// MetricsCollectorFactory creates MetricsCollector instances from a Prometheus address.
// This enables dependency injection for testing.
type MetricsCollectorFactory func(address string) (rsmetrics.MetricsCollector, error)

// Reconcile is the main reconciliation loop for RightSizePolicy resources.
func (r *RightSizePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step 1: Fetch the RightSizePolicy CR.
	var policy rightsizev1alpha1.RightSizePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("RightSizePolicy resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching RightSizePolicy: %w", err)
	}

	// Step 2: Resolve Prometheus address from spec or RightSizeDefaults.
	prometheusAddr, err := r.resolvePrometheusAddress(ctx, &policy)
	if err != nil {
		logger.Error(err, "Failed to resolve Prometheus address")
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PrometheusUnavailable",
			Message:            fmt.Sprintf("Cannot resolve Prometheus address: %v", err),
			ObservedGeneration: policy.Generation,
		})
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	collector, err := r.MetricsFactory(prometheusAddr)
	if err != nil {
		logger.Error(err, "Failed to create metrics collector", "address", prometheusAddr)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PrometheusUnavailable",
			Message:            fmt.Sprintf("Cannot create metrics collector: %v", err),
			ObservedGeneration: policy.Generation,
		})
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Step 3: Discover target workloads.
	workloads, err := r.discoverWorkloads(ctx, &policy)
	if err != nil {
		logger.Error(err, "Failed to discover workloads")
		return ctrl.Result{}, fmt.Errorf("discovering workloads: %w", err)
	}

	logger.Info("Discovered workloads", "count", len(workloads))

	if len(workloads) == 0 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InsufficientData",
			Message:            "No matching workloads found",
			ObservedGeneration: policy.Generation,
		})
		policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{}
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: r.parseCooldown(&policy)}, nil
	}

	// Step 4-8: Process each workload.
	var recommendations []rightsizev1alpha1.WorkloadRecommendation
	var workloadsWithRecs int32
	conflictDetector := conflict.NewDetector(logger)

	for _, workload := range workloads {
		workloadName := workload.GetName()
		workloadKind := workload.GetObjectKind().GroupVersionKind().Kind

		// Step 5: Check for opt-out annotation.
		workloadMeta := metav1.ObjectMeta{Annotations: workload.GetAnnotations()}
		if conflictDetector.CheckAnnotationOptOut(workloadMeta) {
			logger.Info("Workload opted out via annotation", "workload", workloadName)
			continue
		}

		// Step 6: Check for active rollout.
		if r.isRollingOut(workload) {
			logger.Info("Skipping workload mid-rollout", "workload", workloadName)
			continue
		}

		// Step 4: Get pods for the workload.
		pods, err := r.getPodsForWorkload(ctx, workload)
		if err != nil {
			logger.Error(err, "Failed to get pods for workload", "workload", workloadName)
			continue
		}

		if len(pods) == 0 {
			logger.Info("No pods found for workload", "workload", workloadName)
			continue
		}

		// Step 7: Compute recommendations for each container.
		rec, err := r.computeRecommendations(ctx, &policy, workload, pods, collector)
		if err != nil {
			logger.Error(err, "Failed to compute recommendations", "workload", workloadName)
			continue
		}

		if rec != nil {
			rec.Workload = workloadName
			rec.Kind = workloadKind
			recommendations = append(recommendations, *rec)
			workloadsWithRecs++
		}
	}

	// Step 8: Update status.
	policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{
		Discovered:          int32(len(workloads)),
		WithRecommendations: workloadsWithRecs,
	}
	policy.Status.Recommendations = recommendations

	// Compute savings estimate.
	policy.Status.Savings = r.computeSavings(recommendations)

	// Set Ready condition.
	if workloadsWithRecs > 0 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Monitoring",
			Message:            fmt.Sprintf("Watching %d workloads, %d with recommendations", len(workloads), workloadsWithRecs),
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InsufficientData",
			Message:            "No workloads have sufficient data for recommendations",
			ObservedGeneration: policy.Generation,
		})
	}

	if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", statusErr)
	}

	// Step 9: Handle resize modes (placeholder).
	mode := policy.Spec.UpdateStrategy.Mode
	if mode == "OneShot" || mode == "Canary" || mode == "Auto" {
		logger.Info("Resize would happen here", "mode", mode, "workloads", workloadsWithRecs)
	}

	// Step 10: Requeue after cooldown.
	cooldown := r.parseCooldown(&policy)
	logger.Info("Reconciliation complete, requeueing", "cooldown", cooldown)
	return ctrl.Result{RequeueAfter: cooldown}, nil
}

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

// buildPrometheusQuery generates a PromQL query for the given metric type.
func buildPrometheusQuery(namespace, podPrefix, container, metric string) string {
	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s.*",container="%s"}[5m])`,
			namespace, podPrefix, container,
		)
	case "memory":
		return fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s",pod=~"%s.*",container="%s"}`,
			namespace, podPrefix, container,
		)
	default:
		return ""
	}
}

// computeRecommendations generates resource recommendations for all containers
// in a workload based on Prometheus metrics.
func (r *RightSizePolicyReconciler) computeRecommendations(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workload client.Object,
	_ []corev1.Pod,
	collector rsmetrics.MetricsCollector,
) (*rightsizev1alpha1.WorkloadRecommendation, error) {
	logger := log.FromContext(ctx)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, nil
	}

	historyWindow := r.parseHistoryWindow(policy)
	minimumDataPoints := r.getMinimumDataPoints(policy)

	now := time.Now()
	start := now.Add(-historyWindow)
	podPrefix := r.getPodPrefix(workload)

	cpuPercentile := int(policy.Spec.CPU.Percentile)
	if cpuPercentile == 0 {
		cpuPercentile = 95
	}
	memPercentile := int(policy.Spec.Memory.Percentile)
	if memPercentile == 0 {
		memPercentile = 99
	}

	cpuSafetyMargin := parseFloat64(policy.Spec.CPU.SafetyMargin, 1.2)
	memSafetyMargin := parseFloat64(policy.Spec.Memory.SafetyMargin, 1.3)

	cpuBoundsMin := resource.MustParse("50m")
	cpuBoundsMax := resource.MustParse("4000m")
	if policy.Spec.CPU.Bounds != nil {
		cpuBoundsMin = policy.Spec.CPU.Bounds.Min.DeepCopy()
		cpuBoundsMax = policy.Spec.CPU.Bounds.Max.DeepCopy()
	}

	memBoundsMin := resource.MustParse("64Mi")
	memBoundsMax := resource.MustParse("8Gi")
	if policy.Spec.Memory.Bounds != nil {
		memBoundsMin = policy.Spec.Memory.Bounds.Min.DeepCopy()
		memBoundsMax = policy.Spec.Memory.Bounds.Max.DeepCopy()
	}

	cpuEngine := recommendation.NewEngine(cpuPercentile, cpuSafetyMargin, cpuBoundsMin, cpuBoundsMax, float64(policy.Spec.UpdateStrategy.MaxCPUChangePercent))
	memEngine := recommendation.NewEngine(memPercentile, memSafetyMargin, memBoundsMin, memBoundsMax, float64(policy.Spec.UpdateStrategy.MaxMemoryChangePercent))

	var containerRecs []rightsizev1alpha1.ContainerRecommendation

	for _, container := range containers {
		containerName := container.Name

		// Build Prometheus queries.
		cpuQuery := buildPrometheusQuery(policy.Namespace, podPrefix, containerName, "cpu")
		memQuery := buildPrometheusQuery(policy.Namespace, podPrefix, containerName, "memory")

		// Query Prometheus with QueryRange.
		cpuSamples, err := collector.QueryRange(ctx, cpuQuery, start, now, defaultPrometheusStep)
		if err != nil {
			logger.Error(err, "Failed to query CPU metrics", "container", containerName)
			cpuSamples = nil
		}

		memSamples, err := collector.QueryRange(ctx, memQuery, start, now, defaultPrometheusStep)
		if err != nil {
			logger.Error(err, "Failed to query memory metrics", "container", containerName)
			memSamples = nil
		}

		// Build UsageProfile from samples.
		cpuProfile := rsmetrics.BuildProfile(cpuSamples)
		memProfile := rsmetrics.BuildProfile(memSamples)

		// Check for sufficient data points.
		if int32(cpuProfile.DataPoints) < minimumDataPoints && int32(memProfile.DataPoints) < minimumDataPoints {
			logger.Info("Insufficient data points",
				"container", containerName,
				"cpuPoints", cpuProfile.DataPoints,
				"memPoints", memProfile.DataPoints,
				"minimum", minimumDataPoints)
			continue
		}

		// Get current resource values.
		currentCPUReq := container.Resources.Requests.Cpu().DeepCopy()
		currentCPULim := container.Resources.Limits.Cpu().DeepCopy()
		currentMemReq := container.Resources.Requests.Memory().DeepCopy()
		currentMemLim := container.Resources.Limits.Memory().DeepCopy()

		rec := rightsizev1alpha1.ContainerRecommendation{
			Name:       containerName,
			DataPoints: int32(cpuProfile.DataPoints + memProfile.DataPoints),
			Confidence: (cpuProfile.Confidence + memProfile.Confidence) / 2.0,
			LastUpdated: metav1.Time{
				Time: now,
			},
			Current: rightsizev1alpha1.ResourceValues{
				CPURequest:    currentCPUReq,
				CPULimit:      currentCPULim,
				MemoryRequest: currentMemReq,
				MemoryLimit:   currentMemLim,
			},
			Recommended: rightsizev1alpha1.ResourceValues{
				CPURequest:    currentCPUReq,
				CPULimit:      currentCPULim,
				MemoryRequest: currentMemReq,
				MemoryLimit:   currentMemLim,
			},
		}

		// Compute CPU recommendation.
		if int32(cpuProfile.DataPoints) >= minimumDataPoints {
			cpuRec, _ := cpuEngine.Recommend(cpuProfile, currentCPUReq)
			rec.Recommended.CPURequest = cpuRec
		}

		// Compute memory recommendation.
		if int32(memProfile.DataPoints) >= minimumDataPoints {
			memRec, _ := memEngine.Recommend(memProfile, currentMemReq)
			rec.Recommended.MemoryRequest = memRec
		}

		containerRecs = append(containerRecs, rec)
	}

	if len(containerRecs) == 0 {
		return nil, nil
	}

	return &rightsizev1alpha1.WorkloadRecommendation{
		Containers: containerRecs,
	}, nil
}

// resolvePrometheusAddress returns the Prometheus address from the policy spec,
// falling back to the cluster-scoped RightSizeDefaults if not set.
func (r *RightSizePolicyReconciler) resolvePrometheusAddress(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (string, error) {
	// Check policy-level config first.
	if policy.Spec.MetricsSource.Prometheus != nil &&
		policy.Spec.MetricsSource.Prometheus.Address != "" {
		return policy.Spec.MetricsSource.Prometheus.Address, nil
	}

	// Fall back to RightSizeDefaults.
	var defaultsList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &defaultsList); err != nil {
		return "", fmt.Errorf("listing RightSizeDefaults: %w", err)
	}

	for _, defaults := range defaultsList.Items {
		if defaults.Spec.MetricsSource != nil &&
			defaults.Spec.MetricsSource.Prometheus != nil &&
			defaults.Spec.MetricsSource.Prometheus.Address != "" {
			return defaults.Spec.MetricsSource.Prometheus.Address, nil
		}
	}

	return "", fmt.Errorf("no Prometheus address configured in policy or cluster defaults")
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

// parseHistoryWindow parses the history window duration from the policy.
func (r *RightSizePolicyReconciler) parseHistoryWindow(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.MetricsSource.HistoryWindow != nil {
		return policy.Spec.MetricsSource.HistoryWindow.Duration
	}
	return defaultHistoryWindow
}

// getMinimumDataPoints returns the minimum data points threshold from the policy.
func (r *RightSizePolicyReconciler) getMinimumDataPoints(policy *rightsizev1alpha1.RightSizePolicy) int32 {
	if policy.Spec.MetricsSource.MinimumDataPoints > 0 {
		return policy.Spec.MetricsSource.MinimumDataPoints
	}
	return defaultMinimumDataPoints
}

// parseCooldown returns the cooldown duration from the policy's update strategy.
func (r *RightSizePolicyReconciler) parseCooldown(policy *rightsizev1alpha1.RightSizePolicy) time.Duration {
	if policy.Spec.UpdateStrategy.Cooldown != nil {
		return policy.Spec.UpdateStrategy.Cooldown.Duration
	}
	return defaultCooldown
}

// computeSavings calculates the aggregate resource savings across all recommendations.
func (r *RightSizePolicyReconciler) computeSavings(recommendations []rightsizev1alpha1.WorkloadRecommendation) rightsizev1alpha1.SavingsStatus {
	var totalCPUSaved, totalMemSaved int64

	for _, rec := range recommendations {
		for _, c := range rec.Containers {
			cpuDiff := c.Current.CPURequest.MilliValue() - c.Recommended.CPURequest.MilliValue()
			if cpuDiff > 0 {
				totalCPUSaved += cpuDiff
			}

			memDiff := c.Current.MemoryRequest.Value() - c.Recommended.MemoryRequest.Value()
			if memDiff > 0 {
				totalMemSaved += memDiff
			}
		}
	}

	savings := rightsizev1alpha1.SavingsStatus{}
	if totalCPUSaved > 0 {
		savings.CPURequestReduction = resource.NewMilliQuantity(totalCPUSaved, resource.DecimalSI).String()
	}
	if totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(totalMemSaved, resource.BinarySI).String()
	}
	return savings
}

// parseFloat64 parses a string as a float64, returning the fallback on error.
func parseFloat64(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}).
		Complete(r)
}

