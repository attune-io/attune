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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

func validPolicy() *rightsizev1alpha1.RightSizePolicy {
	name := "my-app"
	return &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &name,
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeRecommend,
			},
		},
	}
}

func TestValidate_ValidPolicy(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_CPUBoundsInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.CPU.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2"),
		Max: resource.MustParse("1"),
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.bounds.min")
	assert.Contains(t, err.Error(), "must be <= cpu.bounds.max")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryBoundsInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.Memory.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2Gi"),
		Max: resource.MustParse("1Gi"),
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory.bounds.min")
	assert.Contains(t, err.Error(), "must be <= memory.bounds.max")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryModeWithoutConfig(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.UpdateModeCanary
	policy.Spec.UpdateStrategy.Canary = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "updateStrategy.canary is required when mode is Canary")
	assert.Empty(t, warnings)
}

func TestValidate_NoTargetRef(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targetRef must specify either name or selector")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryDecreaseWarning(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	allowDecrease := true
	policy.Spec.Memory.AllowDecrease = &allowDecrease

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "memory.allowDecrease is enabled")
	assert.Contains(t, warnings[0], "OOMKill risk")
}

func TestValidate_SafetyMarginBelowOneWarns(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.CPU.SafetyMargin = "0.8"

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "below 1.0")
	assert.Contains(t, warnings[0], "reduce resources below the target percentile")
}

func TestValidateUpdate_ValidPolicy(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	old := validPolicy()
	updated := validPolicy()
	updated.Spec.CPU.Percentile = 90

	warnings, err := validator.ValidateUpdate(context.Background(), old, updated)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateUpdate_InvalidBounds(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	old := validPolicy()
	updated := validPolicy()
	updated.Spec.CPU.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2"),
		Max: resource.MustParse("1"),
	}

	_, err := validator.ValidateUpdate(context.Background(), old, updated)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.bounds.min")
}

func TestValidate_SafetyMarginInvalid(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantErr string
	}{
		{"non-numeric CPU", "abc", "1.3", "cpu.safetyMargin"},
		{"non-numeric memory", "1.2", "xyz", "memory.safetyMargin"},
		{"zero CPU", "0", "1.3", "must be positive"},
		{"negative memory", "1.2", "-1.5", "must be positive"},
		{"NaN CPU", "NaN", "1.3", "must be a finite number"},
		{"Inf memory", "1.2", "Inf", "must be a finite number"},
		{"-Inf CPU", "-Inf", "1.3", "must be a finite number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.SafetyMargin = tt.cpu
			policy.Spec.Memory.SafetyMargin = tt.memory

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_NegativeCooldown(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: -5 * time.Minute}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cooldown must be non-negative")
}

func TestValidate_SubMinuteCooldownRejected(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 30 * time.Second}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cooldown must be at least 1m")
}

func TestValidate_SafetyMarginExceedsMax(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantErr string
	}{
		{"CPU exceeds max", "15.0", "1.3", "cpu.safetyMargin must be <= 10.0"},
		{"memory exceeds max", "1.2", "100.0", "memory.safetyMargin must be <= 10.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.SafetyMargin = tt.cpu
			policy.Spec.Memory.SafetyMargin = tt.memory

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_BurstSensitivity(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"negative", "-0.1", "non-negative"},
		{"exceeds max", "1.5", "<= 1.0"},
		{"not a number", "abc", "not a valid number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.BurstSensitivity = &tt.value

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_BurstSensitivityValid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"zero", "0"},
		{"default", "0.1"},
		{"max", "1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.BurstSensitivity = &tt.value

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.NoError(t, err)
		})
	}
}

