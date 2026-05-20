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
	"fmt"
	"io"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	rsmetrics "github.com/SebTardifLabs/kube-rightsize/internal/metrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
	"github.com/SebTardifLabs/kube-rightsize/internal/recommendation"
	"github.com/SebTardifLabs/kube-rightsize/internal/validation"
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
		return nil, fmt.Errorf("collector cache full (%d entries); refusing new Prometheus address %q; consolidate policies to use fewer distinct Prometheus addresses, or use a RightSizeDefaults resource to share a single address across all policies", maxCollectors, config.Address)
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
	podRegex := r.getPodRegex(workload)

	queryStep := r.getQueryStep(policy)
	if queryStep != defaultPrometheusStep {
		logger.V(1).Info("Using custom query step", "queryStep", queryStep)
	}
	// Run CPU and memory queries concurrently. They are independent PromQL
	// expressions against the same Prometheus instance. The rate limiter
	// provides backpressure, so concurrent queries are safe.
	var cpuSamplesByContainer, memSamplesByContainer map[string][]rsmetrics.Sample
	var cpuErr, memErr bool
	var qg errgroup.Group
	qg.Go(func() error {
		cpuSamplesByContainer, cpuErr = queryMetricsGrouped(ctx, collector, policy.Namespace, podRegex, "cpu", start, now, queryStep)
		return nil
	})
	qg.Go(func() error {
		memSamplesByContainer, memErr = queryMetricsGrouped(ctx, collector, policy.Namespace, podRegex, "memory", start, now, queryStep)
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
		if memProfile.DataPoints >= int(minimumDataPoints) {
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

func (r *RightSizePolicyReconciler) cacheDiscoveredPrometheus(addr string) {
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
