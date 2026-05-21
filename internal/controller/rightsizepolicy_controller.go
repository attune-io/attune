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
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/conflict"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/resize"
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
	annotationOriginalCPULimitPrefix     = "rightsize.io/original-cpu-limit."
	annotationOriginalMemoryLimitPrefix  = "rightsize.io/original-memory-limit."
	annotationOriginalRestartCountPrefix = "rightsize.io/original-restart-count."

	// HPA auto-tune annotations.
	annotationHPAAutoTune    = "rightsize.io/auto-tune"
	annotationHPAOriginalCPU = "rightsize.io/original-target-cpu"

	// Startup boost annotation.
	annotationStartupBoostAt = "rightsize.io/startup-boost-at"

	// annotationPolicy records which RightSizePolicy manages a pod, enabling
	// targeted cleanup when the policy is deleted.
	annotationPolicy = "rightsize.io/policy"

	// finalizerName is the finalizer added to RightSizePolicy resources to
	// ensure pod annotations are cleaned up before the policy is deleted.
	finalizerName = "rightsize.io/cleanup"

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

	// maxWorkloadWorkers is the maximum number of goroutines used to process
	// workloads in parallel within a single reconcile cycle. The Prometheus
	// rate limiter provides the real backpressure; this just caps goroutine
	// count to avoid excessive memory from blocked goroutines.
	maxWorkloadWorkers = 10
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/status,verbs=get;update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizenamespacedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheuses,verbs=get;list
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch

// RightSizePolicyReconciler reconciles a RightSizePolicy object.
type RightSizePolicyReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	MetricsFactory          MetricsCollectorFactory
	Clientset               kubernetes.Interface // for resize subresource calls
	Recorder                events.EventRecorder
	MinCooldown             time.Duration // minimum cooldown floor (default: 1m)
	CollectorTTL            time.Duration // how long unused collectors stay cached (default: 10m)
	MaxConcurrentReconciles int           // max parallel reconcile goroutines (default: 1)
	PrometheusTimeout       time.Duration // max time for Prometheus queries per reconcile (default: 5m)
	nowFunc                 atomic.Pointer[func() time.Time]
	collectors              sync.Map // map[string]*collectorEntry cache
	// gaugeKeys tracks which Prometheus gauge label combinations each policy
	// set on its last reconcile. On the next reconcile, only these specific
	// keys are deleted (not the entire namespace), preventing cross-policy
	// gauge interference. This map is in-memory only; after operator restart
	// it starts empty, so gauges from workloads that disappeared during the
	// restart persist until the owning policy's next reconcile refreshes them.
	gaugeKeys sync.Map // map[string][]gaugeKey

	// discoveredPromMu guards the cached Prometheus auto-discovery result.
	discoveredPromMu   sync.Mutex
	discoveredPromAddr string
	discoveredPromTime time.Time
}

// gaugeKey identifies a specific gauge label combination set by a policy.
type gaugeKey struct {
	Namespace string
	Workload  string
	Container string
}

// workloadProcessingResult holds the aggregated results from processing all
// target workloads in a single reconciliation cycle.
type workloadProcessingResult struct {
	recommendations   []rightsizev1alpha1.WorkloadRecommendation
	workloadsWithRecs int32
	maxDataPoints     int
	totalQueryErrors  int
	queryErrorTypes   map[string]struct{}
	gaugeKeys         []gaugeKey
	hpaList           autoscalingv2.HorizontalPodAutoscalerList
}

// deleteGaugeKeys removes recommendation gauge values for the given keys.
func deleteGaugeKeys(keys []gaugeKey) {
	for _, gk := range keys {
		operatormetrics.RecommendationCPU.DeleteLabelValues(gk.Namespace, gk.Workload, gk.Container)
		operatormetrics.RecommendationMemory.DeleteLabelValues(gk.Namespace, gk.Workload, gk.Container)
		operatormetrics.Confidence.DeleteLabelValues(gk.Namespace, gk.Workload, gk.Container)
		operatormetrics.BurstFactor.DeleteLabelValues(gk.Namespace, gk.Workload, gk.Container, "cpu")
		operatormetrics.BurstFactor.DeleteLabelValues(gk.Namespace, gk.Workload, gk.Container, "memory")
	}
}

