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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/recommendation"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
	"github.com/SebTardifLabs/kube-rightsize/internal/safety"
	"github.com/SebTardifLabs/kube-rightsize/internal/validation"
)

const (
	// lastResizeAnnotation is the annotation key for tracking last resize time.
	lastResizeAnnotation = "rightsize.io/last-resize-time"

	// Annotation keys for tracking in-flight resizes on pods.
	// Per-container keys use a ".containerName" suffix.
	annotationResizedAt         = "rightsize.io/resized-at"
	annotationResizedContainers = "rightsize.io/resized-containers"
	annotationResizedWorkload   = "rightsize.io/resized-workload"

	// Label for filtering resized pods in safety observation queries.
	labelTracked = "rightsize.io/tracked"

	// Per-container annotation prefixes (suffixed with ".containerName").
	annotationOriginalCPUPrefix          = "rightsize.io/original-cpu-request."
	annotationOriginalMemoryPrefix       = "rightsize.io/original-memory-request."
	annotationOriginalRestartCountPrefix = "rightsize.io/original-restart-count."

	// defaultHistoryWindow is the default history window if not specified.
	defaultHistoryWindow = 7 * 24 * time.Hour

	// defaultCooldown is the default cooldown between reconciliation cycles.
	defaultCooldown = 1 * time.Hour

	// defaultMinimumDataPoints is the minimum number of data points required.
	defaultMinimumDataPoints int32 = 48

	// defaultPrometheusStep is the step interval for Prometheus range queries.
	defaultPrometheusStep = 5 * time.Minute

	// defaultObservationPeriod is the default safety observation window after resize.
	defaultObservationPeriod = 5 * time.Minute
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/status,verbs=get;update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizenamespacedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheuses,verbs=get;list
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get
//+kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch

// RightSizePolicyReconciler reconciles a RightSizePolicy object.
type RightSizePolicyReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	MetricsFactory MetricsCollectorFactory
	Clientset      kubernetes.Interface // for resize subresource calls
	Recorder       events.EventRecorder
	MinCooldown    time.Duration // minimum cooldown floor (default: 1m)
	CollectorTTL   time.Duration // how long unused collectors stay cached (default: 10m)
	collectors     sync.Map      // map[string]*collectorEntry cache
}

// collectorEntry wraps a MetricsCollector with a last-used timestamp
// for TTL-based eviction.
type collectorEntry struct {
	collector rsmetrics.MetricsCollector
	lastUsed  time.Time
}

// MetricsCollectorFactory creates MetricsCollector instances from a Prometheus address.
// This enables dependency injection for testing.
type MetricsCollectorFactory func(address string) (rsmetrics.MetricsCollector, error)

const (
	// maxCollectors bounds the collector cache to prevent memory-based DoS
	// via address rotation in CRD specs.
	maxCollectors = 64
	// collectorTTL is how long an unused collector stays cached before eviction.
	collectorTTL = 10 * time.Minute
)

