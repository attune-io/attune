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
	"regexp"
	"strings"
	"time"
)

// recordingMetricNameRE matches Prometheus metric name grammar (no selectors).
var recordingMetricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// ValidRecordingMetricName reports whether name is a safe PromQL metric identifier.
func ValidRecordingMetricName(name string) bool {
	return name != "" && recordingMetricNameRE.MatchString(name)
}

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

// PodAggregationMode controls how multi-pod series are reduced in PromQL.
// Empty defaults to Max (size for the busiest pod; cheapest for high replica counts).
type PodAggregationMode string

const (
	// PodAggregationMax takes max by (container) across pods (default).
	PodAggregationMax PodAggregationMode = "Max"
	// PodAggregationAvg takes avg by (container) across pods.
	PodAggregationAvg PodAggregationMode = "Avg"
	// PodAggregationNone leaves one series per pod (legacy; expensive at scale).
	PodAggregationNone PodAggregationMode = "None"
)

// PromQLQueryBuilder builds Prometheus PromQL queries.
type PromQLQueryBuilder struct {
	// Aggregation reduces multi-pod series server-side. Empty means Max.
	Aggregation PodAggregationMode
	// CPUMetric overrides the default rate(container_cpu_usage_seconds_total)
	// expression. Use a pre-aggregated recording rule metric name (labels:
	// namespace, pod, container). When set, rate() is not applied.
	CPUMetric string
	// MemoryMetric overrides container_memory_working_set_bytes. Use a
	// recording rule with the same label set as cadvisor memory metrics.
	MemoryMetric string
}

// BuildQuery produces a PromQL query for the given metric type.
func (b *PromQLQueryBuilder) BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
	ns := EscapePromQL(namespace)
	podRE := podRegex

	containerFilter := ""
	if container != "" {
		containerFilter = fmt.Sprintf(`,container="%s"`, EscapePromQL(container))
	}

	rw := FormatPromDuration(rateWindow)

	if b.CPUMetric != "" && !ValidRecordingMetricName(b.CPUMetric) {
		return ""
	}
	if b.MemoryMetric != "" && !ValidRecordingMetricName(b.MemoryMetric) {
		return ""
	}

	var inner string
	switch metric {
	case "cpu":
		if b.CPUMetric != "" {
			inner = fmt.Sprintf(
				`%s{namespace="%s",pod=~"%s"%s}`,
				b.CPUMetric, ns, podRE, containerFilter,
			)
		} else {
			inner = fmt.Sprintf(
				`rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s"%s}[%s])`,
				ns, podRE, containerFilter, rw,
			)
		}
	case "memory":
		memMetric := "container_memory_working_set_bytes"
		if b.MemoryMetric != "" {
			memMetric = b.MemoryMetric
		}
		inner = fmt.Sprintf(
			`%s{namespace="%s",pod=~"%s"%s}`,
			memMetric, ns, podRE, containerFilter,
		)
	default:
		return ""
	}

	return applyPodAggregation(inner, b.Aggregation)
}

// applyPodAggregation wraps a vector/matrix selector with max/avg by (container).
func applyPodAggregation(inner string, mode PodAggregationMode) string {
	switch mode {
	case PodAggregationNone:
		return inner
	case PodAggregationAvg:
		return fmt.Sprintf(`avg by (container) (%s)`, inner)
	case PodAggregationMax, "":
		// Default: Max. Rightsizing for the busiest pod; O(containers) series.
		return fmt.Sprintf(`max by (container) (%s)`, inner)
	default:
		return fmt.Sprintf(`max by (container) (%s)`, inner)
	}
}

// DatadogQueryBuilder builds Datadog metric query syntax.
type DatadogQueryBuilder struct{}

// BuildQuery produces a Datadog metric query for CPU or memory usage.
// Series are grouped by container and pod name so the collector can drop
// pods the tag glob over-matched.
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
			`avg:kubernetes.cpu.usage.total{kube_namespace:%s,%s%s} by {kube_container_name,pod_name}.rollup(avg,%d)%s%s`,
			namespace, podFilter, containerFilter, rollup, datadogRegexMarker, podRegex,
		)
	case "memory":
		return fmt.Sprintf(
			`avg:kubernetes.memory.working_set{kube_namespace:%s,%s%s} by {kube_container_name,pod_name}.rollup(avg,%d)%s%s`,
			namespace, podFilter, containerFilter, rollup, datadogRegexMarker, podRegex,
		)
	default:
		return ""
	}
}

