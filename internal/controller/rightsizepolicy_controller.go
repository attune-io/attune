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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
)

//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/status,verbs=get;update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizepolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=rightsize.io,resources=rightsizenamespacedefaults,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheuses,verbs=get;list
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update;delete
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
	nowFunc        atomic.Pointer[func() time.Time]
	collectors     sync.Map // map[string]*collectorEntry cache
	// gaugeKeys tracks which Prometheus gauge label combinations each policy
	// set on its last reconcile. On the next reconcile, only these specific
	// keys are deleted (not the entire namespace), preventing cross-policy
	// gauge interference. This map is in-memory only; after operator restart
	// it starts empty, so gauges from workloads that disappeared during the
	// restart persist until the owning policy's next reconcile refreshes them.
	gaugeKeys sync.Map // map[string][]gaugeKey
}

// gaugeKey identifies a specific gauge label combination set by a policy.
type gaugeKey struct {
	Namespace string
	Workload  string
	Container string
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

// collectorEntry wraps a MetricsCollector with a last-used timestamp
// for TTL-based eviction.
type collectorEntry struct {
	collector rsmetrics.MetricsCollector
	lastUsed  time.Time
}

// MetricsCollectorFactory creates MetricsCollector instances from a Prometheus address
// and optional collector options (headers, bearer token, TLS).
// This enables dependency injection for testing.
type MetricsCollectorFactory func(address string, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error)

const (
	// maxCollectors bounds the collector cache to prevent memory-based DoS
	// via address rotation in CRD specs.
	maxCollectors = 64
	// collectorTTL is how long an unused collector stays cached before eviction.
	collectorTTL = 10 * time.Minute
)

// getOrCreateCollector returns a cached collector for the given config,
// creating one if needed. The cache key includes the address, headers, and
// TLS settings so different configs get different collectors. The cache is
// bounded at maxCollectors entries.
func (r *RightSizePolicyReconciler) getOrCreateCollector(config *rightsizev1alpha1.PrometheusConfig, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
	cacheKey := collectorCacheKey(config, opts)
	now := r.now()

	if cached, ok := r.collectors.Load(cacheKey); ok {
		entry := cached.(*collectorEntry)
		r.collectors.Store(cacheKey, &collectorEntry{collector: entry.collector, lastUsed: now})
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
			if closer, ok := entry.collector.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		return true
	})

	var count int
	r.collectors.Range(func(_, _ any) bool {
		count++
		return count < maxCollectors
	})
	if count >= maxCollectors {
		return nil, fmt.Errorf("collector cache full (%d entries); refusing new Prometheus address %q", maxCollectors, config.Address)
	}

	collector, err := r.MetricsFactory(config.Address, opts)
	if err != nil {
		return nil, err
	}
	entry := &collectorEntry{collector: collector, lastUsed: now}
	actual, loaded := r.collectors.LoadOrStore(cacheKey, entry)
	if loaded {
		// Another goroutine won the race; close our unused collector's transport.
		if closer, ok := collector.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return actual.(*collectorEntry).collector, nil
}

func collectorConfigPrefix(address string, headers map[string]string, tlsConfig *rightsizev1alpha1.TLSConfig) string {
	key := address
	if tlsConfig != nil && tlsConfig.InsecureSkipVerify {
		key += "|insecure"
	}
	// Sort header keys for deterministic cache keys (map iteration is random).
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		sum := sha256.Sum256([]byte(headers[k]))
		key += fmt.Sprintf("|h:%s=%x", k, sum[:8])
	}
	return key
}

// collectorCacheKey builds a cache key that includes address, headers,
// bearer token identity, and TLS settings.
func collectorCacheKey(config *rightsizev1alpha1.PrometheusConfig, opts *rsmetrics.CollectorOptions) string {
	headers := map[string]string(nil)
	var tlsConfig *rightsizev1alpha1.TLSConfig
	if opts != nil {
		headers = opts.Headers
		if opts.InsecureSkipVerify {
			tlsConfig = &rightsizev1alpha1.TLSConfig{InsecureSkipVerify: true}
		}
	}
	key := collectorConfigPrefix(config.Address, headers, tlsConfig)
	if opts != nil && opts.BearerToken != "" {
		sum := sha256.Sum256([]byte(opts.BearerToken))
		key += fmt.Sprintf("|bearer:%x", sum[:8])
	}
	if opts != nil && len(opts.QueryParameters) > 0 {
		sortedKeys := make([]string, 0, len(opts.QueryParameters))
		for k := range opts.QueryParameters {
			sortedKeys = append(sortedKeys, k)
		}
		slices.Sort(sortedKeys)
		for _, k := range sortedKeys {
			key += fmt.Sprintf("|qp:%s=%s", k, opts.QueryParameters[k])
		}
	}
	return key
}

