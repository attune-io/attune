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
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardif/kube-rightsize/internal/metrics"
	"github.com/SebTardif/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardif/kube-rightsize/internal/recommendation"
	"github.com/SebTardif/kube-rightsize/internal/resize"
	"github.com/SebTardif/kube-rightsize/internal/safety"
)

const (
	// lastResizeAnnotation is the annotation key for tracking last resize time.
	lastResizeAnnotation = "rightsize.io/last-resize-time"

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

	// defaultObservationPeriod is the default safety observation window after resize.
	defaultObservationPeriod = 5 * time.Minute
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch;update;patch
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
	Clientset      kubernetes.Interface // for resize subresource calls
}

// MetricsCollectorFactory creates MetricsCollector instances from a Prometheus address.
// This enables dependency injection for testing.
type MetricsCollectorFactory func(address string) (rsmetrics.MetricsCollector, error)

// Reconcile is the main reconciliation loop for RightSizePolicy resources.
func (r *RightSizePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
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

	// Merge cluster-scoped defaults into the policy.
	r.mergeDefaults(ctx, &policy)

	// Check pending safety observations from previous resizes before computing
	// new recommendations.
	if policy.Spec.UpdateStrategy.AutoRevert {
		r.checkPendingSafetyObservations(ctx, &policy)
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

	// Step 8: Update status fields.
	policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{
		Discovered:          safeInt32(len(workloads)),
		WithRecommendations: workloadsWithRecs,
	}
	policy.Status.Recommendations = recommendations

	// Compute savings estimate.
	policy.Status.Savings = r.computeSavings(policy.Namespace, recommendations)

	// Step 9: Execute resizes if mode allows.
	mode := policy.Spec.UpdateStrategy.Mode
	if (mode == "OneShot" || mode == "Canary" || mode == "Auto") && !r.isCooldownActive(&policy) {
		resizedCount, history := r.executeResizes(ctx, &policy, workloads, recommendations)
		if resizedCount > 0 {
			policy.Status.Workloads.Resized = safeInt32(resizedCount)
			policy.Status.ResizeHistory = appendHistory(policy.Status.ResizeHistory, history, 20)
		}
	} else if mode == "OneShot" || mode == "Canary" || mode == "Auto" {
		logger.Info("Cooldown active, skipping resize")
	}

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

	// Use a retry loop for the status update to handle resource version conflicts
	// caused by concurrent metadata updates (e.g., cooldown annotations).
	if statusErr := r.updateStatusWithRetry(ctx, &policy, req.NamespacedName); statusErr != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", statusErr)
	}

	// Mark resize time AFTER status is written (avoids resourceVersion conflict
	// between metadata and status subresource updates).
	if policy.Status.Workloads.Resized > 0 {
		if err := r.markResizeTime(ctx, &policy); err != nil {
			logger.Error(err, "Failed to mark resize time")
		}
	}

	// Step 10: Requeue after cooldown.
	cooldown := r.parseCooldown(&policy)
	logger.Info("Reconciliation complete, requeueing", "cooldown", cooldown)
	operatormetrics.ReconcileDuration.WithLabelValues("rightsizepolicy").Observe(time.Since(startTime).Seconds())
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
// If container is empty, the query matches pod-level metrics (no container filter).
func buildPrometheusQuery(namespace, podPrefix, container, metric string) string {
	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(`,container="%s"`, container)
	}

	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s.*"%s}[5m])`,
			namespace, podPrefix, containerFilter,
		)
	case "memory":
		return fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s",pod=~"%s.*"%s}`,
			namespace, podPrefix, containerFilter,
		)
	default:
		return ""
	}
}

