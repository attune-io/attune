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
	"hash/fnv"
	"io"
	"math"
	"slices"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/recommendation"
	"github.com/attune-io/attune/internal/validation"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

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

// Default overhead percentages parsed once from the canonical string constants
// in api/v1alpha1/defaults.go. This avoids hardcoding magic numbers that could
// drift if the constants change.
var (
	defaultCPUOverhead    = mustParseFloat(attunev1alpha1.DefaultCPUOverhead)
	defaultMemoryOverhead = mustParseFloat(attunev1alpha1.DefaultMemoryOverhead)
)

func mustParseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		panic("invalid default constant: " + s)
	}
	return v
}

// getOrCreateCollector returns a cached collector for the given Prometheus
// config, creating one if needed. Delegates to getOrCreateCollectorByKey.
func (r *AttunePolicyReconciler) getOrCreateCollector(config *attunev1alpha1.PrometheusConfig, opts *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
	cacheKey := collectorCacheKey(config, opts)
	return r.getOrCreateCollectorByKey(cacheKey, config.Address, func() (rsmetrics.MetricsCollector, error) {
		return r.MetricsFactory(config.Address, opts)
	})
}

// getOrCreateCollectorByKey returns a cached collector for the given key,
// creating one via factory if needed. The cache is bounded at maxCollectors
// entries, stale entries are TTL-evicted, and LoadOrStore prevents duplicate
// collectors from concurrent goroutines.
func (r *AttunePolicyReconciler) getOrCreateCollectorByKey(cacheKey, description string, factory func() (rsmetrics.MetricsCollector, error)) (rsmetrics.MetricsCollector, error) {
	now := r.now()

	if cached, ok := r.collectors.Load(cacheKey); ok {
		entry, _ := cached.(*collectorEntry)
		if entry != nil {
			r.collectors.CompareAndSwap(cacheKey, cached, &collectorEntry{collector: entry.collector, lastUsed: now})
			return entry.collector, nil
		}
	}

	// Evict stale entries before checking capacity.
	ttl := r.CollectorTTL
	if ttl == 0 {
		ttl = collectorTTL
	}
	r.collectors.Range(func(key, value any) bool {
		entry, ok := value.(*collectorEntry)
		if !ok || entry == nil {
			r.collectors.Delete(key)
			return true
		}
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
		return nil, fmt.Errorf("collector cache full (%d entries); refusing new collector %q; consolidate policies to use fewer distinct addresses, or use an AttuneDefaults resource to share a single address across all policies", maxCollectors, description)
	}

	collector, err := factory()
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
	stored, _ := actual.(*collectorEntry)
	if stored == nil {
		return nil, fmt.Errorf("unexpected nil collector entry for key %q", description)
	}
	return stored.collector, nil
}

// secretForCacheKey returns a stable identifier for a secret value that is safe
// to embed in cache keys. We use FNV-1a (non-cryptographic hash) so that different
// secrets produce different keys (required for secret rotation to create new
// collector entries) without ever using a cryptographic hash (SHA256) on secret
// bytes. This satisfies CodeQL "weak crypto on sensitive data" while preserving
// the exact cache behavior the unit tests expect.
func secretForCacheKey(val string) string {
	if val == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(val))
	return fmt.Sprintf("%x", h.Sum64())
}

func collectorConfigPrefix(address string, headers map[string]string, tlsConfig *attunev1alpha1.TLSConfig) string {
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
		// Use non-crypto identifier for header values (may contain tokens) to avoid
		// CodeQL "weak crypto on sensitive data" while keeping cache keys stable.
		key += fmt.Sprintf("|h:%s=%s", k, secretForCacheKey(headers[k]))
	}
	return key
}