func TestValidate_HistoryWindowBounds(t *testing.T) {
	tests := []struct {
		name    string
		window  time.Duration
		wantErr string
	}{
		{"below minimum", 30 * time.Minute, "historyWindow must be at least 1h"},
		{"above maximum", 1000 * time.Hour, "historyWindow must be at most 720h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: tt.window}

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_HistoryWindowValid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 168 * time.Hour} // 7d

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_PrometheusAddressValid(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"valid http", "http://prometheus:9090", false},
		{"valid https", "https://prometheus.example.com", false},
		{"valid with path", "http://prometheus:9090/api/v1", false},
		{"invalid scheme", "ftp://prometheus:9090", true},
		{"invalid scheme file", "file:///etc/passwd", true},
		{"missing scheme", "prometheus:9090", true},
		{"empty host", "http://", true},
		{"invalid URL", "://bad", true},
		// SSRF protection: loopback and link-local IPs
		{"loopback IPv4", "http://127.0.0.1:9090", true},
		{"loopback IPv6", "http://[::1]:9090", true},
		// Private IPs are allowed (Prometheus typically runs on ClusterIP)
		{"private 10.x allowed", "http://10.0.0.1:9090", false},
		{"private 192.168.x allowed", "http://192.168.1.1:9090", false},
		{"private 172.16.x allowed", "http://172.16.0.1:9090", false},
		{"link-local AWS metadata", "http://169.254.169.254/latest/meta-data/", true},
		// SSRF protection: cloud metadata hostnames
		{"GCP metadata hostname", "http://metadata.google.internal", true},
		{"metadata.internal", "http://metadata.internal", true},
		{"AWS EC2 internal hostname", "http://instance-data.ec2.internal", true},
		{"AWS IPv6 metadata", "http://[fd00:ec2::254]/latest/meta-data/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.MetricsSource.Prometheus = &rightsizev1alpha1.PrometheusConfig{
				Address: tt.address,
			}

			_, err := validator.ValidateCreate(context.Background(), policy)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "prometheus.address")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateCreate_RecordsWebhookMetrics(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()
	operatormetrics.WebhookDuration.Reset()

	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.NoError(t, err)

	// Verify validation counter was incremented with "allowed".
	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("validate_create", "allowed")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())

	// Verify duration histogram was recorded.
	observer, err := operatormetrics.WebhookDuration.GetMetricWithLabelValues("validate_create")
	require.NoError(t, err)
	h := observer.(prometheus.Histogram)
	var hMetric io_prometheus_client.Metric
	require.NoError(t, h.Write(&hMetric))
	assert.Equal(t, uint64(1), hMetric.GetHistogram().GetSampleCount())
}

func TestValidateCreate_RecordsRejectedMetric(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()

	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.CPU.Bounds = &rightsizev1alpha1.ResourceBounds{
		Min: resource.MustParse("2"),
		Max: resource.MustParse("1"),
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)

	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("validate_create", "rejected")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestValidate_UnsupportedPercentileRejected(t *testing.T) {
	validator := &RightSizePolicyValidator{}

	// CPU percentile 75 is not in {50, 90, 95, 99}.
	policy := validPolicy()
	policy.Spec.CPU.Percentile = 75
	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.percentile 75")

	// Memory percentile 80 is not in {50, 90, 95, 99}.
	policy2 := validPolicy()
	policy2.Spec.Memory.Percentile = 80
	_, err2 := validator.ValidateCreate(context.Background(), policy2)
	assert.Error(t, err2)
	assert.Contains(t, err2.Error(), "memory.percentile 80")

	// Supported values should pass.
	for _, p := range []int32{50, 90, 95, 99} {
		policy3 := validPolicy()
		policy3.Spec.CPU.Percentile = p
		policy3.Spec.Memory.Percentile = p
		_, err3 := validator.ValidateCreate(context.Background(), policy3)
		assert.NoError(t, err3, "percentile %d should be accepted", p)
	}
}

func TestValidate_ScheduleTimezoneInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		Timezone: "Not/A/Timezone",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "schedule.timezone")
	assert.Contains(t, err.Error(), "Not/A/Timezone")
}

func TestValidate_ScheduleTimezoneValid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	for _, tz := range []string{"UTC", "America/New_York", "Europe/London", "Asia/Tokyo"} {
		policy := validPolicy()
		policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
			Timezone: tz,
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err, "timezone %q should be valid", tz)
	}
}

func TestValidate_ScheduleDaysOfWeekInvalid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"Monday", "Notaday"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "daysOfWeek")
	assert.Contains(t, err.Error(), "Notaday")
}

func TestValidate_ScheduleDaysOfWeekCaseInsensitive(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"monday", "FRIDAY", "Saturday"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_ScheduleTimeWindowInvalid(t *testing.T) {
	tests := []struct {
		name    string
		start   string
		end     string
		wantErr string
	}{
		{"bad start format", "2:00", "06:00", "HH:MM format"},
		{"bad end format", "02:00", "6:00", "HH:MM format"},
		{"hour out of range", "25:00", "06:00", "not a valid time"},
		{"minute out of range", "02:60", "06:00", "not a valid time"},
		{"letters", "ab:cd", "06:00", "not a valid time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &RightSizePolicyValidator{}
			policy := validPolicy()
			policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
				Windows: []rightsizev1alpha1.TimeWindow{{Start: tt.start, End: tt.end}},
			}

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_ScheduleTimeWindowValid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		Windows:    []rightsizev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
		DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
		Timezone:   "America/New_York",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_ScheduleOvernightWindowValid(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &rightsizev1alpha1.ResizeSchedule{
		Windows: []rightsizev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidateDelete_AlwaysSucceeds(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateDelete(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}
