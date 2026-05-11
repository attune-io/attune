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

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
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

	// Annotation keys for tracking in-flight resizes on pods.
	annotationResizedAt        = "rightsize.io/resized-at"
	annotationResizedContainer = "rightsize.io/resized-container"
	annotationResizedWorkload  = "rightsize.io/resized-workload"
	annotationOriginalCPU      = "rightsize.io/original-cpu-request"
	annotationOriginalMemory   = "rightsize.io/original-memory-request"

	// defaultHistoryWindow is the default history window if not specified.
	defaultHistoryWindow = 7 * 24 * time.Hour

	// defaultCooldown is the default cooldown between reconciliation cycles.
	defaultCooldown = 1 * time.Hour

	// defaultMinimumDataPoints is the minimum number of data points required.
	defaultMinimumDataPoints int32 = 168

	// defaultPrometheusStep is the step interval for Prometheus range queries.
	defaultPrometheusStep = 5 * time.Minute

	// defaultObservationPeriod is the default safety observation window after resize.
	defaultObservationPeriod = 5 * time.Minute
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/status,verbs=get;update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheuses,verbs=get;list
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get
//+kubebuilder:rbac:groups="",resources=services,verbs=get
//+kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list

// RightSizePolicyReconciler reconciles a RightSizePolicy object.
type RightSizePolicyReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	MetricsFactory MetricsCollectorFactory
	Clientset      kubernetes.Interface // for resize subresource calls
	Recorder       events.EventRecorder
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
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("fetch").Inc()
		return ctrl.Result{}, fmt.Errorf("fetching RightSizePolicy: %w", err)
	}

	// Merge cluster-scoped defaults into the policy.
	// Fetch defaults once and reuse for Prometheus address and cost pricing.
	defaults := r.fetchDefaults(ctx)
	r.mergeDefaults(&policy, defaults)

	// Step 2: Resolve Prometheus address from spec or RightSizeDefaults.
	prometheusAddr, err := r.resolvePrometheusAddress(ctx, &policy, defaults)
	if err != nil {
		logger.Error(err, "Failed to resolve Prometheus address")
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot resolve Prometheus address: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	collector, err := r.MetricsFactory(prometheusAddr)
	if err != nil {
		logger.Error(err, "Failed to create metrics collector", "address", prometheusAddr)
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot create metrics collector: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Check pending safety observations from previous resizes before computing
	// new recommendations.
	if policy.Spec.UpdateStrategy.AutoRevert {
		r.checkPendingSafetyObservations(ctx, &policy, collector)
	}

	// Step 3: Discover target workloads.
	workloads, err := r.discoverWorkloads(ctx, &policy)
	if err != nil {
		logger.Error(err, "Failed to discover workloads")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads").Inc()
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonWorkloadDiscoveryFailed,
			fmt.Sprintf("Failed to discover workloads: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	logger.Info("Discovered workloads", "count", len(workloads))

	if len(workloads) == 0 {
		policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{}
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonInsufficientData, "No matching workloads found")
		return ctrl.Result{RequeueAfter: r.parseCooldown(&policy)}, nil
	}

	// Step 4-8: Process each workload.
	var recommendations []rightsizev1alpha1.WorkloadRecommendation
	var workloadsWithRecs int32
	conflictDetector := conflict.NewDetector(logger)

	// List HPAs in the namespace for conflict detection (once for all workloads).
	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &hpaList, client.InNamespace(policy.Namespace)); err != nil {
		logger.Error(err, "Failed to list HPAs for conflict detection")
	}

	// List VPAs in the namespace for conflict detection (once for all workloads).
	vpaList := conflictDetector.ListVPAs(ctx, r.Client, policy.Namespace)

	// List policies in the namespace for conflict detection (once for all workloads).
	policyList := conflictDetector.ListPolicies(ctx, r.Client, policy.Namespace)

	for _, workload := range workloads {
		workloadName := workload.GetName()
		workloadKind := workload.GetObjectKind().GroupVersionKind().Kind

		// Step 5: Check for opt-out annotation.
		workloadMeta := metav1.ObjectMeta{Annotations: workload.GetAnnotations()}
		if conflictDetector.CheckAnnotationOptOut(workloadMeta) {
			logger.Info("Workload opted out via annotation", "workload", workloadName)
			continue
		}

		// Check for HPA conflict (log warning, don't block).
		if hpaConflict := conflictDetector.CheckHPAConflict(hpaList.Items, workloadName, workloadKind); hpaConflict != nil {
			logger.Info("HPA conflict detected", "workload", workloadName, "hpa", hpaConflict.Name, "message", hpaConflict.Message)
		}

		// Check for VPA conflict (log warning, don't block).
		if vpaConflict := conflictDetector.CheckVPAConflictInMemory(vpaList, workloadName, workloadKind); vpaConflict != nil {
			logger.Info("VPA conflict detected", "workload", workloadName, "vpa", vpaConflict.Name, "message", vpaConflict.Message)
		}

		// Check for higher-weight policy conflict (skip this workload if outranked).
		if policyConflict := conflictDetector.CheckPolicyConflictInMemory(policyList, workloadName, workloadKind, policy.Name, policy.Spec.Weight); policyConflict != nil {
			logger.Info("Higher-weight policy exists, skipping workload", "workload", workloadName, "policy", policyConflict.Name, "message", policyConflict.Message)
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
	policy.Status.Savings = r.computeSavings(policy.Namespace, recommendations, defaults)

	// Step 9: Execute resizes if mode allows.
	mode := policy.Spec.UpdateStrategy.Mode
	cooldownActive := r.isCooldownActive(&policy)
	if isResizeMode(mode) && !cooldownActive {
		resizedCount, history := r.executeResizes(ctx, &policy, workloads, recommendations, collector)
		if resizedCount > 0 {
			policy.Status.Workloads.Resized = safeInt32(resizedCount)
			policy.Status.ResizeHistory = appendHistory(policy.Status.ResizeHistory, history, 20)
		}
	} else if isResizeMode(mode) {
		logger.Info("Cooldown active, skipping resize")
	}

	// Pending = workloads with recommendations that have not been resized yet.
	pending := workloadsWithRecs - policy.Status.Workloads.Resized
	if pending < 0 {
		pending = 0
	}
	policy.Status.Workloads.Pending = pending

	// Set Ready condition.
	if workloadsWithRecs > 0 {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonMonitoring,
			Message:            fmt.Sprintf("Watching %d workloads, %d with recommendations", len(workloads), workloadsWithRecs),
			ObservedGeneration: policy.Generation,
		})
	} else {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             rightsizev1alpha1.ReasonInsufficientData,
			Message:            "No workloads have sufficient data for recommendations",
			ObservedGeneration: policy.Generation,
		})
	}

	// Set Resizing condition.
	r.setResizingCondition(&policy, cooldownActive)

	// Set Degraded condition based on recent revert rate.
	r.setDegradedCondition(&policy)

	// Use a retry loop for the status update to handle resource version conflicts
	// caused by concurrent metadata updates (e.g., cooldown annotations).
	if statusErr := r.updateStatusWithRetry(ctx, &policy, req.NamespacedName); statusErr != nil {
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("status_update").Inc()
		return ctrl.Result{}, fmt.Errorf("updating status: %w", statusErr)
	}

	// Mark resize time AFTER status is written (avoids resourceVersion conflict
	// between metadata and status subresource updates).
	if policy.Status.Workloads.Resized > 0 {
		if err := r.markResizeTime(ctx, &policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking resize time: %w", err)
		}
	}

	// Step 10: Requeue after cooldown.
	cooldown := r.parseCooldown(&policy)
	logger.Info("Reconciliation complete, requeueing", "cooldown", cooldown)
	operatormetrics.ReconcileDuration.WithLabelValues("rightsizepolicy").Observe(time.Since(startTime).Seconds())
	return ctrl.Result{RequeueAfter: cooldown}, nil
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
		cpuPercentile = int(rightsizev1alpha1.DefaultCPUPercentile)
	}
	memPercentile := int(policy.Spec.Memory.Percentile)
	if memPercentile == 0 {
		memPercentile = int(rightsizev1alpha1.DefaultMemoryPercentile)
	}

	cpuSafetyMargin := parseFloat64(policy.Spec.CPU.SafetyMargin, 1.2)
	memSafetyMargin := parseFloat64(policy.Spec.Memory.SafetyMargin, 1.3)

	cpuBoundsMin := rightsizev1alpha1.DefaultCPUBoundsMin.DeepCopy()
	cpuBoundsMax := rightsizev1alpha1.DefaultCPUBoundsMax.DeepCopy()
	if policy.Spec.CPU.Bounds != nil {
		cpuBoundsMin = policy.Spec.CPU.Bounds.Min.DeepCopy()
		cpuBoundsMax = policy.Spec.CPU.Bounds.Max.DeepCopy()
	}

	memBoundsMin := rightsizev1alpha1.DefaultMemoryBoundsMin.DeepCopy()
	memBoundsMax := rightsizev1alpha1.DefaultMemoryBoundsMax.DeepCopy()
	if policy.Spec.Memory.Bounds != nil {
		memBoundsMin = policy.Spec.Memory.Bounds.Min.DeepCopy()
		memBoundsMax = policy.Spec.Memory.Bounds.Max.DeepCopy()
	}

	cpuEngine := recommendation.NewEngine(cpuPercentile, cpuSafetyMargin, cpuBoundsMin, cpuBoundsMax, float64(policy.Spec.UpdateStrategy.MaxCPUChangePercent))
	memEngine := recommendation.NewEngine(memPercentile, memSafetyMargin, memBoundsMin, memBoundsMax, float64(policy.Spec.UpdateStrategy.MaxMemoryChangePercent))

	// Build excludeContainers set for O(1) lookup.
	excludeSet := make(map[string]bool, len(policy.Spec.ExcludeContainers))
	for _, name := range policy.Spec.ExcludeContainers {
		excludeSet[name] = true
	}

	var containerRecs []rightsizev1alpha1.ContainerRecommendation

	for _, container := range containers {
		containerName := container.Name

		if excludeSet[containerName] {
			logger.Info("Skipping excluded container", "container", containerName)
			continue
		}

		// Query Prometheus for CPU and memory metrics (with pod-level fallback).
		cpuSamples := queryMetrics(ctx, collector, policy.Namespace, podPrefix, containerName, "cpu", start, now, defaultPrometheusStep)
		memSamples := queryMetrics(ctx, collector, policy.Namespace, podPrefix, containerName, "memory", start, now, defaultPrometheusStep)

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
			// Enforce AllowDecrease: skip memory decreases unless explicitly allowed.
			allowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
			if !allowDecrease && memRec.Cmp(currentMemReq) < 0 {
				memRec = currentMemReq.DeepCopy()
			}
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
func (r *RightSizePolicyReconciler) resolvePrometheusAddress(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) (string, error) {
	// Check policy-level config first.
	if policy.Spec.MetricsSource.Prometheus != nil &&
		policy.Spec.MetricsSource.Prometheus.Address != "" {
		return policy.Spec.MetricsSource.Prometheus.Address, nil
	}

	// Fall back to RightSizeDefaults.
	if defaults != nil &&
		defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Prometheus != nil &&
		defaults.Spec.MetricsSource.Prometheus.Address != "" {
		return defaults.Spec.MetricsSource.Prometheus.Address, nil
	}

	// Fall back to auto-discovery: look for Prometheus Operator's Prometheus CRD.
	if addr := r.discoverPrometheus(ctx); addr != "" {
		log.FromContext(ctx).Info("Auto-discovered Prometheus address", "address", addr)
		return addr, nil
	}

	return "", fmt.Errorf("no Prometheus address configured in policy or cluster defaults, and auto-discovery found no Prometheus instance")
}

// discoverPrometheus attempts to find a Prometheus instance in the cluster
// by checking for the Prometheus Operator's Prometheus CRD, then falling back
// to well-known service names.
func (r *RightSizePolicyReconciler) discoverPrometheus(ctx context.Context) string {
	logger := log.FromContext(ctx)

	// Try Prometheus Operator CRD: monitoring.coreos.com/v1 Prometheus
	promList := &unstructured.UnstructuredList{}
	promList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "PrometheusList",
	})
	if err := r.List(ctx, promList); err == nil && len(promList.Items) > 0 {
		prom := promList.Items[0]
		ns := prom.GetNamespace()
		name := prom.GetName()
		// Prometheus Operator creates a service named "prometheus-<name>"
		// or the service name matches the Prometheus resource name.
		port := int64(9090)
		if p, found, _ := unstructured.NestedInt64(prom.Object, "spec", "port"); found && p > 0 {
			port = p
		}
		addr := fmt.Sprintf("http://prometheus-%s.%s:%d", name, ns, port)
		return addr
	}

	// Try well-known service names.
	wellKnown := []struct{ namespace, name string }{
		{"monitoring", "prometheus-server"},
		{"monitoring", "prometheus-kube-prometheus-prometheus"},
		{"prometheus", "prometheus-server"},
		{"kube-prometheus-stack", "prometheus-kube-prometheus-prometheus"},
	}
	for _, svc := range wellKnown {
		var service corev1.Service
		if err := r.Get(ctx, types.NamespacedName{Namespace: svc.namespace, Name: svc.name}, &service); err == nil {
			addr := fmt.Sprintf("http://%s.%s:%d", svc.name, svc.namespace, 9090)
			logger.V(1).Info("Found well-known Prometheus service", "address", addr)
			return addr
		}
	}

	return ""
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
	collector rsmetrics.MetricsCollector,
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
	monitor := r.newSafetyMonitor(logger, collector)

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

				// Pre-check: total pod resource requests after resize vs node allocatable.
				if pod.Spec.NodeName != "" {
					var node corev1.Node
					if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err == nil {
						totalCPU := int64(0)
						totalMem := int64(0)
						for _, c := range pod.Spec.Containers {
							if c.Name == containerRec.Name {
								totalCPU += target.Requests.Cpu().MilliValue()
								totalMem += target.Requests.Memory().Value()
							} else {
								totalCPU += c.Resources.Requests.Cpu().MilliValue()
								totalMem += c.Resources.Requests.Memory().Value()
							}
						}
						allocCPU := node.Status.Allocatable.Cpu().MilliValue()
						allocMem := node.Status.Allocatable.Memory().Value()
						if totalCPU > allocCPU || totalMem > allocMem {
							logger.Info("Skipping resize: total pod requests would exceed node allocatable",
								"pod", pod.Name, "container", containerRec.Name,
								"totalCPU", totalCPU, "allocCPU", allocCPU,
								"totalMem", totalMem, "allocMem", allocMem)
							continue
						}
					}
				}

				// Pre-check: LimitRange/ResourceQuota compatibility.
				currentRes := corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    containerRec.Current.CPURequest.DeepCopy(),
						corev1.ResourceMemory: containerRec.Current.MemoryRequest.DeepCopy(),
					},
				}
				if err := r.checkQuotaCompatibility(ctx, pod.Namespace, currentRes, target); err != nil {
					logger.Info("Skipping resize: quota/limitrange violation",
						"pod", pod.Name, "container", containerRec.Name, "reason", err.Error())
					continue
				}

				// Pre-check: QoS preservation
				if !resize.PreservesQoS(&pod, containerRec.Name, target) {
					logger.Info("Skipping resize: would change QoS class",
						"pod", pod.Name, "container", containerRec.Name)
					continue
				}

				// Warn if resize will cause container restart (resizePolicy: RestartContainer).
				if resize.WouldRestartContainer(&pod, containerRec.Name) {
					logger.Info("Container has RestartContainer resize policy; resize will trigger restart",
						"pod", pod.Name, "container", containerRec.Name)
				}

				// Execute resize
				resizeStart := time.Now()
				results, err := resizer.ResizePod(ctx, &pod, containerRec.Name, target)
				if err != nil {
					logger.Error(err, "Failed to resize pod",
						"pod", pod.Name, "container", containerRec.Name)
					for _, res := range results {
						history = append(history, newHistoryEntry(now, rec.Workload, containerRec.Name, res, "Failed"))
						operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, rec.Workload, res.Resource, "failed").Inc()
					}
					continue
				}

				operatormetrics.ResizeDuration.WithLabelValues(pod.Namespace, rec.Workload).Observe(time.Since(resizeStart).Seconds())
				totalResized++
				for _, res := range results {
					result := "Success"
					if !res.Success {
						result = "Failed"
					}
					history = append(history, newHistoryEntry(now, rec.Workload, containerRec.Name, res, result))
					if res.Success {
						operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, rec.Workload, res.Resource, "success").Inc()
						if r.Recorder != nil {
							r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "Resized", "resize",
								"Resized %s %s/%s: %s %s -> %s",
								res.Resource, rec.Workload, containerRec.Name, res.Resource, res.From.String(), res.To.String())
						}
					}
				}

				// Track resize via pod annotations for deferred safety observation.
				if pod.Annotations == nil {
					pod.Annotations = make(map[string]string)
				}
				pod.Annotations[annotationResizedAt] = now.UTC().Format(time.RFC3339)
				pod.Annotations[annotationResizedContainer] = containerRec.Name
				pod.Annotations[annotationResizedWorkload] = rec.Workload
				pod.Annotations[annotationOriginalCPU] = containerRec.Current.CPURequest.String()
				pod.Annotations[annotationOriginalMemory] = containerRec.Current.MemoryRequest.String()

				// Persist tracking annotations to the API server so
				// checkPendingSafetyObservations can find them later.
				if updateErr := r.Update(ctx, &pod); updateErr != nil {
					logger.Error(updateErr, "Failed to persist resize tracking annotations", "pod", pod.Name)
				}

				// Safety check (if autoRevert is enabled)
				if policy.Spec.UpdateStrategy.AutoRevert {
					observationEnd := now.Add(getObservationPeriod(policy))
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
						ResizedAt:         now.Time,
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
						if r.Recorder != nil {
							r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "Reverted", "revert",
								"Reverted resize on %s/%s: %s", rec.Workload, containerRec.Name, verdict.Message)
						}
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
func (r *RightSizePolicyReconciler) checkPendingSafetyObservations(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, collector rsmetrics.MetricsCollector) {
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

	monitor := r.newSafetyMonitor(logger, collector)
	observationPeriod := getObservationPeriod(policy)

	for i := range podList.Items {
		pod := &podList.Items[i]
		resizedAtStr, ok := pod.Annotations[annotationResizedAt]
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

		originalCPUStr := pod.Annotations[annotationOriginalCPU]
		originalMemStr := pod.Annotations[annotationOriginalMemory]

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
		containerName := pod.Annotations[annotationResizedContainer]
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
			workloadName := pod.Annotations[annotationResizedWorkload]
			operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, workloadName, verdict.Reason).Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "Reverted", "revert",
					"Safety observation reverted resize on pod %s: %s", pod.Name, verdict.Message)
			}
		}

		// Remove tracking annotations regardless of outcome (observation complete).
		removeTrackingAnnotations(pod)
		if updateErr := r.Update(ctx, pod); updateErr != nil {
			logger.Error(updateErr, "Failed to remove resize tracking annotations", "pod", pod.Name)
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}).
		Complete(r)
}