// collectorCacheKey builds a cache key that includes address, headers,
// bearer token identity, and TLS settings.
func collectorCacheKey(config *attunev1alpha1.PrometheusConfig, opts *rsmetrics.CollectorOptions) string {
	headers := map[string]string(nil)
	var tlsConfig *attunev1alpha1.TLSConfig
	if opts != nil {
		headers = opts.Headers
		if opts.InsecureSkipVerify {
			tlsConfig = &attunev1alpha1.TLSConfig{InsecureSkipVerify: true}
		}
	}
	key := collectorConfigPrefix(config.Address, headers, tlsConfig)
	if opts != nil && opts.BearerToken != "" {
		// Use non-crypto identifier for BearerToken (a secret) to avoid
		// CodeQL "weak crypto on sensitive data" while keeping cache keys stable.
		key += fmt.Sprintf("|bearer:%s", secretForCacheKey(opts.BearerToken))
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

func (r *AttunePolicyReconciler) computeRecommendations(
	ctx context.Context,
	policy *attunev1alpha1.AttunePolicy,
	workload client.Object,
	collector rsmetrics.MetricsCollector,
	qb rsmetrics.QueryBuilder,
	cpuEngine, memEngine *recommendation.RecommendationEngine,
	excludeSet map[string]bool,
	pods []corev1.Pod,
) (rec *attunev1alpha1.WorkloadRecommendation, queryErrors int, failedMetricTypes []string, maxDataPoints int, seriesCapped bool, err error) { //nolint:unparam // error return kept for interface contract
	logger := log.FromContext(ctx)
	containers := r.getContainers(workload)
	if len(containers) == 0 {
		return nil, 0, nil, 0, false, nil
	}

	// Fallback: build engines if not pre-built (used in tests).
	if cpuEngine == nil || memEngine == nil {
		cpuEngine, memEngine = buildRecommendationEngines(policy)
	}
	if excludeSet == nil {
		excludeSet = pkgdefaults.EffectiveExcludedContainers(policy)
	}

	historyWindow := r.parseHistoryWindow(policy)
	minimumDataPoints := r.getMinimumDataPoints(policy)

	now := r.now()
	start := now.Add(-historyWindow)
	// Representative pod sampling uses the shared pods list from Reconcile
	// (one NS-wide List), not a per-workload List here.
	podRegex := r.getPodRegex(workload)
	if r.maxPodsInMetricsQuery() > 0 && len(pods) > r.maxPodsInMetricsQuery() {
		podRegex = r.metricsPodRegex(workload, pods)
		logger.V(1).Info("Sampled pods for metrics query",
			"workload", workload.GetName(),
			"totalPods", len(pods),
			"sampled", r.maxPodsInMetricsQuery())
	}

	queryStep := r.getQueryStep(policy)
	if queryStep != attunev1alpha1.DefaultQueryStep {
		logger.V(1).Info("Using custom query step", "queryStep", queryStep)
	}
	// Run CPU and memory queries concurrently. They are independent queries
	// against the same metrics backend. The rate limiter provides backpressure,
	// so concurrent queries are safe.
	rateWindow := r.getRateWindow(policy)
	var cpuSamplesByContainer, memSamplesByContainer map[string][]rsmetrics.Sample
	var cpuErr, memErr, cpuCapped, memCapped bool
	var qg errgroup.Group
	qg.Go(func() error {
		cpuCtx := rsmetrics.WithNanInfLabels(ctx, policy.Namespace, policy.Name, "cpu")
		cpuSamplesByContainer, cpuErr, cpuCapped = queryMetricsGrouped(cpuCtx, collector, qb, policy.Namespace, podRegex, "cpu", start, now, queryStep, rateWindow)
		return nil
	})
	qg.Go(func() error {
		memCtx := rsmetrics.WithNanInfLabels(ctx, policy.Namespace, policy.Name, "memory")
		memSamplesByContainer, memErr, memCapped = queryMetricsGrouped(memCtx, collector, qb, policy.Namespace, podRegex, "memory", start, now, queryStep, rateWindow)
		return nil
	})
	_ = qg.Wait()
	if cpuErr {
		queryErrors++
		failedMetricTypes = append(failedMetricTypes, "CPU")
	}
	if memErr {
		queryErrors++
		failedMetricTypes = append(failedMetricTypes, "memory")
	}
	seriesCapped = cpuCapped || memCapped

	var containerRecs []attunev1alpha1.ContainerRecommendation
	eligibleContainers := 0
	// True when a container had data for only one resource and neither live
	// pods nor a prior rec could hold the missing arm (template must not
	// become a fresh apply target).
	partialUnfilled := false

	for _, container := range containers {
		if excludeSet[container.Name] {
			logger.Info("Skipping excluded container",
				"container", container.Name,
				"reason", pkgdefaults.ExclusionReason(policy, container.Name))
			continue
		}
		eligibleContainers++

		crec, ok, unfilled, pts := r.recommendContainer(ctx, recommendContainerInput{
			policy:            policy,
			workload:          workload,
			container:         container,
			cpuSamples:        samplesForContainer(cpuSamplesByContainer, container.Name),
			memSamples:        samplesForContainer(memSamplesByContainer, container.Name),
			cpuEngine:         cpuEngine,
			memEngine:         memEngine,
			pods:              pods,
			now:               now,
			minimumDataPoints: minimumDataPoints,
		})
		if pts > maxDataPoints {
			maxDataPoints = pts
		}
		if unfilled {
			partialUnfilled = true
		}
		if ok {
			containerRecs = append(containerRecs, crec)
		}
	}

	if len(containerRecs) == 0 {
		// Only reuse when an eligible container had no usable data.
		// Exclude-all must still return nil so status drops the rec.
		if eligibleContainers > 0 {
			freshness := recommendationFreshnessBound(queryStep)
			if reused := reuseStaleRecommendation(policy, workloadKindName(workload), workload.GetName(), now, freshness); reused != nil {
				logger.Info("Reusing prior recommendation as stale; Prometheus returned no fresh data",
					"workload", workload.GetName(),
					"kind", workloadKindName(workload))
				return reused, queryErrors, failedMetricTypes, maxDataPoints, seriesCapped, nil
			}
		}
		return nil, queryErrors, failedMetricTypes, maxDataPoints, seriesCapped, nil
	}

	last := latestFiniteSampleTime(cpuSamplesByContainer, memSamplesByContainer)
	if last.IsZero() {
		last = now
	}
	lastDataTime := metav1.NewTime(last)
	freshness := recommendationFreshnessBound(queryStep)
	stale := now.Sub(last) > freshness || partialUnfilled
	if stale {
		logger.Info("Recommendation is stale; last Prometheus data is older than freshness bound",
			"workload", workload.GetName(),
			"lastDataTime", lastDataTime,
			"freshnessBound", freshness,
			"partialUnfilled", partialUnfilled)
	}
	return &attunev1alpha1.WorkloadRecommendation{
		Containers:   containerRecs,
		LastDataTime: &lastDataTime,
		Stale:        stale,
	}, queryErrors, failedMetricTypes, maxDataPoints, seriesCapped, nil
}

// recommendContainerInput is the already-fetched query result for one
// container. Dual PromQL and stale reuse stay in computeRecommendations.
type recommendContainerInput struct {
	policy            *attunev1alpha1.AttunePolicy
	workload          client.Object
	container         corev1.Container
	cpuSamples        []rsmetrics.Sample
	memSamples        []rsmetrics.Sample
	cpuEngine         *recommendation.RecommendationEngine
	memEngine         *recommendation.RecommendationEngine
	pods              []corev1.Pod
	now               time.Time
	minimumDataPoints int32
}

// recommendContainer builds one container rec from grouped samples.
// ok is false when both resources are below minimumDataPoints.
// dataPoints is the larger of the CPU and memory profile counts.
func (r *AttunePolicyReconciler) recommendContainer(
	ctx context.Context,
	in recommendContainerInput,
) (rec attunev1alpha1.ContainerRecommendation, ok bool, partialUnfilled bool, dataPoints int) {
	logger := log.FromContext(ctx)
	policy := in.policy
	workload := in.workload
	container := in.container
	containerName := container.Name
	cpuSamples := in.cpuSamples
	memSamples := in.memSamples
	cpuEngine := in.cpuEngine
	memEngine := in.memEngine
	pods := in.pods
	now := in.now
	minimumDataPoints := in.minimumDataPoints

	maxSamples := r.maxProfileSamples()
	cpuSamples = rsmetrics.DownsampleSamples(cpuSamples, maxSamples)
	memSamples = rsmetrics.DownsampleSamples(memSamples, maxSamples)

	cpuProfile := rsmetrics.BuildProfile(cpuSamples)
	memProfile := rsmetrics.BuildProfile(memSamples)

	if len(cpuSamples) > 0 && cpuProfile.DataPoints == 0 {
		operatormetrics.NanInfSamplesTotal.WithLabelValues(
			policy.Namespace, policy.Name, containerName, "cpu").Inc()
		logger.V(1).Info("All CPU samples are NaN/Inf, data quality issue",
			"container", containerName, "rawSamples", len(cpuSamples))
	}
	if len(memSamples) > 0 && memProfile.DataPoints == 0 {
		operatormetrics.NanInfSamplesTotal.WithLabelValues(
			policy.Namespace, policy.Name, containerName, "memory").Inc()
		logger.V(1).Info("All memory samples are NaN/Inf, data quality issue",
			"container", containerName, "rawSamples", len(memSamples))
	}

	if pts := cpuProfile.DataPoints; pts > dataPoints {
		dataPoints = pts
	}
	if pts := memProfile.DataPoints; pts > dataPoints {
		dataPoints = pts
	}

	if cpuProfile.DataPoints < int(minimumDataPoints) && memProfile.DataPoints < int(minimumDataPoints) {
		logger.Info("Insufficient data points",
			"container", containerName,
			"cpuPoints", cpuProfile.DataPoints,
			"memPoints", memProfile.DataPoints,
			"minimum", minimumDataPoints)
		return rec, false, false, dataPoints
	}

	if len(cpuSamples) > 0 && cpuProfile.DataPoints == 0 {
		logger.V(1).Info("All CPU samples were NaN/Inf, using current CPU request",
			"container", containerName,
			"sampleCount", len(cpuSamples))
	}
	if len(memSamples) > 0 && memProfile.DataPoints == 0 {
		logger.V(1).Info("All memory samples were NaN/Inf, using current memory request",
			"container", containerName,
			"sampleCount", len(memSamples))
	}

	rec = newContainerRecommendation(container,
		safeInt32(cpuProfile.DataPoints+memProfile.DataPoints),
		(cpuProfile.Confidence+memProfile.Confidence)/2.0, now)

	explanation := &attunev1alpha1.ContainerRecommendationExplanation{}

	cpuApplied := false
	if cpuProfile.DataPoints >= int(minimumDataPoints) {
		cpuRec, cpuExplain, _ := cpuEngine.RecommendWithExplanation(cpuProfile, rec.Current.CPURequest)
		cpuAllowDecrease := policy.Spec.CPU.AllowDecrease == nil || *policy.Spec.CPU.AllowDecrease
		cpuRec = r.enforceAllowDecrease(cpuAllowDecrease, cpuRec, rec.Current.CPURequest, &cpuExplain, policy, containerName, "CPU")
		rec.Recommended.CPURequest = cpuRec
		explanation.CPU = toAPIRecommendationExplanation(cpuExplain)
		cpuApplied = true
	}

	memApplied := false
	if policy.Spec.Memory.MemoryFromCPURatio != nil && *policy.Spec.Memory.MemoryFromCPURatio != "" && explanation.CPU != nil {
		ratio := parseFloat64Ratio(*policy.Spec.Memory.MemoryFromCPURatio)
		allowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
		memRec, memExplain, applied := deriveMemoryFromCPU(
			rec.Recommended.CPURequest, ratio, memEngine, minimumDataPoints, rec.Current.MemoryRequest, allowDecrease)
		if applied {
			rec.Recommended.MemoryRequest = memRec
			memExplain.FinalAdjustment = appendNote(memExplain.FinalAdjustment,
				fmt.Sprintf("derived from CPU via memoryFromCpuRatio=%s", *policy.Spec.Memory.MemoryFromCPURatio))
			explanation.Memory = toAPIRecommendationExplanation(memExplain)
			memApplied = true
		}
	} else if memProfile.DataPoints >= int(minimumDataPoints) {
		memRec, memExplain, _ := memEngine.RecommendWithExplanation(memProfile, rec.Current.MemoryRequest)
		memAllowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
		memRec = r.enforceAllowDecrease(memAllowDecrease, memRec, rec.Current.MemoryRequest, &memExplain, policy, containerName, "memory")
		rec.Recommended.MemoryRequest = memRec
		explanation.Memory = toAPIRecommendationExplanation(memExplain)
		memApplied = true
	}

	if !cpuApplied || !memApplied {
		prior := priorContainerRecommendation(policy, workloadKindName(workload), workload.GetName(), containerName)
		if !cpuApplied && !holdMissingResourceRequest(&rec, corev1.ResourceCPU, pods, prior) {
			partialUnfilled = true
		}
		if !memApplied && !holdMissingResourceRequest(&rec, corev1.ResourceMemory, pods, prior) {
			partialUnfilled = true
		}
	}
	recordQuerySettings(policy, explanation)
	if explanation.CPU != nil || explanation.Memory != nil {
		rec.Explanation = explanation
	}

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

	if explanation.CPU != nil {
		logger.V(2).Info("CPU recommendation chain",
			"container", containerName,
			"rawPercentile", &explanation.CPU.RawPercentile,
			"afterOverhead", &explanation.CPU.AfterOverhead,
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
			"afterOverhead", &explanation.Memory.AfterOverhead,
			"burstFactor", explanation.Memory.BurstFactor,
			"afterConfidence", &explanation.Memory.AfterConfidence,
			"boundsApplied", explanation.Memory.BoundsApplied,
			"changeFilter", explanation.Memory.ChangeFilterApplied,
			"final", &explanation.Memory.Final)
	}

	scaleControlledLimits(policy, &rec, rec.Current.CPURequest, rec.Current.CPULimit, rec.Current.MemoryRequest, rec.Current.MemoryLimit)

	setRecommendationGauges(policy.Namespace, workload.GetName(), containerName, &rec)
	if rec.Explanation != nil {
		if rec.Explanation.CPU != nil {
			operatormetrics.BurstFactor.WithLabelValues(policy.Namespace, workload.GetName(), containerName, "cpu").Set(rec.Explanation.CPU.BurstFactor)
		}
		if rec.Explanation.Memory != nil {
			operatormetrics.BurstFactor.WithLabelValues(policy.Namespace, workload.GetName(), containerName, "memory").Set(rec.Explanation.Memory.BurstFactor)
		}
	}

	return rec, true, partialUnfilled, dataPoints
}

// reuseStaleRecommendation copies a prior status rec so an empty Prometheus
// query does not wipe the last known sizing. LastDataTime stays the last
// non-empty sample. Callers must treat the copy as stale. Reuse is refused
// when Kind does not match (including an empty prior Kind), LastDataTime
// is missing, or the last sample is older than freshness.
func reuseStaleRecommendation(policy *attunev1alpha1.AttunePolicy, kind, workload string, now time.Time, freshness time.Duration) *attunev1alpha1.WorkloadRecommendation {
	if policy == nil || workload == "" {
		return nil
	}
	for i := range policy.Status.Recommendations {
		prior := &policy.Status.Recommendations[i]
		if prior.Workload != workload || len(prior.Containers) == 0 {
			continue
		}
		if kind != "" && prior.Kind != kind {
			continue
		}
		if prior.LastDataTime == nil || now.Sub(prior.LastDataTime.Time) > freshness {
			continue
		}
		rec := prior.DeepCopy()
		rec.Stale = true
		return rec
	}
	return nil
}

// holdMissingResourceRequest copies live (max request across pods) or last
// successful Recommended into Current and Recommended for a resource that
// had no usable series. When both exist, the larger request wins so a
// last rec is not overwritten by pods still at the template. A zero live
// limit is applied too; leaving the template limit would clamp the hold.
// Returns false when no hold value exists.
func holdMissingResourceRequest(
	rec *attunev1alpha1.ContainerRecommendation,
	res corev1.ResourceName,
	pods []corev1.Pod,
	prior *attunev1alpha1.ContainerRecommendation,
) bool {
	if rec == nil {
		return false
	}
	if res != corev1.ResourceCPU && res != corev1.ResourceMemory {
		return false
	}
	liveReq, liveLim, liveOK := liveResourceHold(pods, rec.Name, res)
	priorReq, priorLim, priorOK := priorResourceHold(prior, res)
	if !liveOK && !priorOK {
		return false
	}

	req, lim := liveReq, liveLim
	// Prefer last rec when it is larger than live, or when live is still
	// the template Current (in-place resize not applied yet).
	if priorOK {
		templateReq := requestFromResourceValues(rec.Current, res)
		if !liveOK || priorReq.Cmp(liveReq) > 0 || liveReq.Equal(templateReq) {
			req, lim = priorReq, priorLim
		}
	}

	switch res {
	case corev1.ResourceCPU:
		rec.Current.CPURequest = req.DeepCopy()
		rec.Recommended.CPURequest = req.DeepCopy()
		rec.Current.CPULimit = lim.DeepCopy()
		rec.Recommended.CPULimit = lim.DeepCopy()
	case corev1.ResourceMemory:
		rec.Current.MemoryRequest = req.DeepCopy()
		rec.Recommended.MemoryRequest = req.DeepCopy()
		rec.Current.MemoryLimit = lim.DeepCopy()
		rec.Recommended.MemoryLimit = lim.DeepCopy()
	}
	return true
}

func requestFromResourceValues(v attunev1alpha1.ResourceValues, res corev1.ResourceName) k8sresource.Quantity {
	switch res {
	case corev1.ResourceCPU:
		return v.CPURequest
	case corev1.ResourceMemory:
		return v.MemoryRequest
	default:
		return k8sresource.Quantity{}
	}
}

func liveResourceHold(pods []corev1.Pod, container string, res corev1.ResourceName) (req, lim k8sresource.Quantity, ok bool) {
	for i := range pods {
		c := findContainerByName(&pods[i], container)
		if c == nil {
			continue
		}
		q, has := c.Resources.Requests[res]
		if !has || q.IsZero() {
			continue
		}
		if !ok || q.Cmp(req) > 0 {
			req = q.DeepCopy()
			if l, hasLim := c.Resources.Limits[res]; hasLim && !l.IsZero() {
				lim = l.DeepCopy()
			} else {
				lim = k8sresource.Quantity{}
			}
			ok = true
		}
	}
	return req, lim, ok
}

func priorResourceHold(prior *attunev1alpha1.ContainerRecommendation, res corev1.ResourceName) (req, lim k8sresource.Quantity, ok bool) {
	if prior == nil {
		return req, lim, false
	}
	switch res {
	case corev1.ResourceCPU:
		req = prior.Recommended.CPURequest
		lim = prior.Recommended.CPULimit
		if req.IsZero() {
			req = prior.Current.CPURequest
			lim = prior.Current.CPULimit
		}
	case corev1.ResourceMemory:
		req = prior.Recommended.MemoryRequest
		lim = prior.Recommended.MemoryLimit
		if req.IsZero() {
			req = prior.Current.MemoryRequest
			lim = prior.Current.MemoryLimit
		}
	}
	return req, lim, !req.IsZero()
}

func priorContainerRecommendation(policy *attunev1alpha1.AttunePolicy, kind, workload, container string) *attunev1alpha1.ContainerRecommendation {
	if policy == nil || workload == "" || container == "" {
		return nil
	}
	for i := range policy.Status.Recommendations {
		prior := &policy.Status.Recommendations[i]
		if prior.Workload != workload {
			continue
		}
		if kind != "" && prior.Kind != kind {
			continue
		}
		for j := range prior.Containers {
			if prior.Containers[j].Name == container {
				return &prior.Containers[j]
			}
		}
	}
	return nil
}

// latestFiniteSampleTime is the newest timestamp among finite samples.
// NaN/Inf points are ignored so LastDataTime tracks usable Prometheus data.
func latestFiniteSampleTime(groups ...map[string][]rsmetrics.Sample) time.Time {
	var latest time.Time
	for _, group := range groups {
		for _, samples := range group {
			for _, sample := range samples {
				if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
					continue
				}
				if sample.Timestamp.After(latest) {
					latest = sample.Timestamp
				}
			}
		}
	}
	return latest
}

// maxProfileSamples returns the sample cap for BuildProfile input.
func (r *AttunePolicyReconciler) maxProfileSamples() int {
	if r.MaxProfileSamples < 0 {
		return 0 // unlimited
	}
	if r.MaxProfileSamples > 0 {
		return r.MaxProfileSamples
	}
	return rsmetrics.DefaultMaxProfileSamples
}

// buildCollectorOptions constructs CollectorOptions from the given PrometheusConfig,
// including headers, query parameters, TLS settings, and Secret-backed bearer token resolution.
func (r *AttunePolicyReconciler) buildCollectorOptions(ctx context.Context, namespace string, config *attunev1alpha1.PrometheusConfig) (*rsmetrics.CollectorOptions, error) {
	if err := validation.PrometheusQueryParameters(config.QueryParameters); err != nil {
		return nil, err
	}
	// MaxSeries: 0 on flag = collector default; negative = unlimited.
	needOpts := config.Headers != nil || config.QueryParameters != nil || config.BearerTokenSecret != nil ||
		(config.TLS != nil && config.TLS.InsecureSkipVerify) || r.MaxPrometheusSeries != 0
	if !needOpts {
		return nil, nil
	}

	opts := &rsmetrics.CollectorOptions{
		Headers:         config.Headers,
		QueryParameters: config.QueryParameters,
		MaxSeries:       r.MaxPrometheusSeries,
	}
	if config.TLS != nil {
		opts.InsecureSkipVerify = config.TLS.InsecureSkipVerify
	}
	if config.BearerTokenSecret != nil {
		secretName := config.BearerTokenSecret.Name
		secretKey := config.BearerTokenSecret.Key
		// Security: only read Secrets in the policy's own namespace to prevent
		// cross-namespace Secret access if the operator is compromised.
		token, err := r.readSecretKey(ctx, namespace, secretName, secretKey)
		if err != nil {
			return nil, fmt.Errorf("cannot read bearer token secret %s/%s: %w", secretName, secretKey, err)
		}
		opts.BearerToken = token
	}
	return opts, nil
}

// resolveMetricsCollector creates the appropriate MetricsCollector and
// QueryBuilder based on which metricsSource field is configured. Falls back
// to Prometheus when no explicit source is set.
func (r *AttunePolicyReconciler) resolveMetricsCollector(ctx context.Context, policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) (rsmetrics.MetricsCollector, rsmetrics.QueryBuilder, error) {
	ms := policy.Spec.MetricsSource

	switch {
	case ms.VPA != nil:
		// VPA source: recommendations come from the VPA object, not a metrics backend.
		// Return nil collector/queryBuilder; processWorkloads handles the VPA path.
		return nil, nil, nil
	case ms.Datadog != nil:
		return r.resolveDatadogCollector(ctx, policy)
	case ms.CloudWatch != nil:
		return r.resolveCloudWatchCollector(ctx, policy)
	default:
		// Prometheus (existing path, including auto-discovery and defaults).
		promConfig, err := r.resolvePrometheusConfig(ctx, policy, defaults)
		if err != nil {
			return nil, nil, err
		}
		opts, err := r.buildCollectorOptions(ctx, policy.Namespace, promConfig)
		if err != nil {
			return nil, nil, err
		}
		collector, err := r.getOrCreateCollector(promConfig, opts)
		if err != nil {
			return nil, nil, err
		}
		return collector, r.promQLBuilder(policy), nil
	}
}

// resolvePodAggregation maps the policy field to the PromQL reduction used
// by QueryBuilder. Empty and unknown values follow applyPodAggregation (Max).
func resolvePodAggregation(policy *attunev1alpha1.AttunePolicy) rsmetrics.PodAggregationMode {
	switch policy.Spec.MetricsSource.PodAggregation {
	case "Avg":
		return rsmetrics.PodAggregationAvg
	case "None":
		return rsmetrics.PodAggregationNone
	default:
		return rsmetrics.PodAggregationMax
	}
}

func podAggregationNote(policy *attunev1alpha1.AttunePolicy) string {
	return "podAggregation=" + string(resolvePodAggregation(policy))
}

func burstSensitivityNote(raw *string) string {
	bs := recommendation.DefaultBurstSensitivity
	if raw != nil {
		bs = parseFloat64NonNeg(*raw, recommendation.DefaultBurstSensitivity)
	}
	return fmt.Sprintf("burstSensitivity=%g", bs)
}

// recordQuerySettings stamps the PromQL aggregation and the burst
// sensitivity that buildRecommendationEngines passed into the estimator.
func recordQuerySettings(policy *attunev1alpha1.AttunePolicy, explanation *attunev1alpha1.ContainerRecommendationExplanation) {
	if explanation == nil {
		return
	}
	agg := podAggregationNote(policy)
	if explanation.CPU != nil {
		explanation.CPU.FinalAdjustment = appendNote(explanation.CPU.FinalAdjustment, agg)
		explanation.CPU.FinalAdjustment = appendNote(explanation.CPU.FinalAdjustment, burstSensitivityNote(policy.Spec.CPU.BurstSensitivity))
	}
	if explanation.Memory != nil {
		explanation.Memory.FinalAdjustment = appendNote(explanation.Memory.FinalAdjustment, agg)
		explanation.Memory.FinalAdjustment = appendNote(explanation.Memory.FinalAdjustment, burstSensitivityNote(policy.Spec.Memory.BurstSensitivity))
	}
}

// promQLBuilder constructs a PromQLQueryBuilder from policy metricsSource settings.
func (r *AttunePolicyReconciler) promQLBuilder(policy *attunev1alpha1.AttunePolicy) *rsmetrics.PromQLQueryBuilder {
	return &rsmetrics.PromQLQueryBuilder{
		Aggregation:  resolvePodAggregation(policy),
		CPUMetric:    policy.Spec.MetricsSource.CPURecordingMetric,
		MemoryMetric: policy.Spec.MetricsSource.MemoryRecordingMetric,
	}
}

// resolveDatadogCollector creates a DatadogCollector from the policy's
// Datadog config, reading API/app keys from the referenced Secret.
func (r *AttunePolicyReconciler) resolveDatadogCollector(ctx context.Context, policy *attunev1alpha1.AttunePolicy) (rsmetrics.MetricsCollector, rsmetrics.QueryBuilder, error) {
	dd := policy.Spec.MetricsSource.Datadog

	site := dd.Site
	if site == "" {
		site = "datadoghq.com"
	}
	if err := validation.DatadogSite(site); err != nil {
		return nil, nil, fmt.Errorf("datadog site: %w", err)
	}

	// One Get for the API-key Secret. API key is required; app-key is optional
	// (absent key is empty). Other Get errors already fail the required key.
	var secret corev1.Secret
	secretNS := policy.Namespace
	secretName := dd.APIKeySecretRef.Name
	if err := r.Get(ctx, types.NamespacedName{Namespace: secretNS, Name: secretName}, &secret); err != nil {
		return nil, nil, fmt.Errorf("cannot read Datadog API key: %w", fmt.Errorf("reading secret %s/%s: %w", secretNS, secretName, err))
	}
	apiKeyData, ok := secret.Data[dd.APIKeySecretRef.Key]
	if !ok {
		return nil, nil, fmt.Errorf("cannot read Datadog API key: %w", fmt.Errorf("key %q not found in secret %s/%s", dd.APIKeySecretRef.Key, secretNS, secretName))
	}
	apiKey := string(apiKeyData)
	var appKey string
	if appKeyData, ok := secret.Data["app-key"]; ok {
		appKey = string(appKeyData)
	}

	// Cache keyed by site + API key + app key (non-crypto identifiers) so
	// adding or rotating app-key creates a new collector. Full TTL eviction,
	// capacity bound, and race-safe LoadOrStore.
	cacheKey := fmt.Sprintf("datadog:%s|%s|%s", site, secretForCacheKey(apiKey), secretForCacheKey(appKey))
	collector, err := r.getOrCreateCollectorByKey(cacheKey, "datadog:"+site, func() (rsmetrics.MetricsCollector, error) {
		inner, innerErr := rsmetrics.NewDatadogCollector(site, apiKey, appKey, log.FromContext(ctx).WithName("datadog"))
		if innerErr != nil {
			return nil, innerErr
		}
		// Datadog: 300 requests/hour => ~0.08 QPS; burst of 3 for concurrent queries.
		return rsmetrics.NewRateLimitedCollector(inner, 0.08, 3), nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating Datadog collector: %w", err)
	}
	return collector, &rsmetrics.DatadogQueryBuilder{}, nil
}

// resolveCloudWatchCollector creates a CloudWatchCollector from the policy's
// CloudWatch config, using the default AWS credential chain.
func (r *AttunePolicyReconciler) resolveCloudWatchCollector(ctx context.Context, policy *attunev1alpha1.AttunePolicy) (rsmetrics.MetricsCollector, rsmetrics.QueryBuilder, error) {
	cw := policy.Spec.MetricsSource.CloudWatch

	// Cache the collector keyed by region + cluster + role, with full
	// TTL eviction, capacity bound, and race-safe LoadOrStore.
	cacheKey := fmt.Sprintf("cloudwatch:%s|%s|%s", cw.Region, cw.ClusterName, cw.RoleARN)
	collector, err := r.getOrCreateCollectorByKey(cacheKey, "cloudwatch:"+cw.Region, func() (rsmetrics.MetricsCollector, error) {
		inner, innerErr := rsmetrics.NewCloudWatchCollector(ctx, cw.Region, cw.ClusterName, cw.RoleARN, log.FromContext(ctx).WithName("cloudwatch"))
		if innerErr != nil {
			return nil, innerErr
		}
		// CloudWatch: 50 TPS quota; use 5 QPS with burst of 10 to stay safe.
		return rsmetrics.NewRateLimitedCollector(inner, 5, 10), nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating CloudWatch collector: %w", err)
	}
	qb := &rsmetrics.CloudWatchQueryBuilder{ClusterName: cw.ClusterName}
	return collector, qb, nil
}

// resolvePrometheusAddress returns the Prometheus address from the policy spec,
// falling back to the cluster-scoped AttuneDefaults if not set.
func (r *AttunePolicyReconciler) resolvePrometheusConfig(ctx context.Context, policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) (*attunev1alpha1.PrometheusConfig, error) {
	// Check policy-level config first.
	if policy.Spec.MetricsSource.Prometheus != nil &&
		policy.Spec.MetricsSource.Prometheus.Address != "" {
		config := policy.Spec.MetricsSource.Prometheus.DeepCopy()
		if err := validation.PrometheusAddress(config.Address); err != nil {
			return nil, fmt.Errorf("SSRF blocked: %w", err)
		}
		return config, nil
	}

	// Fall back to AttuneDefaults.
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
			return &attunev1alpha1.PrometheusConfig{Address: discovered}, nil
		}
	}
	return nil, fmt.Errorf("no Prometheus address configured in policy or cluster defaults, and auto-discovery found no Prometheus instance")
}

// readSecretKey reads a single key from a Kubernetes Secret.
func (r *AttunePolicyReconciler) readSecretKey(ctx context.Context, namespace, name, key string) (string, error) {
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
func (r *AttunePolicyReconciler) discoverPrometheus(ctx context.Context) string {
	const promDiscoveryCacheTTL = 5 * time.Minute

	r.discoveredPromMu.Lock()
	if !r.discoveredPromTime.IsZero() && r.now().Sub(r.discoveredPromTime) < promDiscoveryCacheTTL {
		addr := r.discoveredPromAddr
		r.discoveredPromMu.Unlock()
		return addr
	}
	r.discoveredPromMu.Unlock()

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
		r.cacheDiscoveredPrometheus(addr)
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
			port := int64(9090)
			if len(service.Spec.Ports) > 0 {
				port = int64(service.Spec.Ports[0].Port)
			}
			addr := fmt.Sprintf("http://%s.%s:%d", svc.name, svc.namespace, port)
			logger.V(1).Info("Found well-known Prometheus service", "address", addr)
			r.cacheDiscoveredPrometheus(addr)
			return addr
		}
	}

	// Cache negative result to avoid repeated API calls when no Prometheus
	// is found (common during initial setup or in staging environments).
	r.cacheDiscoveredPrometheus("")
	return ""
}