// datadogRegexMarker separates the Datadog query from the PromQL pod regex
// the collector uses to drop series the glob over-matched. It is stripped
// before the HTTP call.
const datadogRegexMarker = "\n#attune-pod-regex:"

func splitDatadogQuery(query string) (ddQuery, podRegex string) {
	if i := strings.LastIndex(query, datadogRegexMarker); i >= 0 {
		return query[:i], query[i+len(datadogRegexMarker):]
	}
	return query, ""
}

// datadogPodFilter converts a PromQL-style pod regex into a Datadog tag
// clause. The clause is a superset of the regex: literal alternations are
// listed in full, and other patterns keep an escaped literal prefix plus
// a glob. Callers still drop series that fail the original regex.
func datadogPodFilter(podRegex string) string {
	if podRegex == "" {
		return "pod_name:*"
	}
	// Controller regexes are PromQL-string-escaped (`my\\.app`). Interpret
	// the regex Prometheus evaluates, not the escaped query text.
	podRegex = unescapePromQLRegex(podRegex)
	alts := splitTopLevelAlt(podRegex)
	globs := make([]string, 0, len(alts))
	for _, alt := range alts {
		if alt == "" {
			return "pod_name:*"
		}
		if lit, ok := unescapeLiteralRegex(alt); ok {
			globs = append(globs, "pod_name:"+lit)
			continue
		}
		prefix := literalRegexPrefix(alt)
		if prefix == "" {
			return "pod_name:*"
		}
		globs = append(globs, "pod_name:"+prefix+"*")
	}
	if len(globs) == 1 {
		return globs[0]
	}
	return "(" + strings.Join(globs, " OR ") + ")"
}

// CloudWatchQuerySpec is the structured query encoded as JSON in the query
// string passed to CloudWatchCollector.
type CloudWatchQuerySpec struct {
	Metric      string `json:"metric"`
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace"`
	// PodPrefix is a legacy client-side name prefix. PodRegex, when set,
	// is the PromQL pod regex and replaces the prefix check.
	PodPrefix string `json:"podPrefix,omitempty"`
	PodRegex  string `json:"podRegex,omitempty"`
	Container string `json:"container,omitempty"`
	Period    int    `json:"period"`
	Stat      string `json:"stat"`
}

// CloudWatchQueryBuilder builds serialized CloudWatch query specifications.
type CloudWatchQueryBuilder struct {
	ClusterName string
}