// computeRecommendations generates resource recommendations for all containers
// in a workload based on Prometheus metrics.
//
//nolint:unparam // error return is part of the interface contract for future use
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

		// Build Prometheus queries with container label.
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

		// Fallback: if no results with container label, try pod-level metrics (no container filter).
		// Some Prometheus/cAdvisor configurations don't include the container label.
		if len(cpuSamples) == 0 {
			fallbackCPU := buildPrometheusQuery(policy.Namespace, podPrefix, "", "cpu")
			cpuSamples, _ = collector.QueryRange(ctx, fallbackCPU, start, now, defaultPrometheusStep)
		}
		if len(memSamples) == 0 {
			fallbackMem := buildPrometheusQuery(policy.Namespace, podPrefix, "", "memory")
			memSamples, _ = collector.QueryRange(ctx, fallbackMem, start, now, defaultPrometheusStep)
		}

		// Build UsageProfile from samples.
		cpuProfile := rsmetrics.BuildProfile(cpuSamples)
		memProfile := rsmetrics.BuildProfile(memSamples)

		// Check for sufficient data points.
		if cpuProfile.DataPoints < int(minimumDataPoints) && memProfile.DataPoints < int(minimumDataPoints) {
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
			DataPoints: safeInt32(cpuProfile.DataPoints + memProfile.DataPoints),
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
		if cpuProfile.DataPoints >= int(minimumDataPoints) {
			cpuRec, _ := cpuEngine.Recommend(cpuProfile, currentCPUReq)
			rec.Recommended.CPURequest = cpuRec
		}

		// Compute memory recommendation.
		if memProfile.DataPoints >= int(minimumDataPoints) {
			memRec, _ := memEngine.Recommend(memProfile, currentMemReq)
			rec.Recommended.MemoryRequest = memRec
		}

		// Scale limits proportionally if ControlledValues is RequestsAndLimits.
		cpuControlled := "RequestsOnly"
		if policy.Spec.CPU.ControlledValues != nil {
			cpuControlled = *policy.Spec.CPU.ControlledValues
		}
		memControlled := "RequestsOnly"
		if policy.Spec.Memory.ControlledValues != nil {
			memControlled = *policy.Spec.Memory.ControlledValues
		}
		if cpuControlled == "RequestsAndLimits" {
			rec.Recommended.CPULimit = scaleLimits(currentCPUReq, currentCPULim, rec.Recommended.CPURequest)
		}
		if memControlled == "RequestsAndLimits" {
			rec.Recommended.MemoryLimit = scaleLimits(currentMemReq, currentMemLim, rec.Recommended.MemoryRequest)
		}

		// Set recommendation gauges for this container.
		operatormetrics.RecommendationCPU.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(float64(rec.Recommended.CPURequest.MilliValue()) / 1000.0)
		operatormetrics.RecommendationMemory.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(float64(rec.Recommended.MemoryRequest.Value()))
		operatormetrics.Confidence.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(rec.Confidence)

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
func (r *RightSizePolicyReconciler) computeSavings(namespace string, recommendations []rightsizev1alpha1.WorkloadRecommendation) rightsizev1alpha1.SavingsStatus {
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
		operatormetrics.SavingsCPU.WithLabelValues(namespace).Set(float64(totalCPUSaved) / 1000.0)
	}
	if totalMemSaved > 0 {
		savings.MemoryRequestReduction = resource.NewQuantity(totalMemSaved, resource.BinarySI).String()
		operatormetrics.SavingsMemory.WithLabelValues(namespace).Set(float64(totalMemSaved))
	}
	return savings
}

// scaleLimits scales a resource limit proportionally to maintain the same
// request:limit ratio when the request changes.
func scaleLimits(currentReq, currentLim, newReq resource.Quantity) resource.Quantity {
	if currentReq.IsZero() || currentLim.IsZero() {
		return newReq.DeepCopy()
	}
	ratio := float64(currentLim.MilliValue()) / float64(currentReq.MilliValue())
	newLimMilli := int64(float64(newReq.MilliValue()) * ratio)
	return *resource.NewMilliQuantity(newLimMilli, currentLim.Format)
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

// safeInt32 converts an int to int32, clamping to math.MaxInt32 on overflow.
func safeInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v) // #nosec G115 -- overflow guarded by check above
}

// isCooldownActive checks if the policy is within the cooldown window since last resize.
func (r *RightSizePolicyReconciler) isCooldownActive(policy *rightsizev1alpha1.RightSizePolicy) bool {
	ann := policy.Annotations
	if ann == nil {
		return false
	}
	lastStr, ok := ann[lastResizeAnnotation]
	if !ok {
		return false
	}
	last, err := time.Parse(time.RFC3339, lastStr)
	if err != nil {
		return false
	}
	cooldown := r.parseCooldown(policy)
	return time.Since(last) < cooldown
}

// markResizeTime sets the last-resize-time annotation on the policy.
func (r *RightSizePolicyReconciler) markResizeTime(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) error {
	if policy.Annotations == nil {
		policy.Annotations = make(map[string]string)
	}
	policy.Annotations[lastResizeAnnotation] = time.Now().UTC().Format(time.RFC3339)
	return r.Update(ctx, policy)
}

