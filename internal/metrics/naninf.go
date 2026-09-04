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
	"context"
	"strings"

	"github.com/attune-io/attune/internal/operatormetrics"
)

type nanInfLabelKey struct{}

type nanInfLabelContext struct {
	Namespace  string
	Policy     string
	MetricType string
}

// WithNanInfLabels attaches policy identity used when QueryRangeGrouped
// increments attune_nan_inf_samples_total for a series whose samples were
// all non-finite (NaN or Inf). Mixed series drop the bad points only.
func WithNanInfLabels(ctx context.Context, namespace, policy, metricType string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nanInfLabelKey{}, nanInfLabelContext{
		Namespace:  namespace,
		Policy:     policy,
		MetricType: metricType,
	})
}

// nanInfUntrackedContainer is the container label for collector-dropped
// NaN/Inf points. Backend series labels are attacker-controlled and must
// not become Prometheus series.
const nanInfUntrackedContainer = "untracked"

// recordDroppedNonFinite increments attune_nan_inf_samples_total once for
// an unusable series (it had points and every one was NaN or Inf). Call
// after the series, never inside the per-point loop.
func recordDroppedNonFinite(ctx context.Context, fallbackMetricType string) {
	ns, policy, metricType := "", "", fallbackMetricType
	if ctx != nil {
		if labels, ok := ctx.Value(nanInfLabelKey{}).(nanInfLabelContext); ok {
			ns, policy = labels.Namespace, labels.Policy
			if labels.MetricType != "" {
				metricType = labels.MetricType
			}
		}
	}
	if metricType == "" {
		metricType = "unknown"
	}
	operatormetrics.NanInfSamplesTotal.WithLabelValues(ns, policy, nanInfUntrackedContainer, metricType).Inc()
}

func fallbackMetricType(query string) string {
	q := strings.ToLower(query)
	if strings.Contains(q, "memory") {
		return "memory"
	}
	if strings.Contains(q, "cpu") {
		return "cpu"
	}
	return "unknown"
}

func metricTypeFromCPU(isCPU bool) string {
	if isCPU {
		return "cpu"
	}
	return "memory"
}