// getOrCreateCollector returns a cached collector for the address, creating one
// if needed. The cache is bounded at maxCollectors entries. Stale entries
// (unused for collectorTTL) are evicted on each call.
func (r *RightSizePolicyReconciler) getOrCreateCollector(address string) (rsmetrics.MetricsCollector, error) {
	now := time.Now()

	if cached, ok := r.collectors.Load(address); ok {
		entry := cached.(*collectorEntry)
		// Store a new entry to avoid data race on lastUsed with concurrent reconciles.
		r.collectors.Store(address, &collectorEntry{collector: entry.collector, lastUsed: now})
		return entry.collector, nil
	}

	// Evict stale entries before checking capacity.
	ttl := r.CollectorTTL
	if ttl == 0 {
		ttl = collectorTTL
	}
	r.collectors.Range(func(key, value any) bool {
		entry := value.(*collectorEntry)
		if now.Sub(entry.lastUsed) > ttl {
			r.collectors.Delete(key)
		}
		return true
	})

	var count int
	r.collectors.Range(func(_, _ any) bool {
		count++
		return count < maxCollectors
	})
	if count >= maxCollectors {
		return nil, fmt.Errorf("collector cache full (%d entries); refusing new Prometheus address %q", maxCollectors, address)
	}

	collector, err := r.MetricsFactory(address)
	if err != nil {
		return nil, err
	}
	entry := &collectorEntry{collector: collector, lastUsed: now}
	actual, _ := r.collectors.LoadOrStore(address, entry)
	return actual.(*collectorEntry).collector, nil
}

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
	defaults := r.fetchDefaults(ctx, policy.Namespace)
	r.mergeDefaults(&policy, defaults)

	// Step 2: Resolve Prometheus address from spec or RightSizeDefaults.
	prometheusAddr, err := r.resolvePrometheusAddress(ctx, &policy, defaults)
	if err != nil {
		logger.Error(err, "Failed to resolve Prometheus address")
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot resolve Prometheus address: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	collector, err := r.getOrCreateCollector(prometheusAddr)
	if err != nil {
		logger.Error(err, "Failed to create metrics collector", "address", prometheusAddr)
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot create metrics collector: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Step 3: Discover target workloads (before safety check to avoid duplicate API calls).
	workloads, err := r.discoverWorkloads(ctx, &policy)
	if err != nil {
		logger.Error(err, "Failed to discover workloads")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("discover_workloads").Inc()
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonWorkloadDiscoveryFailed,
			fmt.Sprintf("Failed to discover workloads: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Check pending safety observations from previous resizes before computing
	// new recommendations. Uses already-discovered workloads for provenance.
	if policy.Spec.UpdateStrategy.AutoRevert {
		r.checkPendingSafetyObservations(ctx, &policy, collector, workloads)
	}

	logger.Info("Discovered workloads", "count", len(workloads))

	if len(workloads) == 0 {
		earlyNow := metav1.Now()
		policy.Status.LastReconcileTime = &earlyNow
		policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{}
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonInsufficientData, "No matching workloads found")
		return ctrl.Result{RequeueAfter: r.parseCooldown(&policy)}, nil
	}

	// Step 4-8: Process each workload.
	var recommendations []rightsizev1alpha1.WorkloadRecommendation
	var workloadsWithRecs int32
	var globalMaxDataPoints int
	podsByWorkload := make(map[string][]corev1.Pod)
	var totalQueryErrors int
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

		// Clear stale recommendation gauges for this workload before re-setting.
		// Containers that were removed or excluded won't be re-set.
		wlLabels := prometheus.Labels{"namespace": policy.Namespace, "workload": workloadName}
		operatormetrics.RecommendationCPU.DeletePartialMatch(wlLabels)
		operatormetrics.RecommendationMemory.DeletePartialMatch(wlLabels)
		operatormetrics.Confidence.DeletePartialMatch(wlLabels)

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
		if policyConflict := conflictDetector.CheckPolicyConflictInMemory(policyList, workloadName, workloadKind, workload.GetLabels(), policy.Name, policy.Spec.Weight); policyConflict != nil {
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
			operatormetrics.ReconcileErrorsTotal.WithLabelValues("get_pods").Inc()
			continue
		}

		if len(pods) == 0 {
			logger.Info("No pods found for workload", "workload", workloadName)
			continue
		}

		podsByWorkload[workloadName] = pods

		// Step 7: Compute recommendations for each container.
		rec, qErrors, dataPoints, err := r.computeRecommendations(ctx, &policy, workload, collector)
		totalQueryErrors += qErrors
		if dataPoints > globalMaxDataPoints {
			globalMaxDataPoints = dataPoints
		}
		if err != nil {
			logger.Error(err, "Failed to compute recommendations", "workload", workloadName)
			operatormetrics.ReconcileErrorsTotal.WithLabelValues("compute_recommendations").Inc()
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
	nowMeta := metav1.Now()
	policy.Status.LastReconcileTime = &nowMeta
	minimumDP := r.getMinimumDataPoints(&policy)
	policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{
		Discovered:          safeInt32(len(workloads)),
		WithRecommendations: workloadsWithRecs,
		DataPointsCollected: safeInt32(globalMaxDataPoints),
		DataPointsRequired:  safeInt32(int(minimumDP)),
	}
	// Observe mode: collect data and track progress but don't surface
	// recommendations. This gives a zero-footprint data-collection phase.
	if policy.Spec.UpdateStrategy.Mode != rightsizev1alpha1.ModeObserve {
		policy.Status.Recommendations = recommendations
		policy.Status.Savings = r.computeSavings(policy.Namespace, recommendations, defaults)
	}

	// Step 9: Execute resizes if mode allows.
	mode := policy.Spec.UpdateStrategy.Mode
	cooldownActive := r.isCooldownActive(&policy)
	withinWindow := isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, time.Now())
	if isResizeMode(mode) && !cooldownActive && withinWindow {
		resizedCount, history := r.executeResizes(ctx, &policy, workloads, recommendations, podsByWorkload, collector)
		if resizedCount > 0 {
			policy.Status.Workloads.Resized = safeInt32(resizedCount)
			policy.Status.ResizeHistory = appendHistory(policy.Status.ResizeHistory, history, 20)
		}
	}
	if isResizeMode(mode) && !withinWindow {
		logger.Info("Outside resize window, skipping resize")
	}

	// Preserve the Resized count from a concurrent reconcile that may have
	// already updated the status. Without this, a stale snapshot from this
	// reconcile overwrites the count to 0. This applies both when cooldown
	// is active and when executeResizes returns 0 (pods already at target).
	if isResizeMode(mode) && policy.Status.Workloads.Resized == 0 {
		var latest rightsizev1alpha1.RightSizePolicy
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, &latest); err == nil {
			if latest.Status.Workloads.Resized > 0 {
				policy.Status.Workloads.Resized = latest.Status.Workloads.Resized
				if len(latest.Status.ResizeHistory) > len(policy.Status.ResizeHistory) {
					policy.Status.ResizeHistory = latest.Status.ResizeHistory
				}
			}
		}
	}
	if isResizeMode(mode) && cooldownActive {
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
		reason := rightsizev1alpha1.ReasonInsufficientData
		message := fmt.Sprintf("Collecting data: %d/%d data points (%d%%)",
			globalMaxDataPoints, minimumDP,
			progressPercent(globalMaxDataPoints, int(minimumDP)))
		if totalQueryErrors > 0 {
			reason = rightsizev1alpha1.ReasonPrometheusUnavailable
			message = fmt.Sprintf("Prometheus query errors (%d) prevented data collection; check operator logs", totalQueryErrors)
		}
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
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

	// Step 10: Requeue after cooldown, or sooner if safety observations are pending.
	cooldown := r.parseCooldown(&policy)
	requeueAfter := cooldown
	if policy.Spec.UpdateStrategy.AutoRevert && policy.Status.Workloads.Resized > 0 {
		obs := getObservationPeriod(&policy)
		if obs < requeueAfter {
			requeueAfter = obs
		}
	}
	logger.Info("Reconciliation complete, requeueing", "requeueAfter", requeueAfter)
	operatormetrics.ReconcileDuration.WithLabelValues("rightsizepolicy").Observe(time.Since(startTime).Seconds())
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// computeRecommendations generates resource recommendations for all containers
// in a workload based on Prometheus metrics.
//
//nolint:unparam // error return is part of the interface contract for future use
func (r *RightSizePolicyReconciler) computeRecommendations(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workload client.Object,
	collector rsmetrics.MetricsCollector,
) (rec *rightsizev1alpha1.WorkloadRecommendation, queryErrors int, maxDataPoints int, err error) {
	logger := log.FromContext(ctx)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, 0, 0, nil
	}

	historyWindow := r.parseHistoryWindow(policy)
	minimumDataPoints := r.getMinimumDataPoints(policy)

	now := time.Now()
	start := now.Add(-historyWindow)
	podPrefix := r.getPodPrefix(workload)

	cpuEngine, memEngine := buildRecommendationEngines(policy)

	// Build excludeContainers set for O(1) lookup.
	excludeSet := make(map[string]bool, len(policy.Spec.ExcludeContainers))
	for _, name := range policy.Spec.ExcludeContainers {
		excludeSet[name] = true
	}

	cpuSamplesByContainer, cpuErr := queryMetricsGrouped(ctx, collector, policy.Namespace, podPrefix, "cpu", start, now, defaultPrometheusStep)
	memSamplesByContainer, memErr := queryMetricsGrouped(ctx, collector, policy.Namespace, podPrefix, "memory", start, now, defaultPrometheusStep)
	if cpuErr {
		queryErrors++
	}
	if memErr {
		queryErrors++
	}

	var containerRecs []rightsizev1alpha1.ContainerRecommendation

	for _, container := range containers {
		containerName := container.Name

		if excludeSet[containerName] {
			logger.Info("Skipping excluded container", "container", containerName)
			continue
		}

		cpuSamples := samplesForContainer(cpuSamplesByContainer, containerName)
		memSamples := samplesForContainer(memSamplesByContainer, containerName)

		// Build UsageProfile from samples.
		cpuProfile := rsmetrics.BuildProfile(cpuSamples)
		memProfile := rsmetrics.BuildProfile(memSamples)

		// Track maximum data points across all containers.
		if pts := cpuProfile.DataPoints; pts > maxDataPoints {
			maxDataPoints = pts
		}
		if pts := memProfile.DataPoints; pts > maxDataPoints {
			maxDataPoints = pts
		}

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

		explanation := &rightsizev1alpha1.ContainerRecommendationExplanation{}

		// Compute CPU recommendation.
		if cpuProfile.DataPoints >= int(minimumDataPoints) {
			cpuRec, cpuExplain, _ := cpuEngine.RecommendWithExplanation(cpuProfile, currentCPUReq)
			rec.Recommended.CPURequest = cpuRec
			explanation.CPU = toAPIRecommendationExplanation(cpuExplain)
		}

		// Compute memory recommendation.
		if memProfile.DataPoints >= int(minimumDataPoints) {
			memRec, memExplain, _ := memEngine.RecommendWithExplanation(memProfile, currentMemReq)
			// Enforce AllowDecrease: skip memory decreases unless explicitly allowed.
			allowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
			if !allowDecrease && memRec.Cmp(currentMemReq) < 0 {
				memRec = currentMemReq.DeepCopy()
				memExplain.Final = memRec.DeepCopy()
				memExplain.FinalAdjustment = "memory decrease blocked by allowDecrease=false"
			}
			rec.Recommended.MemoryRequest = memRec
			explanation.Memory = toAPIRecommendationExplanation(memExplain)
		}
		if explanation.CPU != nil || explanation.Memory != nil {
			rec.Explanation = explanation
		}

		// Scale limits proportionally if ControlledValues is RequestsAndLimits.
		cpuControlled := rightsizev1alpha1.ControlledRequestsOnly
		if policy.Spec.CPU.ControlledValues != nil {
			cpuControlled = *policy.Spec.CPU.ControlledValues
		}
		memControlled := rightsizev1alpha1.ControlledRequestsOnly
		if policy.Spec.Memory.ControlledValues != nil {
			memControlled = *policy.Spec.Memory.ControlledValues
		}
		if cpuControlled == rightsizev1alpha1.ControlledRequestsAndLimits {
			rec.Recommended.CPULimit = scaleLimits(currentCPUReq, currentCPULim, rec.Recommended.CPURequest)
		}
		if memControlled == rightsizev1alpha1.ControlledRequestsAndLimits {
			rec.Recommended.MemoryLimit = scaleLimits(currentMemReq, currentMemLim, rec.Recommended.MemoryRequest)
		}

		// Set recommendation gauges for this container.
		operatormetrics.RecommendationCPU.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(float64(rec.Recommended.CPURequest.MilliValue()) / 1000.0)
		operatormetrics.RecommendationMemory.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(float64(rec.Recommended.MemoryRequest.Value()))
		operatormetrics.Confidence.WithLabelValues(policy.Namespace, workload.GetName(), containerName).Set(rec.Confidence)

		containerRecs = append(containerRecs, rec)
	}

	if len(containerRecs) == 0 {
		return nil, queryErrors, maxDataPoints, nil
	}

	return &rightsizev1alpha1.WorkloadRecommendation{
		Containers: containerRecs,
	}, queryErrors, maxDataPoints, nil
}

// resolvePrometheusAddress returns the Prometheus address from the policy spec,
// falling back to the cluster-scoped RightSizeDefaults if not set.
func (r *RightSizePolicyReconciler) resolvePrometheusAddress(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) (string, error) {
	var addr string

	// Check policy-level config first.
	if policy.Spec.MetricsSource.Prometheus != nil &&
		policy.Spec.MetricsSource.Prometheus.Address != "" {
		addr = policy.Spec.MetricsSource.Prometheus.Address
	}

	// Fall back to RightSizeDefaults.
	if addr == "" && defaults != nil &&
		defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Prometheus != nil &&
		defaults.Spec.MetricsSource.Prometheus.Address != "" {
		addr = defaults.Spec.MetricsSource.Prometheus.Address
	}

	// Fall back to auto-discovery: look for Prometheus Operator's Prometheus CRD.
	if addr == "" {
		if discovered := r.discoverPrometheus(ctx); discovered != "" {
			if err := validation.PrometheusAddress(discovered); err != nil {
				log.FromContext(ctx).Error(err, "Auto-discovered Prometheus address failed SSRF validation", "address", discovered)
			} else {
				log.FromContext(ctx).Info("Auto-discovered Prometheus address", "address", discovered)
				return discovered, nil
			}
		}
		return "", fmt.Errorf("no Prometheus address configured in policy or cluster defaults, and auto-discovery found no Prometheus instance")
	}

	// Defense-in-depth: re-validate even if the webhook was supposed to
	// catch SSRF. This protects when webhooks are disabled.
	if err := validation.PrometheusAddress(addr); err != nil {
		return "", fmt.Errorf("SSRF blocked: %w", err)
	}

	return addr, nil
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
	case rightsizev1alpha1.ModeOneShot:
		return eligible[:1]
	case rightsizev1alpha1.ModeCanary:
		count := int(canaryPercentage) * len(eligible) / 100
		if count < 1 {
			count = 1
		}
		if count > len(eligible) {
			count = len(eligible)
		}
		return eligible[:count]
	case rightsizev1alpha1.ModeAuto:
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
	podsByWorkload map[string][]corev1.Pod,
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

	// Per-cycle budget caps. Negative remaining means unlimited.
	cpuBudget := int64(-1)
	memBudget := int64(-1)
	if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
		cpuBudget = policy.Spec.UpdateStrategy.MaxTotalCPUIncrease.MilliValue()
	}
	if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
		memBudget = policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease.Value()
	}

	for _, rec := range recommendations {
		if ctx.Err() != nil {
			logger.Info("Context cancelled, aborting remaining resizes")
			break
		}
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

		// Batch workloads (Job/CronJob) are recommend-only; skip resize.
		if isBatchWorkload(matchedWorkload) {
			continue
		}

		// Use pre-fetched pods (avoids duplicate API call per workload).
		pods := podsByWorkload[rec.Workload]
		selectedPods := selectPodsForResize(pods, mode, canaryPct)
		if len(selectedPods) == 0 {
			continue
		}

		var workloadResized bool
		for _, pod := range selectedPods {
			for _, containerRec := range rec.Containers {
				// Check per-cycle budget caps before resizing.
				if cpuBudget >= 0 || memBudget >= 0 {
					cpuIncrease := containerRec.Recommended.CPURequest.MilliValue() - containerRec.Current.CPURequest.MilliValue()
					memIncrease := containerRec.Recommended.MemoryRequest.Value() - containerRec.Current.MemoryRequest.Value()
					// Only increases consume budget; decreases are free.
					if cpuIncrease < 0 {
						cpuIncrease = 0
					}
					if memIncrease < 0 {
						memIncrease = 0
					}
					if (cpuBudget >= 0 && cpuIncrease > cpuBudget) ||
						(memBudget >= 0 && memIncrease > memBudget) {
						logger.Info("Budget exhausted, deferring resize to next cycle",
							"pod", pod.Name, "container", containerRec.Name,
							"cpuIncrease", cpuIncrease, "cpuBudgetRemaining", cpuBudget,
							"memIncrease", memIncrease, "memBudgetRemaining", memBudget)
						continue
					}
					cpuBudget -= cpuIncrease
					memBudget -= memIncrease
				}
				entries, resized := r.resizeContainer(ctx, policy, &pod, rec.Workload, containerRec, resizer, monitor, now)
				history = append(history, entries...)
				if resized {
					workloadResized = true
				}
			}
		}
		if workloadResized {
			totalResized++
		}
	}

	return totalResized, history
}