// selectPodsForResize selects pods eligible for resize based on the update mode.
func selectPodsForResize(pods []corev1.Pod, mode string, canaryPercentage int32) []corev1.Pod {
	var eligible []corev1.Pod
	for _, p := range pods {
		if resize.CanResizeInPlace(&p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	switch mode {
	case "OneShot":
		return eligible[:1]
	case "Canary":
		count := int(canaryPercentage) * len(eligible) / 100
		if count < 1 {
			count = 1
		}
		if count > len(eligible) {
			count = len(eligible)
		}
		return eligible[:count]
	case "Auto":
		return eligible // resize all in Auto mode
	default:
		return nil
	}
}

// executeResizes performs the actual pod resizes for all workloads with recommendations.
func (r *RightSizePolicyReconciler) executeResizes(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workloads []client.Object,
	recommendations []rightsizev1alpha1.WorkloadRecommendation,
) (int, []rightsizev1alpha1.ResizeHistoryEntry) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		logger.Info("No clientset configured, skipping resize execution")
		return 0, nil
	}

	mode := policy.Spec.UpdateStrategy.Mode
	canaryPct := int32(10)
	if policy.Spec.UpdateStrategy.Canary != nil {
		canaryPct = policy.Spec.UpdateStrategy.Canary.Percentage
	}

	resizer := resize.NewPodResizer(r.Clientset, logger)
	monitor := safety.NewMonitor(r.Clientset, logger)

	var totalResized int
	var history []rightsizev1alpha1.ResizeHistoryEntry
	now := metav1.Now()

	for _, rec := range recommendations {
		// Find the matching workload
		var matchedWorkload client.Object
		for _, w := range workloads {
			if w.GetName() == rec.Workload {
				matchedWorkload = w
				break
			}
		}
		if matchedWorkload == nil {
			continue
		}

		// Get pods for this workload
		pods, err := r.getPodsForWorkload(ctx, matchedWorkload)
		if err != nil {
			logger.Error(err, "Failed to get pods for resize", "workload", rec.Workload)
			continue
		}

		selectedPods := selectPodsForResize(pods, mode, canaryPct)
		if len(selectedPods) == 0 {
			continue
		}

		for _, pod := range selectedPods {
			for _, containerRec := range rec.Containers {
				target := corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    containerRec.Recommended.CPURequest.DeepCopy(),
						corev1.ResourceMemory: containerRec.Recommended.MemoryRequest.DeepCopy(),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    containerRec.Recommended.CPULimit.DeepCopy(),
						corev1.ResourceMemory: containerRec.Recommended.MemoryLimit.DeepCopy(),
					},
				}

				// Pre-check: skip if the pod's ACTUAL resources already match the
				// recommendation. Compare against the running pod, not the
				// Deployment template (which isn't updated by in-place resize).
				var podActualCPU, podActualMem int64
				for _, c := range pod.Spec.Containers {
					if c.Name == containerRec.Name {
						podActualCPU = c.Resources.Requests.Cpu().MilliValue()
						podActualMem = c.Resources.Requests.Memory().Value()
						break
					}
				}
				if podActualCPU == containerRec.Recommended.CPURequest.MilliValue() &&
					podActualMem == containerRec.Recommended.MemoryRequest.Value() {
					continue
				}

				// Pre-check: QoS preservation
				if !resize.PreservesQoS(&pod, containerRec.Name, target) {
					logger.Info("Skipping resize: would change QoS class",
						"pod", pod.Name, "container", containerRec.Name)
					continue
				}

				// Execute resize
				results, err := resizer.ResizePod(ctx, &pod, containerRec.Name, target)
				if err != nil {
					logger.Error(err, "Failed to resize pod",
						"pod", pod.Name, "container", containerRec.Name)
					for _, res := range results {
						history = append(history, rightsizev1alpha1.ResizeHistoryEntry{
							Timestamp: now,
							Workload:  rec.Workload,
							Container: containerRec.Name,
							Resource:  res.Resource,
							From:      res.From.String(),
							To:        res.To.String(),
							Method:    "InPlace",
							Result:    "Failed",
						})
						operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, rec.Workload, res.Resource, "failed").Inc()
					}
					continue
				}

				totalResized++
				for _, res := range results {
					result := "Success"
					if !res.Success {
						result = "Failed"
					}
					history = append(history, rightsizev1alpha1.ResizeHistoryEntry{
						Timestamp: now,
						Workload:  rec.Workload,
						Container: containerRec.Name,
						Resource:  res.Resource,
						From:      res.From.String(),
						To:        res.To.String(),
						Method:    "InPlace",
						Result:    result,
					})
					if res.Success {
						operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, rec.Workload, res.Resource, "success").Inc()
					}
				}

				// Track resize via pod annotations for deferred safety observation.
				if pod.Annotations == nil {
					pod.Annotations = make(map[string]string)
				}
				pod.Annotations["rightsize.io/resized-at"] = time.Now().UTC().Format(time.RFC3339)
				pod.Annotations["rightsize.io/resized-container"] = containerRec.Name
				pod.Annotations["rightsize.io/original-cpu-request"] = containerRec.Current.CPURequest.String()
				pod.Annotations["rightsize.io/original-memory-request"] = containerRec.Current.MemoryRequest.String()

				// Safety check (if autoRevert is enabled)
				if policy.Spec.UpdateStrategy.AutoRevert {
					observationEnd := time.Now().Add(30 * time.Second)
					var originalResources corev1.ResourceRequirements
					for _, c := range pod.Spec.Containers {
						if c.Name == containerRec.Name {
							originalResources = c.Resources
							break
						}
					}
					record := safety.ResizeRecord{
						PodName:           pod.Name,
						Namespace:         pod.Namespace,
						Container:         containerRec.Name,
						OriginalResources: originalResources,
						NewResources:      target,
						ResizedAt:         time.Now(),
						ObservationEnd:    observationEnd,
					}

					verdict, err := monitor.CheckPod(ctx, record)
					if err != nil {
						logger.Error(err, "Safety check failed", "pod", pod.Name)
					}
					if !verdict.Safe {
						logger.Info("Safety violation detected, reverting",
							"pod", pod.Name, "reason", verdict.Reason)
						if revertErr := monitor.RevertPod(ctx, record); revertErr != nil {
							logger.Error(revertErr, "Failed to revert pod", "pod", pod.Name)
						}
						operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, rec.Workload, verdict.Reason).Inc()
						operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, rec.Workload, containerRec.Name, "reverted").Inc()
						// Update history entry to Reverted
						for i := len(history) - 1; i >= 0; i-- {
							if history[i].Workload == rec.Workload && history[i].Container == containerRec.Name {
								history[i].Result = "Reverted"
							}
						}
						totalResized--
					}
				}
			}
		}
	}

	return totalResized, history
}