// BuildQuery produces a JSON-serialized CloudWatchQuerySpec that the
// CloudWatchCollector parses to build GetMetricData requests.
func (b *CloudWatchQueryBuilder) BuildQuery(namespace, podRegex, container, metric string, rateWindow time.Duration) string {
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
		PodRegex:    podRegex,
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

// literalRegexPrefix returns the leading literal text of a regex alternative.
// A backslash escapes the next byte, so `my\.app-` yields `my.app-` instead
// of stopping at the escape.
func literalRegexPrefix(regex string) string {
	var b strings.Builder
	for i := 0; i < len(regex); i++ {
		c := regex[i]
		if c == '\\' && i+1 < len(regex) {
			b.WriteByte(regex[i+1])
			i++
			continue
		}
		if strings.ContainsAny(string(c), `[]()+*?{}.^$|`) {
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

// unescapeLiteralRegex reports whether regex is only escaped literals.
func unescapeLiteralRegex(regex string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(regex); i++ {
		c := regex[i]
		if c == '\\' && i+1 < len(regex) {
			b.WriteByte(regex[i+1])
			i++
			continue
		}
		if strings.ContainsAny(string(c), `[]()+*?{}.^$|`) {
			return "", false
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// splitTopLevelAlt splits a regex on unescaped `|`.
func splitTopLevelAlt(regex string) []string {
	var parts []string
	start := 0
	escaped := false
	for i := 0; i < len(regex); i++ {
		if escaped {
			escaped = false
			continue
		}
		if regex[i] == '\\' {
			escaped = true
			continue
		}
		if regex[i] == '|' {
			parts = append(parts, regex[start:i])
			start = i + 1
		}
	}
	return append(parts, regex[start:])
}

// podNameMatches reports whether name matches the PromQL pod regex.
// An empty regex matches every name. An invalid regex matches nothing.
// unescapePromQLRegex undoes PromQL double-quoted string escapes so the
// result is the regex text Prometheus compiles.
func unescapePromQLRegex(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
			i++
		case '"':
			b.WriteByte('"')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func podNameMatches(podRegex, name string) bool {
	if podRegex == "" {
		return true
	}
	re, err := regexp.Compile("^(?:" + unescapePromQLRegex(podRegex) + ")$")
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// cloudWatchPodNameMatches accepts a real pod name or the controller name
// Container Insights writes to the PodName dimension. The default receiver
// sets PodName from the owner (ReplicaSet, DaemonSet, StatefulSet, or Job)
// and leaves FullPodName off. prefer_full_pod_name publishes the pod name,
// which the workload regex already matches.
func cloudWatchPodNameMatches(podRegex, name string) bool {
	if podNameMatches(podRegex, name) {
		return true
	}
	ctrl := cloudWatchControllerRegex(podRegex)
	if ctrl == "" {
		return false
	}
	return podNameMatches(ctrl, name)
}

// cloudWatchControllerRegex strips the pod-name suffix from each alternative
// so the result matches the owner name Container Insights publishes.
func cloudWatchControllerRegex(podRegex string) string {
	parts := splitTopLevelAlt(podRegex)
	out := make([]string, 0, len(parts))
	for _, alt := range parts {
		if c := cloudWatchControllerAlt(alt); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "|")
}

func cloudWatchControllerAlt(alt string) string {
	const (
		podHash = `-[a-z0-9]{5}`
		index   = `-[0-9]+`
		stamp   = `-[0-9]{10}`
		rsHash  = `-[a-z0-9]+`
	)
	switch {
	case strings.HasSuffix(alt, stamp+index+podHash):
		return strings.TrimSuffix(alt, index+podHash)
	case strings.HasSuffix(alt, stamp+podHash):
		return strings.TrimSuffix(alt, podHash)
	case strings.HasSuffix(alt, index+podHash):
		return strings.TrimSuffix(alt, index+podHash)
	case strings.HasSuffix(alt, rsHash+podHash):
		return strings.TrimSuffix(alt, podHash)
	case strings.HasSuffix(alt, podHash):
		return strings.TrimSuffix(alt, podHash)
	case strings.HasSuffix(alt, index):
		return strings.TrimSuffix(alt, index)
	default:
		return cloudWatchLiteralController(alt)
	}
}

// cloudWatchLiteralController reduces one escaped pod name to its owner.
// A 5-character pod hash is dropped. A following short index is dropped for
// indexed Jobs. A 10-digit CronJob stamp is kept, because that Job name is
// the PodName Container Insights publishes. A trailing ordinal is the
// StatefulSet case.
func cloudWatchLiteralController(alt string) string {
	lit, ok := unescapeLiteralRegex(alt)
	if !ok || lit == "" {
		return ""
	}
	name := lit
	if i := strings.LastIndex(name, "-"); i > 0 && podHashToken(name[i+1:]) {
		name = name[:i]
		if j := strings.LastIndex(name, "-"); j > 0 && shortIndex(name[j+1:]) {
			name = name[:j]
		}
	} else if i := strings.LastIndex(name, "-"); i > 0 && shortIndex(name[i+1:]) {
		name = name[:i]
	} else {
		return ""
	}
	if name == "" || name == lit {
		return ""
	}
	return regexp.QuoteMeta(name)
}

func podHashToken(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func shortIndex(s string) bool {
	if s == "" || len(s) >= 10 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// extractLiteralPrefix returns the leading literal portion of a regex before
// the first metacharacter. Used by CloudWatch when only a prefix is available.
func extractLiteralPrefix(regex string) string {
	return literalRegexPrefix(regex)
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