// resizeContainer performs a single container resize on a pod, including
// skip checks, the resize call, annotation persistence, and safety checks.
// Returns the history entries produced and whether the resize counted as successful.
func (r *RightSizePolicyReconciler) resizeContainer(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	pod *corev1.Pod,
	workloadName string,
	containerRec rightsizev1alpha1.ContainerRecommendation,
	resizer *resize.PodResizer,
	monitor *safety.Monitor,
	now metav1.Time,
) ([]rightsizev1alpha1.ResizeHistoryEntry, bool) {
	logger := log.FromContext(ctx)
	target := buildResizeTarget(containerRec)

	skip, reason := r.shouldSkipResize(ctx, policy, pod, containerRec, target)
	if skip {
		if reason != "" {
			logger.Info("Skipping resize: "+reason,
				"pod", pod.Name, "container", containerRec.Name)
		}
		return nil, false
	}

	if resize.WouldRestartContainer(pod, containerRec.Name) {
		logger.Info("Container has RestartContainer resize policy; resize will trigger restart",
			"pod", pod.Name, "container", containerRec.Name)
	}

	resizeStart := time.Now()
	results, err := resizer.ResizePod(ctx, pod, containerRec.Name, target)
	if err != nil {
		// Attempt eviction fallback if configured.
		if policy.Spec.UpdateStrategy.ResizeMethod == rightsizev1alpha1.ResizeMethodInPlaceOrEvict {
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workloadName, containerRec.Name, resizer); evicted {
				return []rightsizev1alpha1.ResizeHistoryEntry{
					{
						Timestamp: now, Workload: workloadName, Container: containerRec.Name,
						Resource: "cpu+memory", Method: "Eviction", Result: rightsizev1alpha1.ResultSuccess,
					},
				}, true
			}
		}

		logger.Error(err, "Failed to resize pod",
			"pod", pod.Name, "container", containerRec.Name)
		var entries []rightsizev1alpha1.ResizeHistoryEntry
		for _, res := range results {
			entries = append(entries, newHistoryEntry(now, workloadName, containerRec.Name, res, rightsizev1alpha1.ResultFailed))
			operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "failed").Inc()
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ResizeFailed", "resize",
				"Failed to resize pod %s container %s: %v", pod.Name, containerRec.Name, err)
		}
		return entries, false
	}

	operatormetrics.ResizeDuration.WithLabelValues(pod.Namespace, workloadName).Observe(time.Since(resizeStart).Seconds())

	var history []rightsizev1alpha1.ResizeHistoryEntry
	for _, res := range results {
		result := rightsizev1alpha1.ResultSuccess
		if !res.Success {
			result = rightsizev1alpha1.ResultFailed
		}
		history = append(history, newHistoryEntry(now, workloadName, containerRec.Name, res, result))
		if res.Success {
			operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "success").Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "Resized", "resize",
					"Resized %s %s/%s: %s %s -> %s",
					res.Resource, workloadName, containerRec.Name, res.Resource, res.From.String(), res.To.String())
			}
		}
	}

	originalResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    containerRec.Current.CPURequest.DeepCopy(),
			corev1.ResourceMemory: containerRec.Current.MemoryRequest.DeepCopy(),
		},
	}

	var restartCount int32
	for _, cs := range slices.Concat(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses) {
		if cs.Name == containerRec.Name {
			restartCount = cs.RestartCount
			break
		}
	}

	// revert reverts the resize and marks all history entries as Reverted.
	revert := func(reason string) {
		revertRecord := safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerRec.Name,
			OriginalResources: originalResources,
		}
		if revertErr := monitor.RevertPod(ctx, revertRecord); revertErr != nil {
			logger.Error(revertErr, "Failed to revert pod after "+reason, "pod", pod.Name)
			return
		}
		operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, workloadName, reason).Inc()
		for _, res := range results {
			if res.Success {
				operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, res.Resource, "reverted").Inc()
			}
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, rightsizev1alpha1.ResultReverted, "revert",
				"Reverted resize on %s/%s: %s", workloadName, containerRec.Name, reason)
		}
		for i := range history {
			if history[i].Workload == workloadName && history[i].Container == containerRec.Name {
				history[i].Result = rightsizev1alpha1.ResultReverted
			}
		}
	}

	// Re-fetch directly from API server (not informer cache) to get
	// fresh resourceVersion after UpdateResize. See #37.
	freshPod, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if getErr != nil {
		logger.Error(getErr, "Failed to re-fetch pod after resize, reverting to avoid untracked resize", "pod", pod.Name)
		revert("re-fetch-failed")
		return history, false
	}

	freshPod.Annotations = ensureAnnotations(freshPod.Annotations)
	freshPod.Annotations[annotationResizedAt] = now.UTC().Format(time.RFC3339)
	freshPod.Annotations[annotationResizedWorkload] = workloadName
	if freshPod.Labels == nil {
		freshPod.Labels = make(map[string]string)
	}
	freshPod.Labels[labelTracked] = "true"
	appendResizedContainer(freshPod, containerRec.Name)
	freshPod.Annotations[annotationOriginalCPUPrefix+containerRec.Name] = containerRec.Current.CPURequest.String()
	freshPod.Annotations[annotationOriginalMemoryPrefix+containerRec.Name] = containerRec.Current.MemoryRequest.String()
	freshPod.Annotations[annotationOriginalRestartCountPrefix+containerRec.Name] = strconv.FormatInt(int64(restartCount), 10)

	if updateErr := r.Update(ctx, freshPod); updateErr != nil {
		logger.Error(updateErr, "Failed to persist resize tracking annotations, reverting resize", "pod", pod.Name)
		revert("annotation-persist-failed")
		return history, false
	}

	if policy.Spec.UpdateStrategy.AutoRevert {
		observationEnd := now.Add(getObservationPeriod(policy))
		record := safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerRec.Name,
			OriginalResources: originalResources,
			NewResources:      target,
			ResizedAt:         now.Time,
			ObservationEnd:    observationEnd,
			RestartCount:      restartCount,
		}
		verdict, err := monitor.CheckPod(ctx, record)
		if err != nil {
			logger.Error(err, "Safety check failed, deferring to observation cycle", "pod", pod.Name)
			return history, true
		}
		if !verdict.Safe {
			logger.Info("Safety violation detected, reverting",
				"pod", pod.Name, "reason", verdict.Reason)
			revert(verdict.Reason)
			return history, false
		}
	}

	return history, true
}