// checkPendingSafetyObservations checks pods that were previously resized and
// annotated with tracking annotations. For each pod whose observation period
// has elapsed, it runs a safety check. Unsafe pods are reverted to their
// original resource values and the annotations are removed.
func (r *RightSizePolicyReconciler) checkPendingSafetyObservations(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		return
	}

	// List pods with the resize-tracking annotation in the policy's namespace.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(policy.Namespace)); err != nil {
		logger.Error(err, "Failed to list pods for safety observation")
		return
	}

	monitor := safety.NewMonitor(r.Clientset, logger)

	observationPeriod := defaultObservationPeriod
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		observationPeriod = policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		resizedAtStr, ok := pod.Annotations["rightsize.io/resized-at"]
		if !ok {
			continue
		}

		resizedAt, err := time.Parse(time.RFC3339, resizedAtStr)
		if err != nil {
			logger.Error(err, "Failed to parse resized-at annotation", "pod", pod.Name)
			continue
		}

		// Skip if the observation period hasn't elapsed yet.
		if time.Since(resizedAt) < observationPeriod {
			continue
		}

		originalCPUStr := pod.Annotations["rightsize.io/original-cpu-request"]
		originalMemStr := pod.Annotations["rightsize.io/original-memory-request"]

		originalCPU, err := resource.ParseQuantity(originalCPUStr)
		if err != nil {
			logger.Error(err, "Failed to parse original CPU annotation", "pod", pod.Name, "value", originalCPUStr)
			continue
		}
		originalMem, err := resource.ParseQuantity(originalMemStr)
		if err != nil {
			logger.Error(err, "Failed to parse original memory annotation", "pod", pod.Name, "value", originalMemStr)
			continue
		}

		// Use the tracked container name from the annotation.
		containerName := pod.Annotations["rightsize.io/resized-container"]
		var currentResources corev1.ResourceRequirements
		for _, c := range pod.Spec.Containers {
			if c.Name == containerName {
				currentResources = c.Resources
				break
			}
		}

		record := safety.ResizeRecord{
			PodName:   pod.Name,
			Namespace: pod.Namespace,
			Container: containerName,
			OriginalResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    originalCPU,
					corev1.ResourceMemory: originalMem,
				},
			},
			NewResources:   currentResources,
			ResizedAt:      resizedAt,
			ObservationEnd: resizedAt.Add(observationPeriod),
		}

		verdict, err := monitor.CheckPod(ctx, record)
		if err != nil {
			logger.Error(err, "Safety observation check failed", "pod", pod.Name)
			continue
		}

		if !verdict.Safe {
			logger.Info("Deferred safety violation detected, reverting",
				"pod", pod.Name, "reason", verdict.Reason)
			if revertErr := monitor.RevertPod(ctx, record); revertErr != nil {
				logger.Error(revertErr, "Failed to revert pod during safety observation", "pod", pod.Name)
			}
			operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, pod.Labels["app"], verdict.Reason).Inc()
		}

		// Remove tracking annotations regardless of outcome (observation complete).
		delete(pod.Annotations, "rightsize.io/resized-at")
		delete(pod.Annotations, "rightsize.io/resized-container")
		delete(pod.Annotations, "rightsize.io/original-cpu-request")
		delete(pod.Annotations, "rightsize.io/original-memory-request")
		if updateErr := r.Update(ctx, pod); updateErr != nil {
			logger.Error(updateErr, "Failed to remove resize tracking annotations", "pod", pod.Name)
		}
	}
}

