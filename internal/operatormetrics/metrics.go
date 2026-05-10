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
// kube-rightsize operator itself.
package operatormetrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	ResizeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_rightsize_resize_total",
			Help: "Total number of resize operations performed",
		},
		[]string{"namespace", "workload", "resource", "result"},
	)

	RevertsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_rightsize_reverts_total",
			Help: "Total number of resize reverts triggered",
		},
		[]string{"namespace", "workload", "reason"},
	)

	RecommendationCPU = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_recommendation_cpu_cores",
			Help: "Recommended CPU cores per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	RecommendationMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_recommendation_memory_bytes",
			Help: "Recommended memory bytes per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	SavingsCPU = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_savings_cpu_cores_total",
			Help: "Total CPU cores saved per namespace",
		},
		[]string{"namespace"},
	)

	SavingsMemory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_savings_memory_bytes_total",
			Help: "Total memory bytes saved per namespace",
		},
		[]string{"namespace"},
	)

	SavingsEstimatedMonthly = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_savings_estimated_monthly_dollars",
			Help: "Estimated monthly cost savings in USD per namespace",
		},
		[]string{"namespace"},
	)

	Confidence = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_rightsize_confidence",
			Help: "Recommendation confidence score (0-1) per workload/container",
		},
		[]string{"namespace", "workload", "container"},
	)

	ResizeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kube_rightsize_resize_duration_seconds",
			Help:    "Duration of individual pod resize operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "workload"},
	)

	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_rightsize_reconcile_errors_total",
			Help: "Total number of reconciliation errors by type",
		},
		[]string{"error_type"},
	)

	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kube_rightsize_reconcile_duration_seconds",
			Help:    "Duration of reconciliation loops",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller"},
	)

	PrometheusQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kube_rightsize_prometheus_query_duration_seconds",
			Help:    "Duration of Prometheus queries",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)

	PrometheusQueryErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kube_rightsize_prometheus_query_errors_total",
			Help: "Total number of Prometheus query errors",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ResizeTotal,
		RevertsTotal,
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
	)
}