// tryEvictionFallback attempts to evict a pod as a fallback when in-place
// resize fails. It checks safety guards before evicting:
//   - Never evict the last replica of a workload
//   - The Eviction API itself enforces PodDisruptionBudgets
//
// Returns true if the eviction was submitted successfully.
func (r *RightSizePolicyReconciler) tryEvictionFallback(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	pod *corev1.Pod,
	workloadName, containerName string,
	resizer *resize.PodResizer,
) bool {
	logger := log.FromContext(ctx)

	// Safety: never evict the last replica. Count running pods for this workload.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(pod.Namespace),
		client.MatchingLabels(pod.Labels),
	); err != nil {
		logger.Error(err, "Cannot list pods for eviction safety check, skipping eviction")
		return false
	}
	running := 0
	for _, p := range podList.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running++
		}
	}
	if running <= 1 {
		logger.Info("Skipping eviction fallback: would evict the last running replica",
			"pod", pod.Name, "workload", workloadName)
		return false
	}

	// The Eviction API respects PDBs. If the eviction is denied, the error
	// will be a 429 TooManyRequests or 500. We just log and skip.
	if err := resizer.EvictPod(ctx, pod); err != nil {
		logger.Error(err, "Eviction fallback denied (PDB or other constraint)",
			"pod", pod.Name, "workload", workloadName)
		return false
	}

	operatormetrics.ResizeTotal.WithLabelValues(pod.Namespace, workloadName, "eviction", "success").Inc()
	if r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "Evicted", "resize",
			"Evicted pod %s for workload %s container %s: in-place resize failed, falling back to eviction",
			pod.Name, workloadName, containerName)
	}
	logger.Info("Eviction fallback successful",
		"pod", pod.Name, "workload", workloadName, "container", containerName)
	return true
}