// Reconcile is the main reconciliation loop for RightSizePolicy resources.
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
				deleteGaugeKeys(prev.([]gaugeKey))
			}
			return ctrl.Result{}, nil
		}
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("fetch").Inc()
		return ctrl.Result{}, fmt.Errorf("fetching RightSizePolicy: %w", err)
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
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable,
			fmt.Sprintf("Cannot resolve Prometheus config: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Build collector options from the PrometheusConfig.
	collectorOpts, err := r.buildCollectorOptions(ctx, policy.Namespace, promConfig)
	if err != nil {
		logger.Error(err, "Failed to build collector options")
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonPrometheusUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	collector, err := r.getOrCreateCollector(promConfig, collectorOpts)
	if err != nil {
		logger.Error(err, "Failed to create metrics collector", "address", promConfig.Address)
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
	if autoRevertEnabled(policy.Spec.UpdateStrategy) {
		r.checkPendingSafetyObservations(ctx, &policy, collector, workloads)
	}

	logger.Info("Discovered workloads", "count", len(workloads))

	if len(workloads) == 0 {
		earlyNow := metav1.NewTime(r.now())
		policy.Status.LastReconcileTime = &earlyNow
		policy.Status.Workloads = rightsizev1alpha1.WorkloadStatus{}
		r.setFailedCondition(ctx, &policy, rightsizev1alpha1.ReasonInsufficientData, "No matching workloads found")
		return ctrl.Result{RequeueAfter: r.parseCooldown(&policy)}, nil
	}

	// Step 4-8: Process each workload.
	var recommendations []rightsizev1alpha1.WorkloadRecommendation
	var workloadsWithRecs int32
	var globalMaxDataPoints int
	var totalQueryErrors int
	queryErrorTypes := make(map[string]struct{})
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

	// Clear gauge values that THIS policy previously set. Using per-policy
	// tracking instead of namespace-wide DeletePartialMatch avoids wiping
	// gauges belonging to other policies in the same namespace.
	policyKey := policy.Namespace + "/" + policy.Name
	if prev, ok := r.gaugeKeys.Load(policyKey); ok {
		deleteGaugeKeys(prev.([]gaugeKey))
	}
	var currentGaugeKeys []gaugeKey

	// Build recommendation engines and exclude set once; they depend only
	// on the policy spec, not the workload.
	cpuEngine, memEngine := buildRecommendationEngines(&policy)
	excludeSet := make(map[string]bool, len(policy.Spec.ExcludeContainers))
	for _, name := range policy.Spec.ExcludeContainers {
		excludeSet[name] = true
	}

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
		if policyConflict := conflictDetector.CheckPolicyConflictInMemory(policyList, workloadName, workloadKind, workload.GetLabels(), policy.Name, policy.Spec.Weight); policyConflict != nil {
			logger.Info("Higher-weight policy exists, skipping workload", "workload", workloadName, "policy", policyConflict.Name, "message", policyConflict.Message)
			continue
		}

		// Step 6: Check for active rollout.
		if r.isRollingOut(workload) {
			logger.Info("Skipping workload mid-rollout", "workload", workloadName)
			continue
		}

		// Step 4: Compute recommendations from historical metrics.
		rec, qErrors, failedMetricTypes, dataPoints, err := r.computeRecommendations(ctx, &policy, workload, collector, cpuEngine, memEngine, excludeSet)
		totalQueryErrors += qErrors
		for _, metricType := range failedMetricTypes {
			queryErrorTypes[metricType] = struct{}{}
		}
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

	// Build gauge keys from recommendations so the next reconcile can clean
	// up only its own stale entries without affecting other policies.
	for _, rec := range recommendations {
		for _, c := range rec.Containers {
			currentGaugeKeys = append(currentGaugeKeys, gaugeKey{
				Namespace: policy.Namespace,
				Workload:  rec.Workload,
				Container: c.Name,
			})
		}
	}
	r.gaugeKeys.Store(policyKey, currentGaugeKeys)

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
	if policy.Spec.UpdateStrategy.Mode != rightsizev1alpha1.UpdateModeObserve {
		policy.Status.Recommendations = recommendations
		policy.Status.Savings = r.computeSavings(policy.Namespace, recommendations, defaults)
	}

	// Export recommendations to ConfigMaps for GitOps workflows if configured.
	if policy.Spec.UpdateStrategy.Export != nil && policy.Spec.UpdateStrategy.Export.ConfigMap && len(recommendations) > 0 {
		r.exportRecommendationConfigMaps(ctx, &policy, recommendations)
	}

	// Step 9: Execute resizes if mode allows.
	mode := policy.Spec.UpdateStrategy.Mode
	cooldownActive := r.isCooldownActive(&policy)
	withinWindow := isWithinResizeWindow(policy.Spec.UpdateStrategy.Schedule, r.now())
	resizesAttempted := false

	// Build podsByWorkload once so both executeResizes and applyStartupBoosts
	// can reuse it, avoiding duplicate getPodsForWorkload calls per cycle.
	var podsByWorkload map[string][]corev1.Pod
	needPods := isResizeMode(mode) && ((!cooldownActive && withinWindow) ||
		(policy.Spec.CPU.StartupBoost != nil && r.Clientset != nil && len(recommendations) > 0))
	if needPods {
		podsByWorkload = make(map[string][]corev1.Pod, len(workloads))
		for _, w := range workloads {
			pods, err := r.getPodsForWorkload(ctx, w)
			if err != nil {
				logger.Error(err, "Failed to get pods for workload", "workload", w.GetName())
				continue
			}
			podsByWorkload[w.GetName()] = pods
		}
	}

	if isResizeMode(mode) && !cooldownActive && withinWindow {
		resizesAttempted = true
		resizedCount, history := r.executeResizes(ctx, &policy, workloads, recommendations, podsByWorkload, collector)
		if resizedCount > 0 {
			policy.Status.Workloads.Resized = safeInt32(resizedCount)
			policy.Status.ResizeHistory = appendHistory(policy.Status.ResizeHistory, history, 20)
			// Auto-tune HPA targets only for workloads that were actually
			// resized, using aggregate CPU across all containers.
			resizedWorkloads := make(map[string]bool, len(history))
			for _, h := range history {
				if h.Result == rightsizev1alpha1.ResizeResultSuccess {
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
		r.applyStartupBoosts(ctx, &policy, podsByWorkload, recommendations, resizer)
	}

	if isResizeMode(mode) && !withinWindow {
		logger.Info("Outside resize window, skipping resize")
		operatormetrics.ScheduleSkippedTotal.WithLabelValues(policy.Namespace, policy.Name).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(&policy, nil, corev1.EventTypeNormal, "ScheduleSkipped", "resize",
				"Resize deferred: outside configured schedule window")
		}
	}

	// Preserve the Resized count from a concurrent reconcile that may have
	// already updated the status. Without this, a stale snapshot from this
	// reconcile overwrites the count to 0. Only re-fetch when executeResizes
	// was actually called (not when cooldown or schedule skipped the resize).
	if resizesAttempted && policy.Status.Workloads.Resized == 0 {
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
		message := fmt.Sprintf("Watching %d workloads, %d with recommendations", len(workloads), workloadsWithRecs)
		if totalQueryErrors > 0 {
			message = fmt.Sprintf("%s; Prometheus query errors (%d) prevented %s data collection for part of the recommendation set, check operator logs",
				message, totalQueryErrors, blockedDataTypes)
		}
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               rightsizev1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             rightsizev1alpha1.ReasonMonitoring,
			Message:            message,
			ObservedGeneration: policy.Generation,
		})
	} else {
		reason := rightsizev1alpha1.ReasonInsufficientData
		remaining := int(minimumDP) - globalMaxDataPoints
		if remaining < 0 {
			remaining = 0
		}
		eta := time.Duration(remaining) * r.getQueryStep(&policy)
		message := fmt.Sprintf("Collecting data: %d/%d data points (%d%%), ~%s remaining",
			globalMaxDataPoints, minimumDP,
			progressPercent(globalMaxDataPoints, int(minimumDP)),
			eta.Truncate(time.Minute))
		if totalQueryErrors > 0 {
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
	if autoRevertEnabled(policy.Spec.UpdateStrategy) && policy.Status.Workloads.Resized > 0 {
		obs := getObservationPeriod(&policy)
		if obs < requeueAfter {
			requeueAfter = obs
		}
	}
	logger.Info("Reconciliation complete, requeueing", "requeueAfter", requeueAfter)
	operatormetrics.ReconcileDuration.WithLabelValues("rightsizepolicy").Observe(time.Since(startTime).Seconds())
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// handleDeletion cleans up pod annotations and Prometheus gauges when a
// RightSizePolicy is being deleted. Only pods tagged with annotationPolicy
// matching this policy's name are cleaned. After cleanup, the finalizer is
// removed so Kubernetes can garbage-collect the resource.
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
		removeTrackingAnnotations(pod)
		delete(pod.Annotations, annotationStartupBoostAt)
		if err := r.Update(ctx, pod); err != nil {
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
		deleteGaugeKeys(prev.([]gaugeKey))
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

// computeRecommendations generates resource recommendations for all containers
// in a workload based on Prometheus metrics.
//
//nolint:unparam // error return is part of the interface contract for future use
func (r *RightSizePolicyReconciler) computeRecommendations(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	workload client.Object,
	collector rsmetrics.MetricsCollector,
	cpuEngine, memEngine *recommendation.RecommendationEngine,
	excludeSet map[string]bool,
) (rec *rightsizev1alpha1.WorkloadRecommendation, queryErrors int, failedMetricTypes []string, maxDataPoints int, err error) {
	logger := log.FromContext(ctx)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, 0, nil, 0, nil
	}

	// Fallback: build engines if not pre-built (used in tests).
	if cpuEngine == nil || memEngine == nil {
		cpuEngine, memEngine = buildRecommendationEngines(policy)
	}
	if excludeSet == nil {
		excludeSet = make(map[string]bool, len(policy.Spec.ExcludeContainers))
		for _, name := range policy.Spec.ExcludeContainers {
			excludeSet[name] = true
		}
	}

	historyWindow := r.parseHistoryWindow(policy)
	minimumDataPoints := r.getMinimumDataPoints(policy)

	now := r.now()
	start := now.Add(-historyWindow)
	podPrefix := r.getPodPrefix(workload)

	queryStep := r.getQueryStep(policy)
	if queryStep != defaultPrometheusStep {
		logger.V(1).Info("Using custom query step", "queryStep", queryStep)
	}
	cpuSamplesByContainer, cpuErr := queryMetricsGrouped(ctx, collector, policy.Namespace, podPrefix, "cpu", start, now, queryStep)
	memSamplesByContainer, memErr := queryMetricsGrouped(ctx, collector, policy.Namespace, podPrefix, "memory", start, now, queryStep)
	if cpuErr {
		queryErrors++
		failedMetricTypes = append(failedMetricTypes, "CPU")
	}
	if memErr {
		queryErrors++
		failedMetricTypes = append(failedMetricTypes, "memory")
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

		// V(1): log per-container recommendation summary.
		cpuChanged := !rec.Recommended.CPURequest.Equal(rec.Current.CPURequest)
		memChanged := !rec.Recommended.MemoryRequest.Equal(rec.Current.MemoryRequest)
		cpuChangeFilter, memChangeFilter := "", ""
		if explanation.CPU != nil {
			cpuChangeFilter = explanation.CPU.ChangeFilterApplied
		}
		if explanation.Memory != nil {
			memChangeFilter = explanation.Memory.ChangeFilterApplied
		}
		logger.V(1).Info("Computed recommendation",
			"container", containerName,
			"cpuCurrent", &rec.Current.CPURequest,
			"cpuRecommended", &rec.Recommended.CPURequest,
			"cpuChanged", cpuChanged,
			"cpuChangeFilter", cpuChangeFilter,
			"memCurrent", &rec.Current.MemoryRequest,
			"memRecommended", &rec.Recommended.MemoryRequest,
			"memChanged", memChanged,
			"memChangeFilter", memChangeFilter,
			"confidence", rec.Confidence)

		// V(2): log full recommendation chain if explanation is available.
		if explanation.CPU != nil {
			logger.V(2).Info("CPU recommendation chain",
				"container", containerName,
				"rawPercentile", &explanation.CPU.RawPercentile,
				"afterMargin", &explanation.CPU.AfterSafetyMargin,
				"burstFactor", explanation.CPU.BurstFactor,
				"afterConfidence", &explanation.CPU.AfterConfidence,
				"boundsApplied", explanation.CPU.BoundsApplied,
				"changeFilter", explanation.CPU.ChangeFilterApplied,
				"final", &explanation.CPU.Final)
		}
		if explanation.Memory != nil {
			logger.V(2).Info("Memory recommendation chain",
				"container", containerName,
				"rawPercentile", &explanation.Memory.RawPercentile,
				"afterMargin", &explanation.Memory.AfterSafetyMargin,
				"burstFactor", explanation.Memory.BurstFactor,
				"afterConfidence", &explanation.Memory.AfterConfidence,
				"boundsApplied", explanation.Memory.BoundsApplied,
				"changeFilter", explanation.Memory.ChangeFilterApplied,
				"final", &explanation.Memory.Final)
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
		if rec.Explanation != nil {
			if rec.Explanation.CPU != nil {
				operatormetrics.BurstFactor.WithLabelValues(policy.Namespace, workload.GetName(), containerName, "cpu").Set(rec.Explanation.CPU.BurstFactor)
			}
			if rec.Explanation.Memory != nil {
				operatormetrics.BurstFactor.WithLabelValues(policy.Namespace, workload.GetName(), containerName, "memory").Set(rec.Explanation.Memory.BurstFactor)
			}
		}

		containerRecs = append(containerRecs, rec)
	}

	if len(containerRecs) == 0 {
		return nil, queryErrors, failedMetricTypes, maxDataPoints, nil
	}

	return &rightsizev1alpha1.WorkloadRecommendation{
		Containers: containerRecs,
	}, queryErrors, failedMetricTypes, maxDataPoints, nil
}

// buildCollectorOptions constructs CollectorOptions from the given PrometheusConfig,
// including headers, TLS settings, and Secret-backed bearer token resolution.
func (r *RightSizePolicyReconciler) buildCollectorOptions(ctx context.Context, namespace string, config *rightsizev1alpha1.PrometheusConfig) (*rsmetrics.CollectorOptions, error) {
	if config.Headers == nil && config.QueryParameters == nil && config.BearerTokenSecret == nil &&
		(config.TLS == nil || !config.TLS.InsecureSkipVerify) {
		return nil, nil
	}

	opts := &rsmetrics.CollectorOptions{
		Headers:         config.Headers,
		QueryParameters: config.QueryParameters,
	}
	if config.TLS != nil {
		opts.InsecureSkipVerify = config.TLS.InsecureSkipVerify
	}
	if config.BearerTokenSecret != nil {
		secretName := config.BearerTokenSecret.Name
		secretKey := config.BearerTokenSecret.Key
		token, err := r.readSecretKey(ctx, namespace, secretName, secretKey)
		if err != nil {
			return nil, fmt.Errorf("cannot read bearer token secret %s/%s: %w", secretName, secretKey, err)
		}
		opts.BearerToken = token
	}
	return opts, nil
}

// resolvePrometheusAddress returns the Prometheus address from the policy spec,
// falling back to the cluster-scoped RightSizeDefaults if not set.
func (r *RightSizePolicyReconciler) resolvePrometheusConfig(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) (*rightsizev1alpha1.PrometheusConfig, error) {
	// Check policy-level config first.
	if policy.Spec.MetricsSource.Prometheus != nil &&
		policy.Spec.MetricsSource.Prometheus.Address != "" {
		config := policy.Spec.MetricsSource.Prometheus.DeepCopy()
		if err := validation.PrometheusAddress(config.Address); err != nil {
			return nil, fmt.Errorf("SSRF blocked: %w", err)
		}
		return config, nil
	}

	// Fall back to RightSizeDefaults.
	if defaults != nil &&
		defaults.Spec.MetricsSource != nil &&
		defaults.Spec.MetricsSource.Prometheus != nil &&
		defaults.Spec.MetricsSource.Prometheus.Address != "" {
		config := defaults.Spec.MetricsSource.Prometheus.DeepCopy()
		if err := validation.PrometheusAddress(config.Address); err != nil {
			return nil, fmt.Errorf("SSRF blocked: %w", err)
		}
		return config, nil
	}

	// Fall back to auto-discovery: look for Prometheus Operator's Prometheus CRD.
	if discovered := r.discoverPrometheus(ctx); discovered != "" {
		if err := validation.PrometheusAddress(discovered); err != nil {
			log.FromContext(ctx).Error(err, "Auto-discovered Prometheus address failed SSRF validation", "address", discovered)
		} else {
			log.FromContext(ctx).Info("Auto-discovered Prometheus address", "address", discovered)
			return &rightsizev1alpha1.PrometheusConfig{Address: discovered}, nil
		}
	}
	return nil, fmt.Errorf("no Prometheus address configured in policy or cluster defaults, and auto-discovery found no Prometheus instance")
}

// readSecretKey reads a single key from a Kubernetes Secret.
func (r *RightSizePolicyReconciler) readSecretKey(ctx context.Context, namespace, name, key string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", namespace, name, err)
	}
	data, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", key, namespace, name)
	}
	return string(data), nil
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
func selectPodsForResize(pods []corev1.Pod, mode rightsizev1alpha1.UpdateMode, canaryPercentage int32) []corev1.Pod {
	var eligible []corev1.Pod
	for _, p := range pods {
		if resize.IsEligibleForResize(&p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	switch mode {
	case rightsizev1alpha1.UpdateModeOneShot:
		return eligible[:1]
	case rightsizev1alpha1.UpdateModeCanary:
		count := int(canaryPercentage) * len(eligible) / 100
		if count < 1 {
			count = 1
		}
		if count > len(eligible) {
			count = len(eligible)
		}
		return eligible[:count]
	case rightsizev1alpha1.UpdateModeAuto:
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
	canaryAutoPromote := false
	if policy.Spec.UpdateStrategy.Canary != nil {
		canaryPct = policy.Spec.UpdateStrategy.Canary.Percentage
		canaryAutoPromote = policy.Spec.UpdateStrategy.Canary.AutoPromote
	}

	// Canary auto-promotion: if all canary pods passed the observation
	// period without reverts, promote to full rollout.
	if mode == rightsizev1alpha1.UpdateModeCanary && canaryAutoPromote {
		mode = r.resolveCanaryPhase(ctx, policy, mode)
	}

	resizer := resize.NewPodResizer(r.Clientset, logger)
	monitor := r.newSafetyMonitor(logger, collector)

	// Pre-fetch namespace-scoped resources once to avoid redundant API
	// calls across all pods during resize pre-checks.
	var limitRanges corev1.LimitRangeList
	if err := r.List(ctx, &limitRanges, client.InNamespace(policy.Namespace)); err != nil {
		logger.V(1).Info("Could not pre-fetch LimitRanges", "error", err)
	}
	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(policy.Namespace)); err != nil {
		logger.V(1).Info("Could not pre-fetch ResourceQuotas", "error", err)
	}
	checks := &resizePreChecks{
		limitRanges: limitRanges.Items,
		quotas:      quotas.Items,
	}

	var totalResized int
	var history []rightsizev1alpha1.ResizeHistoryEntry
	now := metav1.NewTime(r.now())

	// Per-cycle budget caps. Protected by budgetMu for concurrent access.
	var budgetMu sync.Mutex
	cpuBudget := int64(-1)
	memBudget := int64(-1)
	if policy.Spec.UpdateStrategy.MaxTotalCPUIncrease != nil {
		cpuBudget = policy.Spec.UpdateStrategy.MaxTotalCPUIncrease.MilliValue()
	}
	if policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease != nil {
		memBudget = policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease.Value()
	}

	// Concurrency control: semaphore limits parallel resize calls.
	concurrency := int(policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	var historyMu sync.Mutex
	var wg sync.WaitGroup

	// Pre-build name→Object map for O(1) workload lookups.
	workloadMap := make(map[string]client.Object, len(workloads))
	for _, w := range workloads {
		workloadMap[w.GetName()] = w
	}

	for _, rec := range recommendations {
		if ctx.Err() != nil {
			logger.Info("Context cancelled, aborting remaining resizes")
			break
		}
		matchedWorkload := workloadMap[rec.Workload]
		if matchedWorkload == nil {
			continue
		}

		// Batch workloads (Job/CronJob) are recommend-only; skip resize.
		if isBatchWorkload(matchedWorkload) {
			continue
		}

		pods := podsByWorkload[rec.Workload]
		if pods == nil {
			var err error
			pods, err = r.getPodsForWorkload(ctx, matchedWorkload)
			if err != nil {
				logger.Error(err, "Failed to get pods for workload", "workload", rec.Workload)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("get_pods").Inc()
				continue
			}
		}
		if len(pods) == 0 {
			logger.Info("No pods found for workload", "workload", rec.Workload)
			continue
		}
		selectedPods := selectPodsForResize(pods, mode, canaryPct)
		logger.V(1).Info("Pod selection for resize",
			"workload", rec.Workload, "total", len(pods),
			"selected", len(selectedPods), "mode", mode)
		if len(selectedPods) == 0 {
			continue
		}

		var workloadResized int32 // atomic for concurrent access
		for _, pod := range selectedPods {
			// Capture loop variables for the goroutine.
			pod, workloadName := pod, rec.Workload

			wg.Add(1)
			sem <- struct{}{} // acquire semaphore
			go func() {
				defer wg.Done()
				defer func() { <-sem }() // release semaphore

				// Containers within the same pod must resize sequentially.
				// Each UpdateResize bumps resourceVersion; using a stale copy
				// for the next container causes a 409 Conflict.
				for _, containerRec := range rec.Containers {
					// Check per-cycle budget caps before resizing (under lock).
					budgetMu.Lock()
					if cpuBudget >= 0 || memBudget >= 0 {
						cpuIncrease := containerRec.Recommended.CPURequest.MilliValue() - containerRec.Current.CPURequest.MilliValue()
						memIncrease := containerRec.Recommended.MemoryRequest.Value() - containerRec.Current.MemoryRequest.Value()
						if cpuIncrease < 0 {
							cpuIncrease = 0
						}
						if memIncrease < 0 {
							memIncrease = 0
						}
						if (cpuBudget >= 0 && cpuIncrease > cpuBudget) ||
							(memBudget >= 0 && memIncrease > memBudget) {
							budgetMu.Unlock()
							logger.Info("Budget exhausted, deferring resize to next cycle",
								"pod", pod.Name, "container", containerRec.Name)
							operatormetrics.BudgetExhaustedTotal.WithLabelValues(policy.Namespace, policy.Name).Inc()
							if r.Recorder != nil {
								r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "BudgetExhausted", "resize",
									"Resize deferred for pod %s container %s: per-cycle budget exhausted",
									pod.Name, containerRec.Name)
							}
							continue
						}
						cpuBudget -= cpuIncrease
						memBudget -= memIncrease
					}
					budgetMu.Unlock()

					entries, resized := r.resizeContainer(ctx, resizeParams{
						Policy:       policy,
						Pod:          &pod,
						Workload:     matchedWorkload,
						WorkloadName: workloadName,
						ContainerRec: containerRec,
						Resizer:      resizer,
						Monitor:      monitor,
						Now:          now,
						Checks:       checks,
					})
					historyMu.Lock()
					history = append(history, entries...)
					historyMu.Unlock()
					if resized {
						atomic.AddInt32(&workloadResized, 1)
						// Re-fetch pod from API server to get fresh resourceVersion
						// for the next container's UpdateResize call.
						freshPod, err := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
						if err == nil {
							pod = *freshPod
						} else {
							logger.Error(err, "Failed to re-fetch pod after container resize, remaining containers will be deferred",
								"pod", pod.Name)
							break
						}
					}
				}
			}()
		}
		wg.Wait() // wait for all pods in this workload before moving to the next
		if atomic.LoadInt32(&workloadResized) > 0 {
			totalResized++
		}
	}

	return totalResized, history
}

// resizeContainer performs a single container resize on a pod, including
// skip checks, the resize call, annotation persistence, and safety checks.
// Returns the history entries produced and whether the resize counted as successful.
// resizeParams groups parameters for resizeContainer, reducing the function
// signature from 9 parameters to 2 (ctx + params).
type resizeParams struct {
	Policy       *rightsizev1alpha1.RightSizePolicy
	Pod          *corev1.Pod
	Workload     client.Object
	WorkloadName string
	ContainerRec rightsizev1alpha1.ContainerRecommendation
	Resizer      *resize.PodResizer
	Monitor      *safety.Monitor
	Now          metav1.Time
	Checks       *resizePreChecks
}

func (r *RightSizePolicyReconciler) resizeContainer(
	ctx context.Context,
	p resizeParams,
) ([]rightsizev1alpha1.ResizeHistoryEntry, bool) {
	logger := log.FromContext(ctx)
	policy, pod, workload, workloadName := p.Policy, p.Pod, p.Workload, p.WorkloadName
	containerRec, resizer, monitor, now := p.ContainerRec, p.Resizer, p.Monitor, p.Now

	target := buildResizeTarget(containerRec)

	skip, reason := r.shouldSkipResize(ctx, policy, pod, containerRec, target, p.Checks)
	if skip {
		if reason != "" {
			logger.Info("Skipping resize: "+reason,
				"pod", pod.Name, "container", containerRec.Name)
		}
		return nil, false
	}

	evictionHistory := func() []rightsizev1alpha1.ResizeHistoryEntry {
		return []rightsizev1alpha1.ResizeHistoryEntry{
			{
				Timestamp: now, Workload: workloadName, Container: containerRec.Name,
				Resource: "cpu+memory", Method: "Eviction", Result: rightsizev1alpha1.ResizeResultSuccess,
			},
		}
	}

	// Pods already marked Infeasible cannot be resized in-place on the current node.
	if resize.IsResizeInfeasible(pod) {
		if policy.Spec.UpdateStrategy.ResizeMethod == rightsizev1alpha1.ResizeMethodInPlaceOrEvict {
			logger.Info("Pod resize is Infeasible, attempting eviction fallback",
				"pod", pod.Name, "container", containerRec.Name)
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), true
			}
		} else {
			logger.Info("Pod resize is Infeasible and resizeMethod is InPlaceOnly, skipping",
				"pod", pod.Name, "container", containerRec.Name)
			operatormetrics.InfeasibleSkippedTotal.WithLabelValues(pod.Namespace, workloadName).Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, "InfeasibleBlocked", "resize",
					"Pod %s cannot be resized in-place (Infeasible) and resizeMethod is InPlaceOnly; consider InPlaceOrEvict",
					pod.Name)
			}
		}
		return nil, false
	}

	if resize.WouldRestartContainer(pod, containerRec.Name) {
		logger.Info("Container has RestartContainer resize policy; resize will trigger restart",
			"pod", pod.Name, "container", containerRec.Name)
	}

	resizeStart := r.now()
	results, err := resizer.ResizePod(ctx, pod, containerRec.Name, target)
	if err != nil {
		// Attempt eviction fallback if configured.
		if policy.Spec.UpdateStrategy.ResizeMethod == rightsizev1alpha1.ResizeMethodInPlaceOrEvict {
			if evicted := r.tryEvictionFallback(ctx, policy, pod, workload, workloadName, containerRec.Name, resizer); evicted {
				return evictionHistory(), true
			}
		}

		logger.Error(err, "Failed to resize pod",
			"pod", pod.Name, "container", containerRec.Name)
		var entries []rightsizev1alpha1.ResizeHistoryEntry
		for _, res := range results {
			entries = append(entries, newHistoryEntry(now, workloadName, containerRec.Name, res, rightsizev1alpha1.ResizeResultFailed))
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
		result := rightsizev1alpha1.ResizeResultSuccess
		if !res.Success {
			result = rightsizev1alpha1.ResizeResultFailed
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
	if !containerRec.Current.CPULimit.IsZero() || !containerRec.Current.MemoryLimit.IsZero() {
		originalResources.Limits = make(corev1.ResourceList)
		if !containerRec.Current.CPULimit.IsZero() {
			originalResources.Limits[corev1.ResourceCPU] = containerRec.Current.CPULimit.DeepCopy()
		}
		if !containerRec.Current.MemoryLimit.IsZero() {
			originalResources.Limits[corev1.ResourceMemory] = containerRec.Current.MemoryLimit.DeepCopy()
		}
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
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(rightsizev1alpha1.ResizeResultReverted), "revert",
				"Reverted resize on %s/%s: %s", workloadName, containerRec.Name, reason)
		}
		for i := range history {
			if history[i].Workload == workloadName && history[i].Container == containerRec.Name {
				history[i].Result = rightsizev1alpha1.ResizeResultReverted
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
	freshPod.Annotations[annotationPolicy] = policy.Name
	appendResizedContainer(freshPod, containerRec.Name)
	freshPod.Annotations[annotationOriginalCPUPrefix+containerRec.Name] = containerRec.Current.CPURequest.String()
	freshPod.Annotations[annotationOriginalMemoryPrefix+containerRec.Name] = containerRec.Current.MemoryRequest.String()
	if !containerRec.Current.CPULimit.IsZero() {
		freshPod.Annotations[annotationOriginalCPULimitPrefix+containerRec.Name] = containerRec.Current.CPULimit.String()
	}
	if !containerRec.Current.MemoryLimit.IsZero() {
		freshPod.Annotations[annotationOriginalMemoryLimitPrefix+containerRec.Name] = containerRec.Current.MemoryLimit.String()
	}
	freshPod.Annotations[annotationOriginalRestartCountPrefix+containerRec.Name] = strconv.FormatInt(int64(restartCount), 10)

	if updateErr := r.Update(ctx, freshPod); updateErr != nil {
		logger.Error(updateErr, "Failed to persist resize tracking annotations, reverting resize", "pod", pod.Name)
		revert("annotation-persist-failed")
		return history, false
	}

	if autoRevertEnabled(policy.Spec.UpdateStrategy) {
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
		verdict, err := monitor.CheckPod(ctx, record, now.Time)
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
	workload client.Object,
	workloadName, containerName string,
	resizer *resize.PodResizer,
) bool {
	logger := log.FromContext(ctx)

	// Safety: never evict the last replica. Count running pods for this workload.
	selectorLabels := r.getPodSelectorLabels(workload)
	if len(selectorLabels) == 0 {
		logger.Info("Skipping eviction fallback: workload has no pod selector labels",
			"pod", pod.Name, "workload", workloadName)
		return false
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(pod.Namespace),
		client.MatchingLabels(selectorLabels),
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
		operatormetrics.EvictionTotal.WithLabelValues(pod.Namespace, workloadName, "denied").Inc()
		return false
	}

	operatormetrics.EvictionTotal.WithLabelValues(pod.Namespace, workloadName, "success").Inc()
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

// toAPIRecommendationExplanation converts an internal explanation to the API
// type. The explanation parameter is passed by value (already a copy) and is
// not referenced after this call, so quantities are assigned directly without
// redundant DeepCopy.
func toAPIRecommendationExplanation(explanation recommendation.RecommendationExplanation) *rightsizev1alpha1.ResourceRecommendationExplanation {
	return &rightsizev1alpha1.ResourceRecommendationExplanation{
		RawPercentile:     explanation.RawPercentile,
		SafetyMargin:      explanation.SafetyMargin,
		AfterSafetyMargin: explanation.AfterSafetyMargin,
		BurstFactor:       explanation.BurstFactor,
		AfterBurst:        explanation.AfterBurst,
		Confidence:        explanation.Confidence,
		ConfidenceFactor:  explanation.ConfidenceFactor,
		AfterConfidence:   explanation.AfterConfidence,
		Bounds: rightsizev1alpha1.ResourceBounds{
			Min: explanation.MinBound,
			Max: explanation.MaxBound,
		},
		BoundsApplied:       explanation.BoundsApplied,
		AfterBounds:         explanation.AfterBounds,
		MinChangePercent:    explanation.MinChangePercent,
		MaxChangePercent:    explanation.MaxChangePercent,
		ChangeFilterApplied: explanation.ChangeFilterApplied,
		AfterChangeFilter:   explanation.AfterChangeFilter,
		Final:               explanation.Final,
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
	maxCPUPct := rightsizev1alpha1.DefaultMaxCPUChangePercent
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent != nil {
		maxCPUPct = *policy.Spec.UpdateStrategy.MaxCPUChangePercent
	}
	maxMemPct := rightsizev1alpha1.DefaultMaxMemoryChangePercent
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent != nil {
		maxMemPct = *policy.Spec.UpdateStrategy.MaxMemoryChangePercent
	}
	maxCPUChange := min(max(float64(maxCPUPct), 1), 100)
	maxMemChange := min(max(float64(maxMemPct), 1), 100)

	// Parse per-resource burst sensitivity; nil means default (0.1).
	cpuOpts := recommendation.EngineOpts{IsCPU: true}
	if policy.Spec.CPU.BurstSensitivity != nil {
		bs := parseFloat64NonNeg(*policy.Spec.CPU.BurstSensitivity, recommendation.DefaultBurstSensitivity)
		cpuOpts.BurstSensitivity = &bs
	}
	memOpts := recommendation.EngineOpts{}
	if policy.Spec.Memory.BurstSensitivity != nil {
		bs := parseFloat64NonNeg(*policy.Spec.Memory.BurstSensitivity, recommendation.DefaultBurstSensitivity)
		memOpts.BurstSensitivity = &bs
	}

	cpuEngine = recommendation.NewEngine(cpuPercentile, cpuSafetyMargin, cpuBoundsMin, cpuBoundsMax, maxCPUChange, cpuOpts)
	memEngine = recommendation.NewEngine(memPercentile, memSafetyMargin, memBoundsMin, memBoundsMax, maxMemChange, memOpts)
	return cpuEngine, memEngine
}

// buildResizeTarget constructs the target ResourceRequirements from a container recommendation.
// Limits are included when non-zero: for RequestsOnly they equal the current limits (no-op),
// for RequestsAndLimits they are scaled proportionally. Pods that never had limits produce
// zero-valued limit fields, which are omitted to avoid Kubernetes rejecting the resize.
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
	// Clamp requests to not exceed limits. When ControlledValues is
	// RequestsOnly, limits stay at current values and a growing request
	// can exceed them, causing the API server to reject the resize.
	clampRequestsToLimits(&target)
	return target
}

// clampRequestsToLimits ensures requests do not exceed limits for each resource.
// When a limit is present and the request exceeds it, the request is capped
// at the limit value to prevent API server rejection.
func clampRequestsToLimits(target *corev1.ResourceRequirements) {
	if target.Limits == nil {
		return
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		lim, hasLim := target.Limits[res]
		req, hasReq := target.Requests[res]
		if hasLim && hasReq && req.Cmp(lim) > 0 {
			target.Requests[res] = lim.DeepCopy()
		}
	}
}

// resolveCanaryPhase checks whether canary pods have passed the observation
// period without reverts. If so, it promotes to FullRollout and returns
// ModeAuto so selectPodsForResize resizes all pods.
func (r *RightSizePolicyReconciler) resolveCanaryPhase(ctx context.Context, policy *rightsizev1alpha1.RightSizePolicy, currentMode rightsizev1alpha1.UpdateMode) rightsizev1alpha1.UpdateMode {
	logger := log.FromContext(ctx)
	observationPeriod := getObservationPeriod(policy)

	cs := policy.Status.Canary

	// Spec changed since this canary cycle started: reset so the new
	// configuration is re-validated from scratch.
	if cs != nil && cs.ObservedGeneration != 0 && cs.ObservedGeneration != policy.Generation {
		logger.Info("Policy spec changed, resetting canary observation",
			"policy", policy.Name,
			"oldGeneration", cs.ObservedGeneration,
			"newGeneration", policy.Generation)
		policy.Status.Canary = nil
		cs = nil
	}

	// Phase: FullRollout already active from a prior reconcile.
	if cs != nil && cs.Phase == rightsizev1alpha1.CanaryPhaseFullRollout {
		return rightsizev1alpha1.UpdateModeAuto
	}

	// Phase: CanaryInProgress -- check if observation period has elapsed.
	if cs != nil && cs.Phase == rightsizev1alpha1.CanaryPhaseInProgress && cs.StartTime != nil {
		elapsed := r.now().Sub(cs.StartTime.Time)
		if elapsed >= observationPeriod {
			// Check for reverts during the observation window.
			hasRevert := false
			for _, h := range policy.Status.ResizeHistory {
				if h.Result == rightsizev1alpha1.ResizeResultReverted && h.Timestamp.After(cs.StartTime.Time) {
					hasRevert = true
					break
				}
			}
			if hasRevert {
				logger.Info("Canary observation found reverts, staying in canary mode",
					"policy", policy.Name, "observationPeriod", observationPeriod)
				return currentMode
			}
			logger.Info("Canary observation passed, promoting to full rollout",
				"policy", policy.Name, "observationPeriod", observationPeriod)
			policy.Status.Canary.Phase = rightsizev1alpha1.CanaryPhaseFullRollout
			return rightsizev1alpha1.UpdateModeAuto
		}
		return currentMode
	}

	// Phase: not started yet. Initialize canary tracking on the next resize.
	if cs == nil {
		now := metav1.NewTime(r.now())
		policy.Status.Canary = &rightsizev1alpha1.CanaryStatus{
			Phase:              rightsizev1alpha1.CanaryPhaseInProgress,
			StartTime:          &now,
			ObservedGeneration: policy.Generation,
		}
	}
	return currentMode
}

// resizePreChecks holds per-cycle cached data for shouldSkipResize,
// avoiding redundant API calls when checking many pods in the same namespace.
// nodeCache uses sync.Map for safe concurrent access when MaxConcurrentResizes > 1.
type resizePreChecks struct {
	nodeCache   sync.Map // string -> *corev1.Node
	limitRanges []corev1.LimitRange
	quotas      []corev1.ResourceQuota
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
	checks *resizePreChecks,
) (skip bool, reason string) {
	// Already at target (compare against clamped target, not raw recommendation,
	// so requests clamped to limits are correctly detected as no-ops).
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if c.Name == containerRec.Name {
			if c.Resources.Requests.Cpu().MilliValue() == target.Requests.Cpu().MilliValue() &&
				c.Resources.Requests.Memory().Value() == target.Requests.Memory().Value() {
				return true, ""
			}
			break
		}
	}

	// Node allocatable exceeded (use cached node data when available).
	if pod.Spec.NodeName != "" {
		var node *corev1.Node
		if checks != nil {
			if cached, ok := checks.nodeCache.Load(pod.Spec.NodeName); ok {
				node, _ = cached.(*corev1.Node)
			} else {
				var n corev1.Node
				if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &n); err == nil {
					node = &n
				}
				checks.nodeCache.Store(pod.Spec.NodeName, node)
			}
		} else {
			var n corev1.Node
			if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &n); err == nil {
				node = &n
			}
		}
		if node != nil && len(node.Status.Allocatable) > 0 {
			totalCPU := int64(0)
			totalMem := int64(0)
			// Only count containers that consume resources at runtime:
			// native sidecars (restartPolicy=Always) + regular containers.
			// Completed traditional init containers are not running.
			running := append(nativeSidecars(pod.Spec.InitContainers), pod.Spec.Containers...)
			for _, c := range running {
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
	if checks != nil {
		if err := checkQuotaCompatibilityFromLists(checks.limitRanges, checks.quotas, currentRes, target); err != nil {
			return true, "quota/limitrange violation: " + err.Error()
		}
	} else {
		if err := r.checkQuotaCompatibility(ctx, pod.Namespace, currentRes, target); err != nil {
			return true, "quota/limitrange violation: " + err.Error()
		}
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
		operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
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

		// Provenance check: only process pods owned by this policy AND whose
		// tracked workload matches this policy's targets. Prevents cross-policy
		// interference and spoofed annotations from triggering reverts.
		policyAnn := pod.Annotations[annotationPolicy]
		if policyAnn != "" && policyAnn != policy.Name {
			continue
		}
		trackedWorkload := pod.Annotations[annotationResizedWorkload]
		if trackedWorkload == "" || !workloadNames[trackedWorkload] {
			continue
		}

		records, err := parseResizeRecords(pod, observationPeriod, r.now())
		if err != nil {
			if !errors.Is(err, errNotReady) {
				logger.Error(err, "Failed to parse resize records", "pod", pod.Name)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
			}
			continue
		}

		var revertFailed bool
		for _, record := range records {
			verdict, err := monitor.CheckPod(ctx, record, r.now())
			if err != nil {
				logger.Error(err, "Safety observation check failed", "pod", pod.Name, "container", record.Container)
				operatormetrics.ReconcileErrorsTotal.WithLabelValues("safety_observation").Inc()
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
				operatormetrics.RevertsTotal.WithLabelValues(pod.Namespace, trackedWorkload, verdict.Reason).Inc()
				if r.Recorder != nil {
					r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, string(rightsizev1alpha1.ResizeResultReverted), "revert",
						"Safety observation reverted resize on pod %s/%s: %s", pod.Name, record.Container, verdict.Message)
				}
				// Mark matching history entries as reverted so status reflects the revert.
				for i := range policy.Status.ResizeHistory {
					h := &policy.Status.ResizeHistory[i]
					if h.Workload == trackedWorkload && h.Container == record.Container && h.Result == rightsizev1alpha1.ResizeResultSuccess {
						h.Result = rightsizev1alpha1.ResizeResultReverted
					}
				}
			}
		}

		// Only remove tracking annotations if all reverts succeeded.
		// If any failed, keep annotations so the next reconciliation retries.
		if revertFailed {
			continue
		}
		// Re-fetch directly from API server (not informer cache) to get
		// fresh resourceVersion after UpdateResize. The cache may not have
		// the watch event yet, causing a 409 Conflict on annotation update.
		freshPod, getErr := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if getErr != nil {
			logger.Error(getErr, "Failed to re-fetch pod for annotation cleanup", "pod", pod.Name)
			continue
		}
		removeTrackingAnnotations(freshPod)
		if updateErr := r.Update(ctx, freshPod); updateErr != nil {
			logger.Error(updateErr, "Failed to remove resize tracking annotations", "pod", pod.Name)
		}
	}
}

// errNotReady is a sentinel error indicating the pod's observation period hasn't elapsed yet.
var errNotReady = errors.New("observation period not elapsed")

// parseResizeRecords extracts safety.ResizeRecords from a pod's tracking
// annotations, one per resized container. Returns errNotReady if the
// observation period hasn't elapsed or the pod has no tracking annotations.
func parseResizeRecords(pod *corev1.Pod, observationPeriod time.Duration, now time.Time) ([]safety.ResizeRecord, error) {
	resizedAtStr, ok := pod.Annotations[annotationResizedAt]
	if !ok {
		return nil, errNotReady
	}

	resizedAt, err := time.Parse(time.RFC3339, resizedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing resized-at annotation: %w", err)
	}

	if now.Sub(resizedAt) < observationPeriod {
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

		origResources := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    originalCPU,
				corev1.ResourceMemory: originalMem,
			},
		}
		// Restore original limits if they were saved (pods that had limits set before resize).
		if cpuLimStr := pod.Annotations[annotationOriginalCPULimitPrefix+containerName]; cpuLimStr != "" {
			if cpuLim, err := resource.ParseQuantity(cpuLimStr); err == nil {
				if origResources.Limits == nil {
					origResources.Limits = make(corev1.ResourceList)
				}
				origResources.Limits[corev1.ResourceCPU] = cpuLim
			}
		}
		if memLimStr := pod.Annotations[annotationOriginalMemoryLimitPrefix+containerName]; memLimStr != "" {
			if memLim, err := resource.ParseQuantity(memLimStr); err == nil {
				if origResources.Limits == nil {
					origResources.Limits = make(corev1.ResourceList)
				}
				origResources.Limits[corev1.ResourceMemory] = memLim
			}
		}

		records = append(records, safety.ResizeRecord{
			PodName:           pod.Name,
			Namespace:         pod.Namespace,
			Container:         containerName,
			OriginalResources: origResources,
			NewResources:      currentResources,
			ResizedAt:         resizedAt,
			ObservationEnd:    resizedAt.Add(observationPeriod),
			RestartCount:      origRestartCount,
		})
	}

	if len(records) == 0 {
		return nil, errNotReady
	}
	return records, nil
}

// applyStartupBoosts checks for recently created pods that need a temporary
// CPU boost. Pods within the boost duration that don't have the boost annotation
// get inflated CPU; pods with an expired boost get reduced to steady-state.
func (r *RightSizePolicyReconciler) applyStartupBoosts(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	podsByWorkload map[string][]corev1.Pod,
	recommendations []rightsizev1alpha1.WorkloadRecommendation,
	resizer *resize.PodResizer,
) {
	boostConfig := policy.Spec.CPU.StartupBoost
	if boostConfig == nil || r.Clientset == nil {
		return
	}
	logger := log.FromContext(ctx)
	multiplier, err := strconv.ParseFloat(boostConfig.Multiplier, 64)
	if err != nil || multiplier <= 1 {
		return
	}
	boostDuration := boostConfig.Duration.Duration
	// Defense-in-depth: clamp to 1h even if the webhook was bypassed.
	if boostDuration > time.Hour {
		boostDuration = time.Hour
	}
	now := r.now()

	for _, rec := range recommendations {
		pods := podsByWorkload[rec.Workload]
		// Build per-container recommendation map for this workload.
		recMap := make(map[string]resource.Quantity, len(rec.Containers))
		for _, c := range rec.Containers {
			recMap[c.Name] = c.Recommended.CPURequest
		}

		for i := range pods {
			pod := &pods[i]
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}

			boostAtStr := pod.Annotations[annotationStartupBoostAt]
			podAge := now.Sub(pod.CreationTimestamp.Time)

			if boostAtStr == "" && podAge < boostDuration {
				// New pod within boost window: apply boosted CPU.
				boostedAny := false
				for _, c := range pod.Spec.Containers {
					recCPU, ok := recMap[c.Name]
					if !ok {
						continue
					}
					boostedMillis := int64(float64(recCPU.MilliValue()) * multiplier)
					boostedCPU := *resource.NewMilliQuantity(boostedMillis, resource.DecimalSI)
					if c.Resources.Requests.Cpu().Cmp(boostedCPU) >= 0 {
						continue // already at or above boosted level
					}
					// Safety check: verify the boosted target does not violate
					// node allocatable, ResourceQuota, LimitRange, or QoS class.
					boostRec := rightsizev1alpha1.ContainerRecommendation{
						Name: c.Name,
						Current: rightsizev1alpha1.ResourceValues{
							CPURequest:    c.Resources.Requests.Cpu().DeepCopy(),
							MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
						},
						Recommended: rightsizev1alpha1.ResourceValues{
							CPURequest:    boostedCPU.DeepCopy(),
							MemoryRequest: c.Resources.Requests.Memory().DeepCopy(),
						},
					}
					boostTarget := corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    boostedCPU,
							corev1.ResourceMemory: c.Resources.Requests.Memory().DeepCopy(),
						},
					}
					if skip, reason := r.shouldSkipResize(ctx, policy, pod, boostRec, boostTarget, nil); skip {
						logger.Info("Skipping startup boost: "+reason,
							"pod", pod.Name, "container", c.Name,
							"boostedCPU", boostedCPU.String())
						continue
					}
					refreshed, err := r.boostResizeAndRefetch(ctx, resizer, pod, c.Name, boostedCPU)
					if err != nil {
						logger.Error(err, "Failed to apply startup CPU boost", "pod", pod.Name, "container", c.Name)
						if refreshed == nil {
							break // re-fetch failed, stop processing containers
						}
						continue
					}
					boostedAny = true
					logger.Info("Applied startup CPU boost",
						"pod", pod.Name, "container", c.Name,
						"boostedCPU", boostedCPU.String(), "steadyState", recCPU.String())
					*pod = *refreshed
				}
				// Only mark the pod with boost timestamp if at least one
				// resize succeeded. Without this guard, a failed boost
				// would trigger a spurious expiry resize on the next reconcile.
				if boostedAny {
					if pod.Annotations == nil {
						pod.Annotations = make(map[string]string)
					}
					pod.Annotations[annotationStartupBoostAt] = now.UTC().Format(time.RFC3339)
					pod.Annotations[annotationPolicy] = policy.Name
					if pod.Labels == nil {
						pod.Labels = make(map[string]string)
					}
					pod.Labels[labelTracked] = "true"
					if updateErr := r.Update(ctx, pod); updateErr != nil {
						logger.Error(updateErr, "Failed to persist startup boost annotation", "pod", pod.Name)
					}
				}
			} else if boostAtStr != "" {
				// Boost was applied: check if it should expire.
				boostAt, parseErr := time.Parse(time.RFC3339, boostAtStr)
				if parseErr != nil {
					continue
				}
				if now.Sub(boostAt) >= boostDuration {
					// Boost expired: resize back to steady-state.
					for _, c := range pod.Spec.Containers {
						recCPU, ok := recMap[c.Name]
						if !ok {
							continue
						}
						refreshed, err := r.boostResizeAndRefetch(ctx, resizer, pod, c.Name, recCPU)
						if err != nil {
							logger.Error(err, "Failed to reduce startup boost", "pod", pod.Name, "container", c.Name)
							if refreshed == nil {
								break // re-fetch failed
							}
							continue
						}
						logger.Info("Startup boost expired, reduced to steady-state",
							"pod", pod.Name, "container", c.Name, "cpu", recCPU.String())
						*pod = *refreshed
					}
					delete(pod.Annotations, annotationStartupBoostAt)
					if updateErr := r.Update(ctx, pod); updateErr != nil {
						logger.Error(updateErr, "Failed to remove startup boost annotation", "pod", pod.Name)
					}
				}
			}
		}
	}
}

// adjustHPATargets checks for HPAs with the auto-tune annotation and adjusts
// boostResizeAndRefetch resizes a single container's CPU to targetCPU
// (preserving the current memory request) and re-fetches the pod from the API
// server. On resize failure it returns (original pod, err) so the caller can
// continue to the next container. On re-fetch failure it returns (nil, err),
// signaling the caller should break the container loop. On success it returns
// the refreshed pod.
func (r *RightSizePolicyReconciler) boostResizeAndRefetch(
	ctx context.Context,
	resizer *resize.PodResizer,
	pod *corev1.Pod,
	containerName string,
	targetCPU resource.Quantity,
) (*corev1.Pod, error) {
	reqs := corev1.ResourceList{corev1.ResourceCPU: targetCPU}
	if idx := findContainerIndex(pod, containerName); idx >= 0 {
		if memReq, ok := pod.Spec.Containers[idx].Resources.Requests[corev1.ResourceMemory]; ok {
			reqs[corev1.ResourceMemory] = memReq
		}
	}
	target := corev1.ResourceRequirements{Requests: reqs}
	if _, err := resizer.ResizePod(ctx, pod, containerName, target); err != nil {
		// Return the original pod (non-nil) so the caller knows this is a
		// resize failure (continue to next container), not a re-fetch failure
		// (break the loop). A nil pod signals an unrecoverable re-fetch error.
		return pod, err
	}
	freshPod, err := r.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("re-fetch after boost resize: %w", err)
	}
	return freshPod, nil
}

// findContainerIndex returns the index of the named container in pod.Spec.Containers.
// Returns -1 if not found.
func findContainerIndex(pod *corev1.Pod, name string) int {
	for i, c := range pod.Spec.Containers {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// their resource-based target utilization to maintain the same absolute resource
// threshold after a resize changes the request baseline.
func (r *RightSizePolicyReconciler) adjustHPATargets(
	ctx context.Context,
	hpas []autoscalingv2.HorizontalPodAutoscaler,
	workloadName, workloadKind string,
	oldCPURequest, newCPURequest resource.Quantity,
) {
	logger := log.FromContext(ctx)
	for i := range hpas {
		hpa := &hpas[i]
		if hpa.Spec.ScaleTargetRef.Name != workloadName || hpa.Spec.ScaleTargetRef.Kind != workloadKind {
			continue
		}
		if hpa.Annotations == nil || hpa.Annotations[annotationHPAAutoTune] != "true" {
			continue
		}
		if oldCPURequest.IsZero() || newCPURequest.IsZero() || oldCPURequest.Equal(newCPURequest) {
			continue
		}

		adjusted := false
		for j := range hpa.Spec.Metrics {
			m := &hpa.Spec.Metrics[j]
			if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
				continue
			}
			if m.Resource.Name != corev1.ResourceCPU || m.Resource.Target.Type != autoscalingv2.UtilizationMetricType || m.Resource.Target.AverageUtilization == nil {
				continue
			}
			currentTarget := *m.Resource.Target.AverageUtilization
			// Use the stored original target (from first adjustment) to avoid
			// progressive drift on subsequent cycles.
			baseTarget := currentTarget
			if stored := hpa.Annotations[annotationHPAOriginalCPU]; stored != "" {
				if v, parseErr := strconv.ParseInt(stored, 10, 32); parseErr == nil {
					baseTarget = int32(v)
				}
			}
			// newTarget = baseTarget * (oldRequest / newRequest), capped at 100.
			newTarget := int32(float64(baseTarget) * float64(oldCPURequest.MilliValue()) / float64(newCPURequest.MilliValue()))
			if newTarget > 100 {
				newTarget = 100
			}
			if newTarget < 1 {
				newTarget = 1
			}
			if newTarget == currentTarget {
				adjusted = true // no change needed but metric was found
				break
			}

			// Store original target for rollback if not already stored.
			if hpa.Annotations[annotationHPAOriginalCPU] == "" {
				if hpa.Annotations == nil {
					hpa.Annotations = make(map[string]string)
				}
				hpa.Annotations[annotationHPAOriginalCPU] = strconv.FormatInt(int64(currentTarget), 10)
			}
			logger.Info("Auto-tuning HPA CPU target after resize",
				"hpa", hpa.Name, "workload", workloadName,
				"currentTarget", currentTarget, "newTarget", newTarget,
				"oldRequest", oldCPURequest.String(), "newRequest", newCPURequest.String())
			m.Resource.Target.AverageUtilization = &newTarget
			if err := r.Update(ctx, hpa); err != nil {
				logger.Error(err, "Failed to update HPA target", "hpa", hpa.Name)
			}
			adjusted = true
			break
		}
		if !adjusted {
			logger.Info("HPA has auto-tune annotation but no adjustable CPU utilization metric",
				"hpa", hpa.Name, "workload", workloadName)
		}
	}
}

// exportRecommendationConfigMaps creates or updates ConfigMaps with
// recommendation data for GitOps workflows.
func (r *RightSizePolicyReconciler) exportRecommendationConfigMaps(
	ctx context.Context,
	policy *rightsizev1alpha1.RightSizePolicy,
	recommendations []rightsizev1alpha1.WorkloadRecommendation,
) {
	logger := log.FromContext(ctx)
	for _, rec := range recommendations {
		cmName := fmt.Sprintf("%s-%s-recommendations", policy.Name, rec.Workload)
		data := map[string]string{
			"workload": rec.Workload,
			"kind":     rec.Kind,
		}
		for _, c := range rec.Containers {
			prefix := c.Name + "."
			data[prefix+"cpu-request"] = c.Recommended.CPURequest.String()
			data[prefix+"memory-request"] = c.Recommended.MemoryRequest.String()
			if !c.Recommended.CPULimit.IsZero() {
				data[prefix+"cpu-limit"] = c.Recommended.CPULimit.String()
			}
			if !c.Recommended.MemoryLimit.IsZero() {
				data[prefix+"memory-limit"] = c.Recommended.MemoryLimit.String()
			}
			data[prefix+"confidence"] = fmt.Sprintf("%.2f", c.Confidence)
		}
		data["last-updated"] = r.now().UTC().Format(time.RFC3339)

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: policy.Namespace,
				Labels: map[string]string{
					"rightsize.io/policy":   policy.Name,
					"rightsize.io/workload": rec.Workload,
				},
			},
			Data: data,
		}
		if err := ctrl.SetControllerReference(policy, cm, r.Scheme); err != nil {
			logger.Error(err, "Failed to set owner reference on recommendation ConfigMap", "configmap", cmName)
			continue
		}

		var existing corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKeyFromObject(cm), &existing); err != nil {
			if apierrors.IsNotFound(err) {
				if createErr := r.Create(ctx, cm); createErr != nil {
					logger.Error(createErr, "Failed to create recommendation ConfigMap", "configmap", cmName)
				}
			} else {
				logger.Error(err, "Failed to check recommendation ConfigMap", "configmap", cmName)
			}
			continue
		}
		existing.Data = data
		existing.Labels = cm.Labels
		if updateErr := r.Update(ctx, &existing); updateErr != nil {
			logger.Error(updateErr, "Failed to update recommendation ConfigMap", "configmap", cmName)
		} else {
			logger.V(1).Info("Exported recommendations to ConfigMap",
				"configMap", cmName, "workload", rec.Workload)
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RightSizePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizev1alpha1.RightSizePolicy{}).
		Complete(r)
}