func (r *AttunePolicyReconciler) cacheDiscoveredPrometheus(addr string) {
	r.discoveredPromMu.Lock()
	r.discoveredPromAddr = addr
	r.discoveredPromTime = r.now()
	r.discoveredPromMu.Unlock()
}

func samplesForContainer(grouped map[string][]rsmetrics.Sample, container string) []rsmetrics.Sample {
	if samples, ok := grouped[container]; ok {
		return samples
	}
	return grouped[""]
}

// deriveMemoryFromCPU computes a memory recommendation by deriving it from
// the CPU recommendation using a fixed ratio instead of Prometheus memory
// metrics. The derived value passes through the memory engine's bounds and
// change-filter pipeline via a synthetic usage profile.
//
// Returns the recommended quantity, explanation, and whether derivation was
// applied (false when the ratio is non-positive).
func deriveMemoryFromCPU(
	cpuRec k8sresource.Quantity,
	ratio float64,
	memEngine *recommendation.RecommendationEngine,
	minimumDataPoints int32,
	currentMemReq k8sresource.Quantity,
	allowDecrease bool,
) (k8sresource.Quantity, recommendation.RecommendationExplanation, bool) {
	if ratio <= 0 {
		return currentMemReq.DeepCopy(), recommendation.RecommendationExplanation{}, false
	}

	// CPU recommendation is in millicores. Convert to cores, multiply
	// by ratio to get GiB, then convert to bytes for the memory engine.
	cpuCores := float64(cpuRec.MilliValue()) / 1000
	memBytes := int64(cpuCores * ratio * 1024 * 1024 * 1024)

	// Pass through the memory engine's bounds + change filter by
	// running it with a synthetic profile that targets the derived value.
	// Confidence is set very high (1e9) so the engine clamps it to 1.0,
	// giving factor = 1 + M*(1-1.0)^E = 1.0. A ratio-derived value
	// is deterministic and should not receive the statistical uncertainty
	// buffer that Prometheus-sourced recommendations get.
	memRec, memExplain, _ := memEngine.RecommendWithExplanation(
		rsmetrics.UsageProfile{
			OverallPercentiles: rsmetrics.PercentileSet{
				P50: float64(memBytes), P90: float64(memBytes),
				P95: float64(memBytes), P99: float64(memBytes),
				Max: float64(memBytes),
			},
			DataPoints: int(minimumDataPoints),
			Confidence: 1e9,
		}, currentMemReq)

	if !allowDecrease && memRec.Cmp(currentMemReq) < 0 {
		memRec = currentMemReq.DeepCopy()
		memExplain.Final = memRec.DeepCopy()
		memExplain.FinalAdjustment = fmt.Sprintf("Memory decrease blocked by allowDecrease=false (derived from CPU via ratio %.4g)", ratio)
	}

	return memRec, memExplain, true
}