// progressPercent returns collected/required as an integer percentage, clamped to 0-99.
func progressPercent(collected, required int) int {
	if required <= 0 {
		return 0
	}
	pct := collected * 100 / required
	if pct > 99 {
		pct = 99
	}
	return pct
}

// ensureAnnotations returns a non-nil annotations map.
func ensureAnnotations(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

func samplesForContainer(grouped map[string][]rsmetrics.Sample, container string) []rsmetrics.Sample {
	if samples, ok := grouped[container]; ok {
		return samples
	}
	return grouped[""]
}

func toAPIRecommendationExplanation(explanation recommendation.RecommendationExplanation) *rightsizev1alpha1.ResourceRecommendationExplanation {
	return &rightsizev1alpha1.ResourceRecommendationExplanation{
		RawPercentile:     explanation.RawPercentile.DeepCopy(),
		SafetyMargin:      explanation.SafetyMargin,
		AfterSafetyMargin: explanation.AfterSafetyMargin.DeepCopy(),
		Confidence:        explanation.Confidence,
		ConfidenceFactor:  explanation.ConfidenceFactor,
		AfterConfidence:   explanation.AfterConfidence.DeepCopy(),
		Bounds: rightsizev1alpha1.ResourceBounds{
			Min: explanation.MinBound.DeepCopy(),
			Max: explanation.MaxBound.DeepCopy(),
		},
		BoundsApplied:       explanation.BoundsApplied,
		AfterBounds:         explanation.AfterBounds.DeepCopy(),
		MinChangePercent:    explanation.MinChangePercent,
		MaxChangePercent:    explanation.MaxChangePercent,
		ChangeFilterApplied: explanation.ChangeFilterApplied,
		AfterChangeFilter:   explanation.AfterChangeFilter.DeepCopy(),
		Final:               explanation.Final.DeepCopy(),
		FinalAdjustment:     explanation.FinalAdjustment,
	}
}

// buildRecommendationEngines creates CPU and memory recommendation engines
// from the policy's configuration, falling back to defaults.
func buildRecommendationEngines(policy *rightsizev1alpha1.RightSizePolicy) (cpuEngine, memEngine *recommendation.RecommendationEngine) {
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

	// Defense-in-depth: clamp maxChangePercent to [1, 100] even if webhook is bypassed.
	maxCPUChange := min(max(float64(policy.Spec.UpdateStrategy.MaxCPUChangePercent), 1), 100)
	maxMemChange := min(max(float64(policy.Spec.UpdateStrategy.MaxMemoryChangePercent), 1), 100)
	cpuEngine = recommendation.NewEngine(cpuPercentile, cpuSafetyMargin, cpuBoundsMin, cpuBoundsMax, maxCPUChange, true)
	memEngine = recommendation.NewEngine(memPercentile, memSafetyMargin, memBoundsMin, memBoundsMax, maxMemChange)
	return cpuEngine, memEngine
}

// buildResizeTarget constructs the target ResourceRequirements from a container recommendation.
// Limits are only included when the recommendation has non-zero limits (i.e., controlledValues
// is RequestsAndLimits and the original pod had limits set). This avoids trying to ADD limits
// to pods that never had them, which Kubernetes rejects.
func buildResizeTarget(rec rightsizev1alpha1.ContainerRecommendation) corev1.ResourceRequirements {
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    rec.Recommended.CPURequest.DeepCopy(),
			corev1.ResourceMemory: rec.Recommended.MemoryRequest.DeepCopy(),
		},
	}
	if !rec.Recommended.CPULimit.IsZero() || !rec.Recommended.MemoryLimit.IsZero() {
		target.Limits = corev1.ResourceList{}
		if !rec.Recommended.CPULimit.IsZero() {
			target.Limits[corev1.ResourceCPU] = rec.Recommended.CPULimit.DeepCopy()
		}
		if !rec.Recommended.MemoryLimit.IsZero() {
			target.Limits[corev1.ResourceMemory] = rec.Recommended.MemoryLimit.DeepCopy()
		}
	}
	return target
}

