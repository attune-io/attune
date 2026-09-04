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
// increments attune_nan_inf_samples_total for a dropped NaN/Inf point.
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

func recordDroppedNonFinite(ctx context.Context, container, fallbackMetricType string) {
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
	operatormetrics.NanInfSamplesTotal.WithLabelValues(ns, policy, container, metricType).Inc()
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
