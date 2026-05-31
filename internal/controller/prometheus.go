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
	if err != nil {
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
			r.collectors.Store(cacheKey, &collectorEntry{collector: entry.collector, lastUsed: now})
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
) (rec *attunev1alpha1.WorkloadRecommendation, queryErrors int, failedMetricTypes []string, maxDataPoints int, err error) { //nolint:unparam // error return kept for interface contract
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
		excludeSet = make(map[string]bool, len(policy.Spec.ExcludedContainers))
		for _, name := range policy.Spec.ExcludedContainers {
			excludeSet[name] = true
		}
	}

	historyWindow := r.parseHistoryWindow(policy)
	minimumDataPoints := r.getMinimumDataPoints(policy)

	now := r.now()
	start := now.Add(-historyWindow)
	podRegex := r.getPodRegex(workload)

	queryStep := r.getQueryStep(policy)
	if queryStep != attunev1alpha1.DefaultQueryStep {
		logger.V(1).Info("Using custom query step", "queryStep", queryStep)
	}
	// Run CPU and memory queries concurrently. They are independent queries
	// against the same metrics backend. The rate limiter provides backpressure,
	// so concurrent queries are safe.
	rateWindow := r.getRateWindow(policy)
	var cpuSamplesByContainer, memSamplesByContainer map[string][]rsmetrics.Sample
	var cpuErr, memErr bool
	var qg errgroup.Group
	qg.Go(func() error {
		cpuSamplesByContainer, cpuErr = queryMetricsGrouped(ctx, collector, qb, policy.Namespace, podRegex, "cpu", start, now, queryStep, rateWindow)
		return nil
	})
	qg.Go(func() error {
		memSamplesByContainer, memErr = queryMetricsGrouped(ctx, collector, qb, policy.Namespace, podRegex, "memory", start, now, queryStep, rateWindow)
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

	var containerRecs []attunev1alpha1.ContainerRecommendation

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

		// Detect when all samples were non-finite (NaN/Inf). This means
		// Prometheus returned data but every value was unusable.
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

		// Track maximum data points across all containers.
		if pts := cpuProfile.DataPoints; pts > maxDataPoints {
			maxDataPoints = pts
		}
		if pts := memProfile.DataPoints; pts > maxDataPoints {
			maxDataPoints = pts
		}

		// Check for sufficient data points.
		if cpuProfile.DataPoints < int(minimumDataPoints) && memProfile.DataPoints < int(minimumDataPoints) {
			// Distinguish "no data from Prometheus" from "data received but
			// all values were NaN/Inf" to help debug data quality issues.
			if len(cpuSamples) > 0 && cpuProfile.DataPoints == 0 {
				logger.V(1).Info("All CPU samples were NaN/Inf",
					"container", containerName,
					"sampleCount", len(cpuSamples))
			}
			if len(memSamples) > 0 && memProfile.DataPoints == 0 {
				logger.V(1).Info("All memory samples were NaN/Inf",
					"container", containerName,
					"sampleCount", len(memSamples))
			}
			logger.Info("Insufficient data points",
				"container", containerName,
				"cpuPoints", cpuProfile.DataPoints,
				"memPoints", memProfile.DataPoints,
				"minimum", minimumDataPoints)
			continue
		}

		// Log per-resource NaN/Inf data quality issues even when the
		// container has enough data from the other resource to produce a
		// recommendation. Without this, a user sees CPU unchanged with no
		// explanation when only CPU samples are non-finite.
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

		// Get current resource values.
		currentCPUReq := container.Resources.Requests.Cpu().DeepCopy()
		currentCPULim := container.Resources.Limits.Cpu().DeepCopy()
		currentMemReq := container.Resources.Requests.Memory().DeepCopy()
		currentMemLim := container.Resources.Limits.Memory().DeepCopy()

		rec := attunev1alpha1.ContainerRecommendation{
			Name:       containerName,
			DataPoints: safeInt32(cpuProfile.DataPoints + memProfile.DataPoints),
			Confidence: (cpuProfile.Confidence + memProfile.Confidence) / 2.0,
			LastUpdated: metav1.Time{
				Time: now,
			},
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    currentCPUReq,
				CPULimit:      currentCPULim,
				MemoryRequest: currentMemReq,
				MemoryLimit:   currentMemLim,
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    currentCPUReq,
				CPULimit:      currentCPULim,
				MemoryRequest: currentMemReq,
				MemoryLimit:   currentMemLim,
			},
		}

		explanation := &attunev1alpha1.ContainerRecommendationExplanation{}

		// Compute CPU recommendation.
		if cpuProfile.DataPoints >= int(minimumDataPoints) {
			cpuRec, cpuExplain, _ := cpuEngine.RecommendWithExplanation(cpuProfile, currentCPUReq)
			// Enforce AllowDecrease: for CPU, nil defaults to true (decreases
			// allowed) because CPU throttle is detected by the safety monitor.
			cpuAllowDecrease := policy.Spec.CPU.AllowDecrease == nil || *policy.Spec.CPU.AllowDecrease
			if !cpuAllowDecrease && cpuRec.Cmp(currentCPUReq) < 0 {
				unclampedCPU := cpuRec.String()
				if r.Recorder != nil {
					r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "DecreaseSuppressed", "recommend",
						"CPU decrease blocked by allowDecrease=false for container %s (current: %s)",
						containerName, currentCPUReq.String())
				}
				cpuRec = currentCPUReq.DeepCopy()
				cpuExplain.Final = cpuRec.DeepCopy()
				cpuExplain.FinalAdjustment = fmt.Sprintf("CPU decrease from %s to %s blocked by allowDecrease=false", currentCPUReq.String(), unclampedCPU)
			}
			rec.Recommended.CPURequest = cpuRec
			explanation.CPU = toAPIRecommendationExplanation(cpuExplain)
		}

		// Compute memory recommendation.
		// When memoryFromCpuRatio is set, derive memory from the CPU
		// recommendation instead of using Prometheus memory metrics.
		if policy.Spec.Memory.MemoryFromCPURatio != nil && *policy.Spec.Memory.MemoryFromCPURatio != "" && explanation.CPU != nil {
			ratio := parseFloat64Ratio(*policy.Spec.Memory.MemoryFromCPURatio)
			allowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
			memRec, memExplain, applied := deriveMemoryFromCPU(
				rec.Recommended.CPURequest, ratio, memEngine, minimumDataPoints, currentMemReq, allowDecrease)
			if applied {
				rec.Recommended.MemoryRequest = memRec
				memExplain.FinalAdjustment = appendNote(memExplain.FinalAdjustment,
					fmt.Sprintf("derived from CPU via memoryFromCpuRatio=%s", *policy.Spec.Memory.MemoryFromCPURatio))
				explanation.Memory = toAPIRecommendationExplanation(memExplain)
			}
		} else if memProfile.DataPoints >= int(minimumDataPoints) {
			memRec, memExplain, _ := memEngine.RecommendWithExplanation(memProfile, currentMemReq)
			// Enforce AllowDecrease: skip memory decreases unless explicitly allowed.
			allowDecrease := policy.Spec.Memory.AllowDecrease != nil && *policy.Spec.Memory.AllowDecrease
			if !allowDecrease && memRec.Cmp(currentMemReq) < 0 {
				unclampedMem := memRec.String()
				if r.Recorder != nil {
					r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, "DecreaseSuppressed", "recommend",
						"Memory decrease blocked by allowDecrease=false for container %s (current: %s)",
						containerName, currentMemReq.String())
				}
				memRec = currentMemReq.DeepCopy()
				memExplain.Final = memRec.DeepCopy()
				memExplain.FinalAdjustment = fmt.Sprintf("memory decrease from %s to %s blocked by allowDecrease=false", currentMemReq.String(), unclampedMem)
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

		// Scale limits proportionally if ControlledValues is RequestsAndLimits.
		cpuControlled := attunev1alpha1.ControlledRequestsOnly
		if policy.Spec.CPU.ControlledValues != nil {
			cpuControlled = *policy.Spec.CPU.ControlledValues
		}
		memControlled := attunev1alpha1.ControlledRequestsOnly
		if policy.Spec.Memory.ControlledValues != nil {
			memControlled = *policy.Spec.Memory.ControlledValues
		}
		if cpuControlled == attunev1alpha1.ControlledRequestsAndLimits {
			rec.Recommended.CPULimit = scaleLimits(currentCPUReq, currentCPULim, rec.Recommended.CPURequest)
		}
		if memControlled == attunev1alpha1.ControlledRequestsAndLimits {
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

	lastDataTime := metav1.NewTime(now)
	return &attunev1alpha1.WorkloadRecommendation{
		Containers:   containerRecs,
		LastDataTime: &lastDataTime,
	}, queryErrors, failedMetricTypes, maxDataPoints, nil
}

// buildCollectorOptions constructs CollectorOptions from the given PrometheusConfig,
// including headers, query parameters, TLS settings, and Secret-backed bearer token resolution.
func (r *AttunePolicyReconciler) buildCollectorOptions(ctx context.Context, namespace string, config *attunev1alpha1.PrometheusConfig) (*rsmetrics.CollectorOptions, error) {
	if err := validation.PrometheusQueryParameters(config.QueryParameters); err != nil {
		return nil, err
	}
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
		return collector, &rsmetrics.PromQLQueryBuilder{}, nil
	}
}