// shouldSkipResize runs pre-checks and returns whether to skip the resize
// and an optional reason string. An empty reason with skip=true means the
// pod already matches the recommendation (no log needed).
func (r *RightSizePolicyReconciler) shouldSkipResize(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	pod *corev1.Pod,
	containerRec rightsizev1alpha1.ContainerRecommendation,
	target corev1.ResourceRequirements,
) (skip bool, reason string) {
	// Already at target (search both regular and init containers).
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name == containerRec.Name {
			if c.Resources.Requests.Cpu().MilliValue() == containerRec.Recommended.CPURequest.MilliValue() &&
				c.Resources.Requests.Memory().Value() == containerRec.Recommended.MemoryRequest.Value() {
				return true, ""
			}
			break
		}
	}

	// Node allocatable exceeded.
	if pod.Spec.NodeName != "" {
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err == nil {
			totalCPU := int64(0)
			totalMem := int64(0)
			for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
				if c.Name == containerRec.Name {
					totalCPU += target.Requests.Cpu().MilliValue()
					totalMem += target.Requests.Memory().Value()
				} else {
					totalCPU += c.Resources.Requests.Cpu().MilliValue()
					totalMem += c.Resources.Requests.Memory().Value()
				}
			}
			if totalCPU > node.Status.Allocatable.Cpu().MilliValue() ||
				totalMem > node.Status.Allocatable.Memory().Value() {
				return true, "total pod requests would exceed node allocatable"
			}
		}
	}

	// Quota/LimitRange violation.
	currentRes := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    containerRec.Current.CPURequest.DeepCopy(),
			corev1.ResourceMemory: containerRec.Current.MemoryRequest.DeepCopy(),
		},
	}
	if err := r.checkQuotaCompatibility(ctx, pod.Namespace, currentRes, target); err != nil {
		return true, "quota/limitrange violation: " + err.Error()
	}

	// QoS class change.
	if !resize.PreservesQoS(pod, containerRec.Name, target) {
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ResizeSkipped", "resize",
				"Skipping resize for pod %s container %s: would change QoS class from Guaranteed. "+
					"Set controlledValues: RequestsAndLimits to resize Guaranteed pods",
				pod.Name, containerRec.Name)
		}
		return true, "would change QoS class"
	}

	return false, ""
}

