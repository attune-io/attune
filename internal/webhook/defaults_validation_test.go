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

package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

func TestDefaultsValidator_NoPricing(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
}

func TestDefaultsValidator_ValidPricing(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour:   "0.031",
				MemoryPerGiBHour: "0.004",
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
}

func TestDefaultsValidator_MemoryStartupBoostWarning(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			Memory: &attunev1alpha1.ResourceConfig{
				StartupBoost: &attunev1alpha1.StartupBoost{
					Multiplier: "2.0",
					Duration:   metav1.Duration{Duration: 60000000000}, // 1m
				},
			},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "memory.startupBoost has no effect")
}

func TestDefaultsValidator_QueryStepTooSmall(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep: &metav1.Duration{Duration: 5000000000}, // 5s
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queryStep must be at least 10s")
}

func TestDefaultsValidator_QueryStepTooLarge(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep: &metav1.Duration{Duration: 7200000000000}, // 2h
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queryStep must be at most 1h")
}

func TestDefaultsValidator_QueryStepValid(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				QueryStep: &metav1.Duration{Duration: 60000000000}, // 1m
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
}

func TestDefaultsValidator_InvalidCPUPrice(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour: "banana",
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpuPerCoreHour")
}

func TestDefaultsValidator_NegativeMemoryPrice(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				MemoryPerGiBHour: "-0.5",
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memoryPerGiBHour")
	assert.Contains(t, err.Error(), "positive")
}

func TestDefaultsValidator_Update(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	old := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	updated := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour: "invalid",
			},
		},
	}
	_, err := v.ValidateUpdate(context.Background(), old, updated)
	assert.Error(t, err)
}

func TestDefaultsValidator_Delete(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	_, err := v.ValidateDelete(context.Background(), &attunev1alpha1.AttuneDefaults{})
	require.NoError(t, err)
}

func TestDefaultsValidator_InvalidScheduleTimezone(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					Timezone: "Invalid/Zone",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timezone")
}

func TestDefaultsValidator_InvalidScheduleDayOfWeek(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					DaysOfWeek: []string{"Notaday"},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "daysOfWeek")
}

func TestDefaultsValidator_ValidSchedule(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
					DaysOfWeek: []string{"Monday", "Friday"},
					Timezone:   "UTC",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.NoError(t, err)
}

func TestDefaultsValidator_RecordsMetrics(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()
	operatormetrics.WebhookDuration.Reset()

	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}

	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)

	// Verify validation counter was incremented.
	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("defaults_validate_create", "allowed")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())

	// Verify duration histogram was recorded.
	observer, err := operatormetrics.WebhookDuration.GetMetricWithLabelValues("defaults_validate_create")
	require.NoError(t, err)
	h := observer.(prometheus.Histogram)
	var hMetric io_prometheus_client.Metric
	require.NoError(t, h.Write(&hMetric))
	assert.Equal(t, uint64(1), hMetric.GetHistogram().GetSampleCount())
}

func TestDefaultsValidator_PrometheusQueryParametersReservedRejected(t *testing.T) {
	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address:         "http://prometheus-server.monitoring:80",
					QueryParameters: map[string]string{"step": "30s"},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metricsSource.prometheus.queryParameters")
	assert.Contains(t, err.Error(), "reserved")
}

func TestDefaultsValidator_PrometheusAddressSSRF(t *testing.T) {
	tests := []struct {
		name      string
		address   string
		expectErr bool
	}{
		{"valid cluster address", "http://prometheus-server.monitoring:80", false},
		{"valid private IP", "http://10.0.0.1:9090", false},
		{"loopback IPv4", "http://127.0.0.1:9090", true},
		{"loopback IPv6", "http://[::1]:9090", true},
		{"link-local AWS metadata", "http://169.254.169.254/latest/meta-data/", true},
		{"GCP metadata hostname", "http://metadata.google.internal", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					MetricsSource: &attunev1alpha1.MetricsSource{
						Prometheus: &attunev1alpha1.PrometheusConfig{
							Address: tt.address,
						},
					},
				},
			}
			_, err := v.ValidateCreate(context.Background(), defaults)
			if tt.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "metricsSource.prometheus.address")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDefaultsValidator_RecordsRejectedMetric(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()

	v := &AttuneDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour: "invalid",
			},
		},
	}

	_, err := v.ValidateCreate(context.Background(), defaults)
	require.Error(t, err)

	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("defaults_validate_create", "rejected")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestNamespaceDefaultsValidator_Update(t *testing.T) {
	v := &AttuneNamespaceDefaultsValidator{}
	old := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
	}
	updated := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour: "invalid",
			},
		},
	}
	_, err := v.ValidateUpdate(context.Background(), old, updated)
	assert.Error(t, err)
}

func TestNamespaceDefaultsValidator_Delete(t *testing.T) {
	v := &AttuneNamespaceDefaultsValidator{}
	_, err := v.ValidateDelete(context.Background(), &attunev1alpha1.AttuneNamespaceDefaults{})
	require.NoError(t, err)
}

func TestNamespaceDefaultsValidator_RecordsMetrics(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()
	operatormetrics.WebhookDuration.Reset()

	v := &AttuneNamespaceDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
	}

	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)

	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("namespace_defaults_validate_create", "allowed")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())

	observer, err := operatormetrics.WebhookDuration.GetMetricWithLabelValues("namespace_defaults_validate_create")
	require.NoError(t, err)
	h := observer.(prometheus.Histogram)
	var hMetric io_prometheus_client.Metric
	require.NoError(t, h.Write(&hMetric))
	assert.Equal(t, uint64(1), hMetric.GetHistogram().GetSampleCount())
}

