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

package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// QueryBuilder creates backend-specific query strings from metric parameters.
// Each implementation produces queries understood by its matching collector:
// PromQL for Prometheus, Datadog query syntax for Datadog, and serialized
// JSON specs for CloudWatch.
type QueryBuilder interface {
	// BuildQuery produces a query string for the given metric type.
	// namespace and podRegex identify the workload. container is empty for
	// pod-level queries. metric is "cpu" or "memory". rateWindow is the
	// window for rate calculations (relevant for CPU).
	BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string
}

// PromQLQueryBuilder builds Prometheus PromQL queries.
type PromQLQueryBuilder struct{}

// BuildQuery produces a PromQL query for the given metric type.
func (b *PromQLQueryBuilder) BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	ns := EscapePromQL(namespace)

	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(`,container="%s"`, EscapePromQL(container))
	}

	rw := FormatPromDuration(rateWindow)

	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s"%s}[%s])`,
			ns, podRegex, containerFilter, rw,
		)
	case "memory":
		return fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s",pod=~"%s"%s}`,
			ns, podRegex, containerFilter,
		)
	default:
		return ""
	}
}

// DatadogQueryBuilder builds Datadog metric query syntax.
type DatadogQueryBuilder struct{}

// BuildQuery produces a Datadog metric query for CPU or memory usage.
// The query uses by {kube_container_name} to group results per container.
func (b *DatadogQueryBuilder) BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	podFilter := datadogPodFilter(podRegex)

	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(",kube_container_name:%s", container)
	}

	rollup := int(rateWindow.Seconds())
	if rollup < 60 {
		rollup = 60
	}

	switch metric {
	case "cpu":
		return fmt.Sprintf(
			`avg:kubernetes.cpu.usage.total{kube_namespace:%s,pod_name:%s%s} by {kube_container_name}.rollup(avg,%d)`,
			namespace, podFilter, containerFilter, rollup,
		)
	case "memory":
		return fmt.Sprintf(
			`avg:kubernetes.memory.working_set{kube_namespace:%s,pod_name:%s%s} by {kube_container_name}.rollup(avg,%d)`,
			namespace, podFilter, containerFilter, rollup,
		)
	default:
		return ""
	}
}

// datadogPodFilter converts a PromQL-style pod regex into a Datadog tag
// filter with glob-style wildcards.
func datadogPodFilter(podRegex string) string {
	prefix := extractLiteralPrefix(podRegex)
	if prefix == "" {
		return "*"
	}
	return prefix + "*"
}

// CloudWatchQuerySpec is the structured query encoded as JSON in the query
// string passed to CloudWatchCollector.
type CloudWatchQuerySpec struct {
	Metric      string `json:"metric"`
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace"`
	PodPrefix   string `json:"podPrefix"`
	Container   string `json:"container,omitempty"`
	Period      int    `json:"period"`
	Stat        string `json:"stat"`
}

// CloudWatchQueryBuilder builds serialized CloudWatch query specifications.
type CloudWatchQueryBuilder struct {
	ClusterName string
}

// BuildQuery produces a JSON-serialized CloudWatchQuerySpec that the
// CloudWatchCollector parses to build GetMetricData requests.
func (b *CloudWatchQueryBuilder) BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	podPrefix := cloudWatchPodPrefix(podRegex)

	var cwMetric string
	switch metric {
	case "cpu":
		cwMetric = "container_cpu_usage_total"
	case "memory":
		cwMetric = "container_memory_working_set"
	default:
		return ""
	}

	period := int(rateWindow.Seconds())
	if period < 60 {
		period = 60
	}
	// Round up to a multiple of 60 (CloudWatch requires it).
	period = ((period + 59) / 60) * 60

	spec := CloudWatchQuerySpec{
		Metric:      cwMetric,
		ClusterName: b.ClusterName,
		Namespace:   namespace,
		PodPrefix:   podPrefix,
		Container:   container,
		Period:      period,
		Stat:        "Average",
	}

	data, _ := json.Marshal(spec)
	return string(data)
}

// cloudWatchPodPrefix extracts a literal prefix from a PromQL-style regex.
func cloudWatchPodPrefix(podRegex string) string {
	return extractLiteralPrefix(podRegex)
}

// extractLiteralPrefix returns the leading literal portion of a regex before
// the first metacharacter. Used by both Datadog and CloudWatch query builders.
func extractLiteralPrefix(regex string) string {
	for i, ch := range regex {
		if strings.ContainsRune(`[]()+*?{}.^$|\`, ch) {
			return regex[:i]
		}
	}
	return regex
}

// FormatPromDuration formats a Go duration as a PromQL duration string.
// PromQL accepts "Nm" for minutes, "Ns" for seconds, "Nh" for hours.
func FormatPromDuration(d time.Duration) string {
	if d <= 0 {
		return "5m"
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