// checkPendingSafetyObservations checks pods that were previously resized and
// annotated with tracking annotations. For each pod whose observation period
// has elapsed, it runs a safety check. Unsafe pods are reverted to their
// original resource values and the annotations are removed.
func (r *RightSizePolicyReconciler) checkPendingSafetyObservations(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, collector rsmetrics.MetricsCollector, workloads []client.Object) {
	logger := log.FromContext(ctx)
	if r.Clientset == nil {
		return
	}

	// List only pods with the tracking label (set during resize).
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(policy.Namespace), client.MatchingLabels{labelTracked: "true"}); err != nil {
		logger.Error(err, "Failed to list pods for safety observation")
		return
	}

	monitor := r.newSafetyMonitor(logger, collector)
	observationPeriod := getObservationPeriod(policy)

	// Build a set of workload names this policy targets for provenance checks.
	workloadNames := make(map[string]bool, len(workloads))
	for _, w := range workloads {
		workloadNames[w.GetName()] = true
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Provenance check: only process pods whose tracked workload matches
		// this policy's targets. Prevents spoofed annotations from triggering
		// reverts via the operator's elevated permissions.
		trackedWorkload := pod.Annotations[annotationResizedWorkload]
		if trackedWorkload == "" || !workloadNames[trackedWorkload] {
			continue
		}

		records, err := parseResizeRecords(pod, observationPeriod)
		if err != nil {
			if !errors.Is(err, errNotReady) {
				logger.Error(err, "Failed to parse resize records", "pod", pod.Name)
			}
			continue
		}

		var revertFailed bool
		for _, record := range records {
			verdict, err := monitor.CheckPod(ctx, record)
			if err != nil {
				logger.Error(err, "Safety observation check failed", "pod", pod.Name, "container", record.Container)
				revertFailed = true
				continue
			}

			if !verdict.Safe {
				logger.Info("Deferred safety violation detected, reverting",
					"pod", pod.Name, "container", record.Container, "reason", verdict.Reason)
				if revertErr := monitor.RevertPod(ctx, record); revertErr != nil {
					logger.Error(revertErr, "Failed to revert pod during safety observation", "pod", pod.Name)
					revertFailed = true
					continue
				}
				workloadName := pod.Annotations[annotationResizedWorkload]
				operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, workloadName, verdict.Reason).Inc()
				if r.Recorder != nil {
					r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, rightsizev1alpha1.ResultReverted, "revert",
						"Safety observation reverted resize on pod %s/%s: %s", pod.Name, record.Container, verdict.Message)
				}
			}
		}

		// Only remove tracking annotations if all reverts succeeded.
		// If any failed, keep annotations so the next reconciliation retries.
		if revertFailed {
			continue
		}
		removeTrackingAnnotations(pod)
		if updateErr := r.Update(ctx, pod); updateErr != nil {
			logger.Error(updateErr, "Failed to remove resize tracking annotations", "pod", pod.Name)
		}
	}
}