func TestNamespaceDefaultsValidator_InvalidScheduleTimezone(t *testing.T) {
	v := &AttuneNamespaceDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Schedule: &attunev1alpha1.ResizeSchedule{
					Timezone: "Invalid/Zone",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timezone")
}

func TestNamespaceDefaultsValidator_PrometheusQueryParametersReservedRejected(t *testing.T) {
	v := &AttuneNamespaceDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address:         "http://prometheus-server.monitoring:80",
					QueryParameters: map[string]string{"timeout": "5s"},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metricsSource.prometheus.queryParameters")
	assert.Contains(t, err.Error(), "reserved")
}

func TestNamespaceDefaultsValidator_PrometheusAddressSSRF(t *testing.T) {
	v := &AttuneNamespaceDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://127.0.0.1:9090",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metricsSource.prometheus.address")
}

func TestNamespaceDefaultsValidator_RecordsRejectedMetric(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()

	v := &AttuneNamespaceDefaultsValidator{}
	defaults := &attunev1alpha1.AttuneNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "production"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour: "invalid",
			},
		},
	}

	_, err := v.ValidateCreate(context.Background(), defaults)
	require.Error(t, err)

	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("namespace_defaults_validate_create", "rejected")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestDefaultsValidate_OverheadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantErr string
	}{
		{"NaN CPU", "NaN", "30", "must be a finite number"},
		{"negative CPU", "-5", "30", "must be non-negative"},
		{"CPU exceeds max", "1000", "30", "cpu.overhead must be <= 900"},
		{"NaN memory", "20", "NaN", "must be a finite number"},
		{"negative memory", "20", "-1.5", "must be non-negative"},
		{"memory exceeds max", "20", "1000", "memory.overhead must be <= 900"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{
						Percentile: 95,
						Overhead:   tc.cpu,
					},
					Memory: &attunev1alpha1.ResourceConfig{
						Percentile: 99,
						Overhead:   tc.memory,
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefaultsValidate_UnsupportedPercentile(t *testing.T) {
	tests := []struct {
		name       string
		resource   string
		percentile int32
		wantErr    string
	}{
		{"CPU percentile 75", "cpu", 75, "cpu.percentile 75"},
		{"memory percentile 80", "memory", 80, "memory.percentile 80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec:       attunev1alpha1.AttuneDefaultsSpec{},
			}
			rc := &attunev1alpha1.ResourceConfig{Percentile: tc.percentile}
			if tc.resource == "cpu" {
				defaults.Spec.CPU = rc
			} else {
				defaults.Spec.Memory = rc
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefaultsValidate_BoundsMinExceedsMax(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		min      string
		max      string
		wantErr  string
	}{
		{"CPU min > max", "cpu", "2", "1", "cpu.minAllowed"},
		{"memory min > max", "memory", "2Gi", "1Gi", "memory.minAllowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec:       attunev1alpha1.AttuneDefaultsSpec{},
			}
			minQ := resource.MustParse(tc.min)
			maxQ := resource.MustParse(tc.max)
			rc := &attunev1alpha1.ResourceConfig{
				MinAllowed: &minQ,
				MaxAllowed: &maxQ,
			}
			if tc.resource == "cpu" {
				defaults.Spec.CPU = rc
			} else {
				defaults.Spec.Memory = rc
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "must be <=")
		})
	}
}

func TestDefaultsValidate_BurstSensitivityInvalid(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"negative", "-0.1", "non-negative"},
		{"exceeds max", "1.5", "<= 1.0"},
		{"not a number", "abc", "not a valid number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{
						BurstSensitivity: &tc.value,
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefaultsValidate_StartupBoostInvalid(t *testing.T) {
	tests := []struct {
		name       string
		multiplier string
		duration   time.Duration
		wantErr    string
	}{
		{"multiplier exactly 1", "1.0", 30 * time.Second, "must be > 1.0"},
		{"multiplier below 1", "0.5", 30 * time.Second, "must be > 1.0"},
		{"multiplier exceeds max", "11.0", 30 * time.Second, "must be <= 10.0"},
		{"duration too short", "2.0", 5 * time.Second, "at least 10s"},
		{"duration too long", "2.0", 2 * time.Hour, "at most 1h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					CPU: &attunev1alpha1.ResourceConfig{
						StartupBoost: &attunev1alpha1.StartupBoost{
							Multiplier: tc.multiplier,
							Duration:   metav1.Duration{Duration: tc.duration},
						},
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefaultsValidate_CooldownInvalid(t *testing.T) {
	tests := []struct {
		name     string
		cooldown time.Duration
		wantErr  string
	}{
		{"sub-minute cooldown", 30 * time.Second, "cooldown must be at least 1m"},
		{"negative cooldown", -5 * time.Minute, "cooldown must be non-negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					UpdateStrategy: &attunev1alpha1.UpdateStrategy{
						Cooldown: &metav1.Duration{Duration: tc.cooldown},
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDefaultsValidate_HistoryWindowInvalid(t *testing.T) {
	tests := []struct {
		name    string
		window  time.Duration
		wantErr string
	}{
		{"below minimum", 30 * time.Minute, "historyWindow must be at least 1h"},
		{"above maximum", 1000 * time.Hour, "historyWindow must be at most 720h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AttuneDefaultsValidator{}
			defaults := &attunev1alpha1.AttuneDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: attunev1alpha1.AttuneDefaultsSpec{
					MetricsSource: &attunev1alpha1.MetricsSource{
						HistoryWindow: &metav1.Duration{Duration: tc.window},
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), defaults)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