// appendNote appends a note to an existing adjustment string, separated by "; ".
func appendNote(existing, note string) string {
	if existing == "" {
		return note
	}
	return existing + "; " + note
}

// toAPIRecommendationExplanation converts an internal explanation to the API
// type. The explanation parameter is passed by value (already a copy) and is
// not referenced after this call, so quantities are assigned directly without
// redundant DeepCopy.
func toAPIRecommendationExplanation(explanation recommendation.RecommendationExplanation) *attunev1alpha1.ResourceRecommendationExplanation {
	return &attunev1alpha1.ResourceRecommendationExplanation{
		RawPercentile:    explanation.RawPercentile,
		Overhead:         explanation.Overhead,
		AfterOverhead:    explanation.AfterOverhead,
		BurstFactor:      explanation.BurstFactor,
		AfterBurst:       explanation.AfterBurst,
		Confidence:       explanation.Confidence,
		ConfidenceFactor: explanation.ConfidenceFactor,
		AfterConfidence:  explanation.AfterConfidence,
		Bounds: attunev1alpha1.ResourceBounds{
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
func buildRecommendationEngines(policy *attunev1alpha1.AttunePolicy) (cpuEngine, memEngine *recommendation.RecommendationEngine) {
	cpuPercentile := int(policy.Spec.CPU.Percentile)
	if cpuPercentile == 0 {
		cpuPercentile = int(attunev1alpha1.DefaultCPUPercentile)
	}
	memPercentile := int(policy.Spec.Memory.Percentile)
	if memPercentile == 0 {
		memPercentile = int(attunev1alpha1.DefaultMemoryPercentile)
	}

	cpuOverhead := parseOverheadPercent(policy.Spec.CPU.Overhead, defaultCPUOverhead)
	memOverhead := parseOverheadPercent(policy.Spec.Memory.Overhead, defaultMemoryOverhead)

	cpuBoundsMin := attunev1alpha1.DefaultCPUBoundsMin.DeepCopy()
	cpuBoundsMax := attunev1alpha1.DefaultCPUBoundsMax.DeepCopy()
	if policy.Spec.CPU.MinAllowed != nil {
		cpuBoundsMin = policy.Spec.CPU.MinAllowed.DeepCopy()
	}
	if policy.Spec.CPU.MaxAllowed != nil {
		cpuBoundsMax = policy.Spec.CPU.MaxAllowed.DeepCopy()
	}

	memBoundsMin := attunev1alpha1.DefaultMemoryBoundsMin.DeepCopy()
	memBoundsMax := attunev1alpha1.DefaultMemoryBoundsMax.DeepCopy()
	if policy.Spec.Memory.MinAllowed != nil {
		memBoundsMin = policy.Spec.Memory.MinAllowed.DeepCopy()
	}
	if policy.Spec.Memory.MaxAllowed != nil {
		memBoundsMax = policy.Spec.Memory.MaxAllowed.DeepCopy()
	}

	// Resolve directional change caps with precedence:
	// maxIncreasePercent/maxDecreasePercent > maxChangePercent > built-in default.
	cpuIncrease, cpuDecrease := resolveChangeCaps(policy.Spec.CPU,
		attunev1alpha1.DefaultCPUMaxChangePercent)
	memIncrease, memDecrease := resolveChangeCaps(policy.Spec.Memory,
		attunev1alpha1.DefaultMemoryMaxChangePercent)

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

	cpuEngine = recommendation.NewEngine(cpuPercentile, cpuOverhead, cpuBoundsMin, cpuBoundsMax, cpuIncrease, cpuDecrease, cpuOpts)
	memEngine = recommendation.NewEngine(memPercentile, memOverhead, memBoundsMin, memBoundsMax, memIncrease, memDecrease, memOpts)
	return cpuEngine, memEngine
}

// resolveChangeCaps resolves directional change caps from the ResourceConfig.
// Precedence: maxIncreasePercent/maxDecreasePercent > maxChangePercent > builtInDefault.
// Defense-in-depth: clamps to [1, 100] even if webhook is bypassed.
func resolveChangeCaps(rc attunev1alpha1.ResourceConfig, builtInDefault int32) (increase, decrease float64) {
	base := builtInDefault
	if rc.MaxChangePercent != nil {
		base = *rc.MaxChangePercent
	}
	inc := base
	if rc.MaxIncreasePercent != nil {
		inc = *rc.MaxIncreasePercent
	}
	dec := base
	if rc.MaxDecreasePercent != nil {
		dec = *rc.MaxDecreasePercent
	}
	return min(max(float64(inc), 1), 100), min(max(float64(dec), 1), 100)
}