// appendHistory appends new entries to existing history, capping at maxEntries.
//
//nolint:unparam // maxEntries is a parameter for configurability
func appendHistory(existing []rightsizev1alpha1.ResizeHistoryEntry,
	newEntries []rightsizev1alpha1.ResizeHistoryEntry, maxEntries int) []rightsizev1alpha1.ResizeHistoryEntry {
	result := append(existing, newEntries...)
	if len(result) > maxEntries {
		result = result[len(result)-maxEntries:]
	}
	return result
}

// mergeDefaults reads the cluster-scoped RightSizeDefaults and merges values
// into the policy where the policy has not specified its own values.
func (r *RightSizePolicyReconciler) mergeDefaults(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) {
	var defaultsList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &defaultsList); err != nil || len(defaultsList.Items) == 0 {
		return
	}
	defaults := defaultsList.Items[0].Spec

	// Merge CPU config
	if policy.Spec.CPU.Percentile == 0 && defaults.CPU != nil {
		policy.Spec.CPU.Percentile = defaults.CPU.Percentile
	}
	if policy.Spec.CPU.SafetyMargin == "" && defaults.CPU != nil {
		policy.Spec.CPU.SafetyMargin = defaults.CPU.SafetyMargin
	}

	// Merge Memory config
	if policy.Spec.Memory.Percentile == 0 && defaults.Memory != nil {
		policy.Spec.Memory.Percentile = defaults.Memory.Percentile
	}
	if policy.Spec.Memory.SafetyMargin == "" && defaults.Memory != nil {
		policy.Spec.Memory.SafetyMargin = defaults.Memory.SafetyMargin
	}

	// Merge UpdateStrategy mode
	if policy.Spec.UpdateStrategy.Mode == "" && defaults.UpdateStrategy != nil {
		policy.Spec.UpdateStrategy.Mode = defaults.UpdateStrategy.Mode
	}
}

// updateStatusWithRetry performs a status update with a retry on conflict.
// If the first attempt fails with a conflict, it re-fetches the policy,
// re-applies the status fields, and retries once.
func (r *RightSizePolicyReconciler) updateStatusWithRetry(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, key types.NamespacedName) error {
	err := r.Status().Update(ctx, policy)
	if err == nil {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return err
	}

	// Conflict: re-fetch and retry.
	logger := log.FromContext(ctx)
	logger.Info("Status update conflict, retrying")
	savedStatus := policy.Status.DeepCopy()
	if fetchErr := r.Get(ctx, key, policy); fetchErr != nil {
		return fetchErr
	}
	policy.Status = *savedStatus
	return r.Status().Update(ctx, policy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}).
		Complete(r)
}