// SetNowFunc sets an injectable clock for testing. Safe for concurrent use.
func (r *RightSizePolicyReconciler) SetNowFunc(fn func() time.Time) {
	if fn == nil {
		r.nowFunc.Store(nil)
	} else {
		r.nowFunc.Store(&fn)
	}
}

// now returns the current time, using the injected clock if set, otherwise time.Now.
func (r *RightSizePolicyReconciler) now() time.Time {
	if p := r.nowFunc.Load(); p != nil {
		return (*p)()
	}
	return time.Now()
}

func (r *RightSizePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := r.now()
	logger := log.FromContext(ctx)

	// Step 1: Fetch the RightSizePolicy CR.
	var policy rightsizev1alpha1.RightSizePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("RightSizePolicy resource not found, likely deleted")
			// Clean up gauge values this policy previously set.
			deletedPolicyKey := req.Namespace + "/" + req.Name
			if prev, ok := r.gaugeKeys.LoadAndDelete(deletedPolicyKey); ok {
				if keys, ok := prev.([]gaugeKey); ok {
					deleteGaugeKeys(keys)
				}
			}
			return ctrl.Result{}, nil
		}
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("fetch").Inc()
		return ctrl.Result{}, fmt.Errorf("fetching RightSizePolicy: %w", err)
	}
	if normalizeResizeHistoryMethods(policy.Status.ResizeHistory) {
		logger.V(1).Info("Normalized legacy resize history methods", "entries", len(policy.Status.ResizeHistory))
	}

	// Handle deletion: clean up pod annotations before allowing garbage collection.
	if !policy.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &policy)
	}

	// Ensure finalizer is present so we get a chance to clean up on deletion.
	if !controllerutil.ContainsFinalizer(&policy, finalizerName) {
		patch := client.MergeFrom(policy.DeepCopy())
		controllerutil.AddFinalizer(&policy, finalizerName)
		if err := r.Patch(ctx, &policy, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	// Merge defaults into the policy. Namespace-scoped defaults take precedence,
	// and defaults lookup failures fail closed rather than silently falling back
	// to another scope.
	defaults, err := r.fetchDefaults(ctx, policy.Namespace)
	if err != nil {
		logger.Error(err, "Failed to fetch defaults")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("fetch_defaults").Inc()
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonInvalidConfig,
			fmt.Sprintf("Failed to fetch defaults: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}
	r.mergeDefaults(&policy, defaults)
	r.applyBuiltInDefaults(&policy)

	// Step 2: Resolve Prometheus address and config from spec or RightSizeDefaults.
	promConfig, err := r.resolvePrometheusConfig(ctx, &policy, defaults)
	if err != nil {
		logger.Error(err, "Failed to resolve Prometheus config")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("prometheus_config").Inc()
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot resolve Prometheus config: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Build collector options from the PrometheusConfig.
	collectorOpts, err := r.buildCollectorOptions(ctx, policy.Namespace, promConfig)
	if err != nil {
		logger.Error(err, "Failed to build collector options")
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("collector_options").Inc()
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	collector, err := r.getOrCreateCollector(promConfig, collectorOpts)
	if err != nil {
		logger.Error(err, "Failed to create metrics collector", "address", promConfig.Address)
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("collector_create").Inc()
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
	var safetyObservationsPending bool
	if autoRevertEnabled(policy.Spec.UpdateStrategy) {
		safetyObservationsPending = r.checkPendingSafetyObservations(ctx, &policy, collector, workloads)
	}

	logger.Info("Discovered workloads", "count", len(workloads))

	if len(workloads) == 0 {
		earlyNow := metav1.NewTime(r.now())
		policy.Status.LastReconcileTime = &earlyNow
		policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{}
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonInsufficientData, "No matching workloads found")
		return ctrl.Result{RequeueAfter: r.parseCooldown(&policy)}, nil
	}

	// Step 4-8: Process each workload with a timeout to prevent indefinite
	// stalls when Prometheus is unresponsive.
	promTimeout := r.PrometheusTimeout
	if promTimeout <= 0 {
		promTimeout = 5 * time.Minute
	}
	workloadCtx, workloadCancel := context.WithTimeout(ctx, promTimeout)
	defer workloadCancel()
	wpResult := r.processWorkloads(workloadCtx, &policy, workloads, collector)
	promTimedOut := workloadCtx.Err() == context.DeadlineExceeded
	if promTimedOut {
		logger.Info("Prometheus query timeout exceeded, using partial results",
			"timeout", promTimeout,
			"workloadsWithRecommendations", wpResult.workloadsWithRecs,
			"workloadsTotal", len(workloads))
	}
	recommendations := wpResult.recommendations
	workloadsWithRecs := wpResult.workloadsWithRecs
	globalMaxDataPoints := wpResult.maxDataPoints
	totalQueryErrors := wpResult.totalQueryErrors
	queryErrorTypes := wpResult.queryErrorTypes
	hpaList := wpResult.hpaList

	// Emit event when recommendations first become available.
	previousWithRecs := policy.Status.Workloads.WithRecommendations
	if previousWithRecs == 0 && workloadsWithRecs > 0 && r.Recorder != nil {
		r.Recorder.Eventf(&policy, nil, corev1.EventTypeNormal, "RecommendationsReady", "recommend",
			"First recommendations available: %d of %d workloads have sizing data",
			workloadsWithRecs, len(workloads))
	}

	// Step 8: Update status fields.
	nowMeta := metav1.NewTime(r.now())
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
	if policy.Spec.UpdateStrategy.Mode == rightsizev1alpha1.UpdateModeObserve {
		logger.V(1).Info("Observe mode: recommendations computed but not surfaced in status",
			"workloadsWithRecs", len(recommendations))
		// Reset savings gauges in Observe mode so they don't show stale values.
		updateSavingsGauges(policy.Namespace, nil, nil)
	} else {
		policy.Status.Recommendations = recommendations
		policy.Status.Savings = r.computeSavings(recommendations, defaults)
		updateSavingsGauges(policy.Namespace, recommendations, defaults)
	}

	// Export recommendations to ConfigMaps for GitOps workflows if configured.
	if policy.Spec.UpdateStrategy.Export != nil && policy.Spec.UpdateStrategy.Export.ConfigMap && len(recommendations) > 0 {
		r.exportRecommendationConfigMaps(ctx, &policy, recommendations)
	}

	// Step 9: Execute resizes if mode allows.
	mode := policy.Spec.UpdateStrategy.Mode
	cooldownActive := r.isCooldownActive(&policy)
	withinWindow := isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now())
	var newResizedCount int

	// Build podsByWorkload once so both executeResizes and applyStartupBoosts
	// can reuse it, avoiding duplicate getPodsForWorkload calls per cycle.
	// Only pre-fetch pods for workloads that have recommendations, not all
	// discovered workloads (the fallback in executeResizes handles misses).
	var podsByWorkload map[string][]corev1.Pod
	needPods := isResizeMode(mode) && ((!cooldownActive && withinWindow) ||
		(policy.Spec.CPU.StartupBoost != nil && r.Clientset != nil && len(recommendations) > 0))
	if needPods {
		recWorkloads := make(map[string]bool, len(recommendations))
		for _, rec := range recommendations {
			recWorkloads[rec.Workload] = true
		}
		podsByWorkload = make(map[string][]corev1.Pod, len(recWorkloads))
		for _, w := range workloads {
			if !recWorkloads[w.GetName()] {
				continue
			}
			pods, err := r.getPodsForWorkload(ctx, w)
			if err != nil {
				logger.Error(err, "Failed to get pods for workload", "workload", w.GetName())
				continue
			}
			podsByWorkload[w.GetName()] = pods
		}
	}

	// Pre-fetch namespace-scoped LimitRanges and ResourceQuotas once so both
	// executeResizes and applyStartupBoosts can reuse them without duplicate
	// API calls.
	var preChecks *resizePreChecks
	if needPods {
		preChecks = r.buildResizePreChecks(ctx, &policy)
	}

	if isResizeMode(mode) && !cooldownActive && withinWindow {
		resizedCount, history := r.executeResizes(ctx, &policy, workloads, recommendations, podsByWorkload, collector, preChecks)
		newResizedCount = resizedCount
		// Always record history entries (including immediate reverts) so
		// the escalation mechanisms (consecutiveReverts, Degraded condition,
		// exponential backoff) work for all revert reasons.
		if len(history) > 0 {
			policy.Status.ResizeHistory = appendHistory(policy.Status.ResizeHistory, history, 20)
		}
		if resizedCount > 0 {
			policy.Status.Workloads.Resized = safeInt32(resizedCount)
			// Auto-tune HPA targets only for workloads that were actually
			// resized, using aggregate CPU across all containers.
			resizedWorkloads := make(map[string]bool, len(history))
			for _, h := range history {
				if isSuccessfulInPlaceHistory(h) {
					resizedWorkloads[h.Workload] = true
				}
			}
			for _, rec := range recommendations {
				if !resizedWorkloads[rec.Workload] {
					continue
				}
				var totalOldCPU, totalNewCPU int64
				for _, c := range rec.Containers {
					totalOldCPU += c.Current.CPURequest.MilliValue()
					totalNewCPU += c.Recommended.CPURequest.MilliValue()
				}
				if totalOldCPU != totalNewCPU {
					r.adjustHPATargets(ctx, hpaList.Items, rec.Workload, rec.Kind,
						*resource.NewMilliQuantity(totalOldCPU, resource.DecimalSI),
						*resource.NewMilliQuantity(totalNewCPU, resource.DecimalSI))
				}
			}
		}
	}
	// Apply startup CPU boosts for newly created pods if configured.
	// Only in resize modes (Auto, OneShot, Canary); Observe and Recommend
	// modes must not modify pod resources.
	if isResizeMode(mode) && policy.Spec.CPU.StartupBoost != nil && r.Clientset != nil && len(recommendations) > 0 {
		resizer := resize.NewPodResizer(r.Clientset, logger)
		r.applyStartupBoosts(ctx, &policy, podsByWorkload, recommendations, resizer, preChecks)
	}

	if isResizeMode(mode) && !withinWindow {
		logger.Info("Outside resize window, skipping resize")
		operatormetrics.ScheduleSkippedTotal.WithLabelValues(policy.Namespace, policy.Name).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(&policy, nil, corev1.EventTypeNormal, "ScheduleSkipped", "resize",
				"Resize deferred: outside configured schedule window")
		}
	}

	// Set ScheduleBlocked condition when a schedule is configured.
	r.setScheduleBlockedCondition(&policy, withinWindow)

	// Derive the Resized count from history to self-heal from race conditions
	// where a concurrent reconcile overwrote Resized to 0 after a successful
	// resize. The resize history is preserved through races because
	// updateStatusWithRetry keeps the longer history on conflict retry.
	if isResizeMode(mode) && policy.Status.Workloads.Resized == 0 {
		resizedWorkloads := make(map[string]bool)
		for _, h := range policy.Status.ResizeHistory {
			if isSuccessfulInPlaceHistory(h) {
				resizedWorkloads[h.Workload] = true
			}
		}
		if derived := safeInt32(len(resizedWorkloads)); derived > policy.Status.Workloads.Resized {
			policy.Status.Workloads.Resized = derived
		}
	}
	if isResizeMode(mode) && cooldownActive {
		logger.Info("Cooldown active, skipping resize")
	}

	// Expose effective cooldown status with backoff details.
	r.setCooldownStatus(&policy)

	// Pending = workloads with recommendations that have not been resized yet.
	pending := workloadsWithRecs - policy.Status.Workloads.Resized
	if pending < 0 {
		pending = 0
	}
	policy.Status.Workloads.Pending = pending

	// Set Ready condition.
	r.setReadyCondition(&policy, len(workloads), workloadsWithRecs, totalQueryErrors, queryErrorTypes, globalMaxDataPoints, promTimedOut, promTimeout)

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
	// between metadata and status subresource updates). Only set when a new
	// resize actually happened this cycle (not when Resized was derived from
	// history), to avoid resetting the cooldown timer spuriously.
	if newResizedCount > 0 {
		if err := r.markResizeTime(ctx, &policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking resize time: %w", err)
		}
	}

	// Step 10: Requeue after cooldown, or sooner if safety observations are pending.
	cooldown := r.parseCooldown(&policy)
	requeueAfter := cooldown
	if autoRevertEnabled(policy.Spec.UpdateStrategy) && (policy.Status.Workloads.Resized > 0 || safetyObservationsPending) {
		obs := getObservationPeriod(&policy)
		if obs < requeueAfter {
			requeueAfter = obs
		}
	}
	logger.Info("Reconciliation complete, requeueing", "requeueAfter", requeueAfter)
	operatormetrics.ReconcileDuration.WithLabelValues("rightsizepolicy", policy.Namespace, policy.Name).Observe(time.Since(startTime).Seconds())
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// processWorkloads processes discovered workloads in parallel, checking for
// conflicts and opt-outs, computing recommendations, and returning the
// aggregated results. Workloads are processed by a bounded worker pool
// (maxWorkloadWorkers goroutines). The Prometheus rate limiter provides
// backpressure so workers that exceed the QPS budget block automatically.
func (r *RightSizePolicyReconciler) processWorkloads(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workloads []client.Object,
	collector rsmetrics.MetricsCollector,
) workloadProcessingResult {
	logger := log.FromContext(ctx)
	result := workloadProcessingResult{
		queryErrorTypes: make(map[string]struct{}),
	}

	conflictDetector := conflict.NewDetector(logger)

	// List HPAs, VPAs, and policies once for all workloads. The HPA list is
	// also returned in the result so Reconcile can reuse it for HPA auto-tune
	// without a redundant API call.
	if err := r.List(ctx, &result.hpaList, client.InNamespace(policy.Namespace)); err != nil {
		logger.Error(err, "Failed to list HPAs for conflict detection")
	}
	vpaList := conflictDetector.ListVPAs(ctx, r.Client, policy.Namespace)
	policyList := conflictDetector.ListPolicies(ctx, r.Client, policy.Namespace)

	// Clear gauge values that THIS policy previously set.
	policyKey := policy.Namespace + "/" + policy.Name
	if prev, ok := r.gaugeKeys.Load(policyKey); ok {
		if keys, ok := prev.([]gaugeKey); ok {
			deleteGaugeKeys(keys)
		}
	}

	cpuEngine, memEngine := buildRecommendationEngines(policy)
	excludeSet := make(map[string]bool, len(policy.Spec.ExcludeContainers))
	for _, name := range policy.Spec.ExcludeContainers {
		excludeSet[name] = true
	}

	// Process workloads in parallel. The errgroup context propagates
	// cancellation to all workers if the parent context is cancelled.
	var mu sync.Mutex
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxWorkloadWorkers)

	for _, workload := range workloads {
		g.Go(func() error {
			workloadName := workload.GetName()
			workloadKind := workload.GetObjectKind().GroupVersionKind().Kind

			workloadMeta := metav1.ObjectMeta{Annotations: workload.GetAnnotations()}
			if conflictDetector.CheckAnnotationOptOut(workloadMeta) {
				logger.Info("Workload opted out via annotation", "workload", workloadName)
				return nil
			}

			if hpaConflict := conflictDetector.CheckHPAConflict(result.hpaList.Items, workloadName, workloadKind); hpaConflict != nil {
				logger.Info("HPA conflict detected", "workload", workloadName, "hpa", hpaConflict.Name, "message", hpaConflict.Message)
			}

			if vpaConflict := conflictDetector.CheckVPAConflictInMemory(vpaList, workloadName, workloadKind); vpaConflict != nil {
				logger.Info("VPA conflict detected", "workload", workloadName, "vpa", vpaConflict.Name, "message", vpaConflict.Message)
			}

			if policyConflict := conflictDetector.CheckPolicyConflictInMemory(policyList, workloadName, workloadKind, workload.GetLabels(), policy.Name, policy.Spec.Weight); policyConflict != nil {
				logger.Info("Higher-weight policy exists, skipping workload", "workload", workloadName, "policy", policyConflict.Name, "message", policyConflict.Message)
				return nil
			}

			if r.isRollingOut(workload) {
				logger.Info("Skipping workload mid-rollout", "workload", workloadName)
				return nil
			}

			rec, qErrors, failedMetricTypes, dataPoints, err := r.computeRecommendations(gCtx, policy, workload, collector, cpuEngine, memEngine, excludeSet)
			// Log and record metrics outside the lock (both are goroutine-safe).
			if err != nil {
				logger.Error(err, "Failed to compute recommendations", "workload", workloadName)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("compute_recommendations").Inc()
			}

			// Single critical section for result aggregation.
			mu.Lock()
			result.totalQueryErrors += qErrors
			for _, metricType := range failedMetricTypes {
				result.queryErrorTypes[metricType] = struct{}{}
			}
			if dataPoints > result.maxDataPoints {
				result.maxDataPoints = dataPoints
			}
			if err == nil && rec != nil {
				rec.Workload = workloadName
				rec.Kind = workloadKind
				result.recommendations = append(result.recommendations, *rec)
				result.workloadsWithRecs++
			}
			mu.Unlock()

			return nil
		})
	}

	// Workers never return errors (they log and continue), but Wait()
	// propagates context cancellation.
	_ = g.Wait()

	// Build gauge keys from recommendations.
	for _, rec := range result.recommendations {
		for _, c := range rec.Containers {
			result.gaugeKeys = append(result.gaugeKeys, gaugeKey{
				Namespace: policy.Namespace,
				Workload:  rec.Workload,
				Container: c.Name,
			})
		}
	}
	r.gaugeKeys.Store(policyKey, result.gaugeKeys)

	return result
}

// setReadyCondition sets the Ready status condition based on whether
// recommendations were generated and whether Prometheus queries failed.
func (r *RightSizePolicyReconciler) setReadyCondition(
	policy *rightsizev1alpha1.RightSizePolicy,
	workloadCount int,
	workloadsWithRecs int32,
	totalQueryErrors int,
	queryErrorTypes map[string]struct{},
	maxDataPoints int,
	promTimedOut bool,
	effectiveTimeout time.Duration,
) {
	blockedDataTypes := "CPU and/or memory"
	_, cpuFailed := queryErrorTypes["CPU"]
	_, memoryFailed := queryErrorTypes["memory"]
	switch {
	case cpuFailed && memoryFailed:
		blockedDataTypes = "CPU and memory"
	case cpuFailed:
		blockedDataTypes = "CPU"
	case memoryFailed:
		blockedDataTypes = "memory"
	}

	if workloadsWithRecs > 0 {
		message := fmt.Sprintf("Watching %d workloads, %d with recommendations", workloadCount, workloadsWithRecs)
		if totalQueryErrors > 0 {
			message = fmt.Sprintf("%s; Prometheus query errors (%d) prevented %s data collection for part of the recommendation set, check operator logs",
				message, totalQueryErrors, blockedDataTypes)
		}
		if promTimedOut {
			message += "; Prometheus query timeout exceeded, some workloads may have incomplete data"
		}
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonMonitoring,
			Message:            message,
			ObservedGeneration: policy.Generation,
		})
		return
	}

	minimumDP := r.getMinimumDataPoints(policy)
	reason := rightsizev1alpha1.ReasonInsufficientData
	remaining := int(minimumDP) - maxDataPoints
	if remaining < 0 {
		remaining = 0
	}
	eta := time.Duration(remaining) * r.getQueryStep(policy)
	message := fmt.Sprintf("Collecting data: %d/%d data points (%d%%), ~%s remaining",
		maxDataPoints, minimumDP,
		progressPercent(maxDataPoints, int(minimumDP)),
		eta.Truncate(time.Minute))
	if promTimedOut {
		reason = rightsizev1alpha1.ReasonPrometheusUnavailable
		message = fmt.Sprintf("Prometheus query timeout exceeded after %s; some workloads may not have been queried", effectiveTimeout)
	} else if totalQueryErrors > 0 {
		reason = rightsizev1alpha1.ReasonPrometheusUnavailable
		message = fmt.Sprintf("Prometheus query errors (%d) prevented %s data collection; check operator logs", totalQueryErrors, blockedDataTypes)
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               rightsizev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
}

// handleDeletion cleans up pod annotations and Prometheus gauges when a
// RightSizePolicy is being deleted. Only pods tagged with annotationPolicy
// matching this policy's name are cleaned. After cleanup, the finalizer is
// removed so Kubernetes can garbage-collect the resource.
//
//nolint:unparam // ctrl.Result is always zero but the signature matches controller-runtime convention
func (r *RightSizePolicyReconciler) handleDeletion(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(policy, finalizerName) {
		return ctrl.Result{}, nil
	}

	// List tracked pods in the policy's namespace.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(policy.Namespace),
		client.MatchingLabels{labelTracked: "true"},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing tracked pods for cleanup: %w", err)
	}

	var cleanupErrs []error
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Annotations[annotationPolicy] != policy.Name {
			continue
		}
		original := pod.DeepCopy()
		removeTrackingAnnotations(pod)
		delete(pod.Annotations, annotationStartupBoostAt)
		if err := r.Patch(ctx, pod, client.MergeFrom(original)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "Failed to clean pod annotations", "pod", pod.Name)
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleaning pod %s: %w", pod.Name, err))
			continue
		}
		logger.Info("Cleaned tracking annotations from pod", "pod", pod.Name)
	}
	if len(cleanupErrs) > 0 {
		return ctrl.Result{}, errors.Join(cleanupErrs...)
	}

	// Clean Prometheus gauge values this policy set.
	policyKey := policy.Namespace + "/" + policy.Name
	if prev, ok := r.gaugeKeys.LoadAndDelete(policyKey); ok {
		if keys, ok := prev.([]gaugeKey); ok {
			deleteGaugeKeys(keys)
		}
	}

	// Clean namespace-level savings gauges if this is the last policy in the namespace.
	var policyList rightsizev1alpha1.RightSizePolicyList
	if listErr := r.List(ctx, &policyList, client.InNamespace(policy.Namespace)); listErr == nil {
		// Count remaining policies (exclude the one being deleted).
		remaining := 0
		for i := range policyList.Items {
			if policyList.Items[i].Name != policy.Name {
				remaining++
			}
		}
		if remaining == 0 {
			operatormetrics.SavingsCPU.DeleteLabelValues(policy.Namespace)
			operatormetrics.SavingsMemory.DeleteLabelValues(policy.Namespace)
			operatormetrics.SavingsEstimatedMonthly.DeleteLabelValues(policy.Namespace)
		}
	}

	// Remove the finalizer to allow garbage collection.
	patch := client.MergeFrom(policy.DeepCopy())
	controllerutil.RemoveFinalizer(policy, finalizerName)
	if err := r.Patch(ctx, policy, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Completed deletion cleanup for policy", "policy", policy.Name)
	return ctrl.Result{}, nil
}

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

// specOrDeletePredicate filters out reconcile triggers caused by the
// controller's own status updates and annotation patches. Without this,
// every status update (updateStatusWithRetry) and metadata patch
// (markResizeTime, addFinalizer) produces an Update event that re-enqueues
// the policy, causing 2-3x reconcile amplification per cycle. Only spec
// changes (generation bump) and deletion (DeletionTimestamp set) trigger
// new reconciles. Timer-based requeues bypass predicates entirely.
type specOrDeletePredicate struct {
	predicate.Funcs
}

func (specOrDeletePredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	// Generation change means the spec was modified by a user/webhook.
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		return true
	}
	// DeletionTimestamp was just set: the object is being deleted and the
	// finalizer handler needs to run before Kubernetes can remove it.
	if !e.ObjectNew.GetDeletionTimestamp().IsZero() && e.ObjectOld.GetDeletionTimestamp().IsZero() {
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}, builder.WithPredicates(specOrDeletePredicate{})).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
