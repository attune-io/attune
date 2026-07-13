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

// Package operatormetrics exposes Prometheus metrics from the
// attune operator itself.
package operatormetrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	ResizeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_resize_total",
			Help: "Total number of resize operations performed",
		},
		[]string{"namespace", "workload", "resource", "result"},
	)

	RevertsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_reverts_total",
			Help: "Total number of resize reverts triggered",
		},
		[]string{"namespace", "workload", "reason"},
	)

	ThrottleDeferredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_throttle_deferred_total",
			Help: "Total number of throttle safety checks deferred due to grace period",
		},
		[]string{"namespace", "workload"},
	)

	RecommendationCPU = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_recommendation_cpu_cores",
			Help: "Recommended CPU cores per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	RecommendationMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_recommendation_memory_bytes",
			Help: "Recommended memory bytes per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	SavingsCPU = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_savings_cpu_cores_total",
			Help: "Total CPU cores saved per namespace",
		},
		[]string{"namespace"},
	)

	SavingsMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_savings_memory_bytes_total",
			Help: "Total memory bytes saved per namespace",
		},
		[]string{"namespace"},
	)

	SavingsEstimatedMonthly = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_savings_estimated_monthly_dollars",
			Help: "Estimated monthly cost savings in USD per namespace",
		},
		[]string{"namespace"},
	)

	Confidence = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_confidence",
			Help: "Recommendation confidence score (0-1) per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	ResizeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "attune_resize_duration_seconds",
			Help:    "Duration of individual pod resize operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "workload"},
	)

	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_reconcile_errors_total",
			Help: "Total number of reconciliation errors by type",
		},
		[]string{"error_type"},
	)

	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "attune_reconcile_duration_seconds",
			Help:    "Duration of reconciliation loops",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "namespace", "policy"},
	)

	PrometheusQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "attune_prometheus_query_duration_seconds",
			Help:    "Duration of metrics backend queries (Prometheus, Datadog, or CloudWatch)",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"query_type"},
	)

	PrometheusQueryErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_prometheus_query_errors_total",
			Help: "Total number of metrics backend query errors (Prometheus, Datadog, or CloudWatch)",
		},
		[]string{"namespace", "query_type"},
	)

	WebhookValidationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_webhook_validation_total",
			Help: "Total number of webhook admission decisions",
		},
		[]string{"operation", "result"},
	)

	WebhookDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "attune_webhook_duration_seconds",
			Help:    "Duration of webhook validation and defaulting operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	ScheduleSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_schedule_skipped_total",
			Help: "Total resize cycles skipped due to schedule window constraints",
		},
		[]string{"namespace", "policy"},
	)

	BudgetExhaustedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_budget_exhausted_total",
			Help: "Total resize operations deferred due to per-cycle budget caps",
		},
		[]string{"namespace", "policy"},
	)

	EvictionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_eviction_total",
			Help: "Total eviction attempts (InPlaceOrRecreate fallback)",
		},
		[]string{"namespace", "workload", "result"},
	)

	InfeasibleSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_infeasible_skipped_total",
			Help: "Total Infeasible pods skipped with InPlaceOnly resize method",
		},
		[]string{"namespace", "workload"},
	)

	BurstFactor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "attune_burst_factor",
			Help: "Burst detection multiplier applied to recommendations (1.0 = no burst)",
		},
		[]string{"namespace", "workload", "container", "resource"},
	)

	StartupBoostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_startup_boost_total",
			Help: "Total startup boost lifecycle events (applied, expired, failed)",
		},
		[]string{"namespace", "workload", "action"},
	)

	StaleRecommendationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_stale_recommendations_total",
			Help: "Total times recommendations were marked stale due to Prometheus data gaps",
		},
		[]string{"namespace", "policy"},
	)

	RequestClampedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_request_clamped_total",
			Help: "Total times a recommended request was capped at the container's limit value",
		},
		[]string{"namespace", "policy", "container", "resource"},
	)

	NanInfSamplesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_nan_inf_samples_total",
			Help: "Total times all samples for a container metric were non-finite (NaN or Inf)",
		},
		[]string{"namespace", "policy", "container", "metric_type"},
	)

	RevertFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_revert_failures_total",
			Help: "Total number of failed resize revert attempts",
		},
		[]string{"namespace", "workload", "reason"},
	)

	TemplatePatchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "attune_template_patch_total",
			Help: "Total number of workload pod template resource patches",
		},
		[]string{"namespace", "workload", "result"},
	)
)

// WebhookTimer tracks webhook operation duration and result.
type WebhookTimer struct {
	operation string
	start     time.Time
}

// NewWebhookTimer starts timing a webhook operation.
func NewWebhookTimer(operation string) *WebhookTimer {
	return &WebhookTimer{operation: operation, start: time.Now()}
}

// Observe records the duration.
func (t *WebhookTimer) Observe() {
	WebhookDuration.WithLabelValues(t.operation).Observe(time.Since(t.start).Seconds())
}

// RecordResult increments the validation counter with the appropriate result.
func (t *WebhookTimer) RecordResult(err error) {
	result := "allowed"
	if err != nil {
		result = "rejected"
	}
	WebhookValidationTotal.WithLabelValues(t.operation, result).Inc()
}

func init() {
	metrics.Registry.MustRegister(
		ResizeTotal,
		RevertsTotal,
		ThrottleDeferredTotal,
		RecommendationCPU,
		RecommendationMemory,
		SavingsCPU,
		SavingsMemory,
		SavingsEstimatedMonthly,
		Confidence,
		ResizeDuration,
		ReconcileErrorsTotal,
		ReconcileDuration,
		PrometheusQueryDuration,
		PrometheusQueryErrors,
		WebhookValidationTotal,
		WebhookDuration,
		ScheduleSkippedTotal,
		BudgetExhaustedTotal,
		EvictionTotal,
		InfeasibleSkippedTotal,
		BurstFactor,
		StartupBoostTotal,
		StaleRecommendationsTotal,
		RequestClampedTotal,
		NanInfSamplesTotal,
		RevertFailuresTotal,
		TemplatePatchTotal,
	)
}