// errNotReady is a sentinel error indicating the pod's observation period hasn't elapsed yet.
var errNotReady = fmt.Errorf("observation period not elapsed")

// parseResizeRecords extracts safety.ResizeRecords from a pod's tracking
// annotations, one per resized container. Returns errNotReady if the
// observation period hasn't elapsed or the pod has no tracking annotations.
func parseResizeRecords(pod *corev1.Pod, observationPeriod time.Duration) ([]safety.ResizeRecord, error) {
	resizedAtStr, ok := pod.Annotations[annotationResizedAt]
	if !ok {
		return nil, errNotReady
	}

	resizedAt, err := time.Parse(time.RFC3339, resizedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing resized-at annotation: %w", err)
	}

	if time.Since(resizedAt) < observationPeriod {
		return nil, errNotReady
	}

	containerNames := strings.Split(pod.Annotations[annotationResizedContainers], ",")
	var records []safety.ResizeRecord
	for _, containerName := range containerNames {
		containerName = strings.TrimSpace(containerName)
		if containerName == "" {
			continue
		}

		originalCPU, cpuErr := resource.ParseQuantity(pod.Annotations[annotationOriginalCPUPrefix+containerName])
		if cpuErr != nil {
			return nil, fmt.Errorf("parsing original CPU for %s: %w", containerName, cpuErr)
		}
		originalMem, memErr := resource.ParseQuantity(pod.Annotations[annotationOriginalMemoryPrefix+containerName])
		if memErr != nil {
			return nil, fmt.Errorf("parsing original memory for %s: %w", containerName, memErr)
		}

		var currentResources corev1.ResourceRequirements
		for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
			if c.Name == containerName {
				currentResources = c.Resources
				break
			}
		}

		var origRestartCount int32
		if rcStr := pod.Annotations[annotationOriginalRestartCountPrefix+containerName]; rcStr != "" {
			rc, parseErr := strconv.ParseInt(rcStr, 10, 32)
			if parseErr != nil {
				return nil, fmt.Errorf("parsing original restart count for %s: %w", containerName, parseErr)
			}
			origRestartCount = int32(rc)
		}

		records = append(records, safety.ResizeRecord{
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
			RestartCount:   origRestartCount,
		})
	}

	if len(records) == 0 {
		return nil, errNotReady
	}
	return records, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}).
		Complete(r)
}