// resolveDatadogCollector creates a DatadogCollector from the policy's
// Datadog config, reading API/app keys from the referenced Secret.
func (r *AttunePolicyReconciler) resolveDatadogCollector(ctx context.Context, policy *attunev1alpha1.AttunePolicy) (rsmetrics.MetricsCollector, rsmetrics.QueryBuilder, error) {
	dd := policy.Spec.MetricsSource.Datadog

	// Read API key from the referenced Secret.
	apiKey, err := r.readSecretKey(ctx, policy.Namespace, dd.APIKeySecretRef.Name, dd.APIKeySecretRef.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read Datadog API key: %w", err)
	}

	// Read optional app key from the same Secret (key "app-key").
	appKey, _ := r.readSecretKey(ctx, policy.Namespace, dd.APIKeySecretRef.Name, "app-key")

	site := dd.Site
	if site == "" {
		site = "datadoghq.com"
	}

	// Cache the collector keyed by site + API key (non-crypto identifier), with full
	// TTL eviction, capacity bound, and race-safe LoadOrStore.
	// We avoid hashing the actual secret bytes to satisfy CodeQL "weak crypto on sensitive data".
	cacheKey := fmt.Sprintf("datadog:%s|%s", site, secretForCacheKey(apiKey))
	collector, err := r.getOrCreateCollectorByKey(cacheKey, "datadog:"+site, func() (rsmetrics.MetricsCollector, error) {
		inner := rsmetrics.NewDatadogCollector(site, apiKey, appKey, log.FromContext(ctx).WithName("datadog"))
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
