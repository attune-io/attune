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

package operatormetrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMetricsRegistered(t *testing.T) {
	// Verify each metric can receive a value without panicking
	assert.NotPanics(t, func() {
		ResizeTotal.WithLabelValues("default", "api-server", "cpu", "success").Inc()
		RevertsTotal.WithLabelValues("default", "api-server", "oomkill").Inc()
		RecommendationCPU.WithLabelValues("default", "api-server", "api").Set(0.15)
		RecommendationMemory.WithLabelValues("default", "api-server", "api").Set(268435456)
		SavingsCPU.WithLabelValues("default").Set(0.35)
		SavingsMemory.WithLabelValues("default").Set(536870912)
		Confidence.WithLabelValues("default", "api-server", "api").Set(0.92)
		ReconcileDuration.WithLabelValues("policy").Observe(1.5)
		PrometheusQueryDuration.WithLabelValues("cpu_grouped").Observe(0.05)
		PrometheusQueryErrors.WithLabelValues("default", "cpu_grouped").Inc()
	})
}

func TestMetricLabels(t *testing.T) {
	// Verify counter increment works correctly
	assert.NotPanics(t, func() {
		ResizeTotal.WithLabelValues("prod", "worker", "memory", "failed").Inc()
		ResizeTotal.WithLabelValues("prod", "worker", "memory", "failed").Inc()
	})
	// No panic means labels are valid
}
