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

func validPolicy() *attunev1alpha1.AttunePolicy {
	name := "my-app"
	return &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &name,
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
			},
			UpdateStrategy: attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeRecommend,
			},
		},
	}
}

func TestValidate_ValidPolicy(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_CPUBoundsInvalid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	cpuMin := resource.MustParse("2")
	cpuMax := resource.MustParse("1")
	policy.Spec.CPU.MinAllowed = &cpuMin
	policy.Spec.CPU.MaxAllowed = &cpuMax

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.minAllowed")
	assert.Contains(t, err.Error(), "must be <= cpu.maxAllowed")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryBoundsInvalid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	memMin := resource.MustParse("2Gi")
	memMax := resource.MustParse("1Gi")
	policy.Spec.Memory.MinAllowed = &memMin
	policy.Spec.Memory.MaxAllowed = &memMax

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory.minAllowed")
	assert.Contains(t, err.Error(), "must be <= memory.maxAllowed")
	assert.Empty(t, warnings)
}

func TestValidate_CPUBoundsMaxExceedsUpperLimit(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	cpuMin := resource.MustParse("100m")
	cpuMax := resource.MustParse("512")
	policy.Spec.CPU.MinAllowed = &cpuMin
	policy.Spec.CPU.MaxAllowed = &cpuMax

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.maxAllowed")
	assert.Contains(t, err.Error(), "exceeds the maximum allowed value of 256 cores")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryBoundsMaxExceedsUpperLimit(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	memMin := resource.MustParse("64Mi")
	memMax := resource.MustParse("32Ti")
	policy.Spec.Memory.MinAllowed = &memMin
	policy.Spec.Memory.MaxAllowed = &memMax

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "memory.maxAllowed")
	assert.Contains(t, err.Error(), "exceeds the maximum allowed value of 16Ti")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryModeWithoutConfig(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "updateStrategy.canary is required when mode is Canary")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryObservationPeriodNegative(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        10,
		ObservationPeriod: metav1.Duration{Duration: -time.Minute},
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be non-negative")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryObservationPeriodTooShort(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        10,
		ObservationPeriod: metav1.Duration{Duration: 30 * time.Second},
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be at least 1m")
	assert.Empty(t, warnings)
}

func TestValidate_CanaryObservationPeriodZeroWarns(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeCanary
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        10,
		ObservationPeriod: metav1.Duration{Duration: 0},
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "default observation period")
}

func TestValidate_SafetyObservationPeriodTooShort(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 30 * time.Second}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "safetyObservationPeriod must be at least 1m")
	assert.Empty(t, warnings)
}

func TestValidate_SafetyObservationPeriodNegative(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: -1 * time.Second}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "safetyObservationPeriod must be non-negative")
	assert.Empty(t, warnings)
}

func TestValidate_SafetyObservationPeriodValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SafetyObservationPeriod = &metav1.Duration{Duration: 2 * time.Minute}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_NoTargetRef(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = nil

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targetRef must specify either name or selector")
	assert.Empty(t, warnings)
}

func TestValidate_UnsupportedWorkloadKind(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.TargetRef.Kind = "ConfigMap"

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
	assert.Contains(t, err.Error(), "Deployment")
	assert.Empty(t, warnings)
}

func TestValidate_NameAndSelectorBothSet(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	name := "my-app"
	policy.Spec.TargetRef.Name = &name
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "my-app"},
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not both")
	assert.Empty(t, warnings)
}

func TestValidate_EmptySelectorRejected(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targetRef.selector must include at least one")
	assert.Empty(t, warnings)
}

func TestValidate_MemoryDecreaseWarning(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	allowDecrease := true
	policy.Spec.Memory.AllowDecrease = &allowDecrease

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "memory.allowDecrease is enabled")
	assert.Contains(t, warnings[0], "OOMKill risk")
}

func TestValidate_OverheadZeroIsValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.CPU.Overhead = "0"

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_MemoryStartupBoostWarning(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.Memory.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 1 * time.Minute},
	}

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "memory.startupBoost has no effect")
}

func TestValidate_QueryStepTooSmall(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 5 * time.Second}

	_, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queryStep must be at least 10s")
}

func TestValidate_QueryStepTooLarge(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 2 * time.Hour}

	_, err := validator.ValidateCreate(context.Background(), policy)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "queryStep must be at most 1h")
}

func TestValidate_QueryStepValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: 30 * time.Second}

	_, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
}

func TestValidateUpdate_ValidPolicy(t *testing.T) {
	validator := &AttunePolicyValidator{}
	old := validPolicy()
	updated := validPolicy()
	updated.Spec.CPU.Percentile = 90

	warnings, err := validator.ValidateUpdate(context.Background(), old, updated)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateUpdate_InvalidBounds(t *testing.T) {
	validator := &AttunePolicyValidator{}
	old := validPolicy()
	updated := validPolicy()
	cpuMin := resource.MustParse("2")
	cpuMax := resource.MustParse("1")
	updated.Spec.CPU.MinAllowed = &cpuMin
	updated.Spec.CPU.MaxAllowed = &cpuMax

	_, err := validator.ValidateUpdate(context.Background(), old, updated)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpu.minAllowed")
}

func TestValidate_OverheadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantErr string
	}{
		{"non-numeric CPU", "abc", "30", "cpu.overhead"},
		{"non-numeric memory", "20", "xyz", "memory.overhead"},
		{"negative CPU", "-5", "30", "must be non-negative"},
		{"negative memory", "20", "-1.5", "must be non-negative"},
		{"NaN CPU", "NaN", "30", "must be a finite number"},
		{"Inf memory", "20", "Inf", "must be a finite number"},
		{"-Inf CPU", "-Inf", "30", "must be a finite number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.Overhead = tt.cpu
			policy.Spec.Memory.Overhead = tt.memory

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_NegativeCooldown(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: -5 * time.Minute}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cooldown must be non-negative")
}

func TestValidate_SubMinuteCooldownRejected(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: 30 * time.Second}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cooldown must be at least 1m")
}

func TestValidate_NegativeBudgetCaps(t *testing.T) {
	validator := &AttunePolicyValidator{}

	t.Run("negative maxTotalCpuIncrease", func(t *testing.T) {
		policy := validPolicy()
		neg := resource.MustParse("-100m")
		policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &neg
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "maxTotalCpuIncrease must be non-negative")
	})

	t.Run("negative maxTotalMemoryIncrease", func(t *testing.T) {
		policy := validPolicy()
		neg := resource.MustParse("-1Mi")
		policy.Spec.UpdateStrategy.MaxTotalMemoryIncrease = &neg
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "maxTotalMemoryIncrease must be non-negative")
	})

	t.Run("zero budget is valid", func(t *testing.T) {
		policy := validPolicy()
		zero := resource.MustParse("0")
		policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &zero
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})
}

func TestValidate_OverheadExceedsMax(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantErr string
	}{
		{"CPU exceeds max", "1000", "30", "cpu.overhead must be <= 900"},
		{"memory exceeds max", "20", "1000", "memory.overhead must be <= 900"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.Overhead = tt.cpu
			policy.Spec.Memory.Overhead = tt.memory

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
			validator := &AttunePolicyValidator{}
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
			validator := &AttunePolicyValidator{}
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
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: tt.window}

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_HistoryWindowValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 168 * time.Hour} // 7d

	warnings, err := validator.ValidateCreate(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_PrometheusQueryParametersReservedRejected(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address:         "http://prometheus:9090",
		QueryParameters: map[string]string{"query": "up"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metricsSource.prometheus.queryParameters")
	assert.Contains(t, err.Error(), "reserved")
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
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
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

	validator := &AttunePolicyValidator{}
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

	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	cpuMin := resource.MustParse("2")
	cpuMax := resource.MustParse("1")
	policy.Spec.CPU.MinAllowed = &cpuMin
	policy.Spec.CPU.MaxAllowed = &cpuMax

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)

	counter, err := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("validate_create", "rejected")
	require.NoError(t, err)
	var metric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&metric))
	assert.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestValidate_UnsupportedPercentileRejected(t *testing.T) {
	validator := &AttunePolicyValidator{}

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
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Timezone: "Not/A/Timezone",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "schedule.timezone")
	assert.Contains(t, err.Error(), "Not/A/Timezone")
}

func TestValidate_ScheduleTimezoneValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	for _, tz := range []string{"UTC", "America/New_York", "Europe/London", "Asia/Tokyo"} {
		policy := validPolicy()
		policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
			Timezone: tz,
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err, "timezone %q should be valid", tz)
	}
}

func TestValidate_ScheduleDaysOfWeekInvalid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		DaysOfWeek: []string{"Monday", "Notaday"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "daysOfWeek")
	assert.Contains(t, err.Error(), "Notaday")
}

func TestValidate_ScheduleDaysOfWeekCaseInsensitive(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
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
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
				Windows: []attunev1alpha1.TimeWindow{{Start: tt.start, End: tt.end}},
			}

			_, err := validator.ValidateCreate(context.Background(), policy)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_ScheduleTimeWindowValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Windows:    []attunev1alpha1.TimeWindow{{Start: "02:00", End: "06:00"}},
		DaysOfWeek: []string{"Monday", "Wednesday", "Friday"},
		Timezone:   "America/New_York",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_ScheduleOvernightWindowValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Windows: []attunev1alpha1.TimeWindow{{Start: "22:00", End: "06:00"}},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_CPUStartupBoost(t *testing.T) {
	tests := []struct {
		name       string
		multiplier string
		duration   time.Duration
		wantErr    string
	}{
		{name: "valid", multiplier: "2.0", duration: 30 * time.Second},
		{name: "not a number", multiplier: "abc", duration: 30 * time.Second, wantErr: "not a valid number"},
		{name: "NaN", multiplier: "NaN", duration: 30 * time.Second, wantErr: "finite number"},
		{name: "Inf", multiplier: "Inf", duration: 30 * time.Second, wantErr: "finite number"},
		{name: "-Inf", multiplier: "-Inf", duration: 30 * time.Second, wantErr: "finite number"},
		{name: "too low", multiplier: "0.5", duration: 30 * time.Second, wantErr: "must be > 1.0"},
		{name: "exactly 1", multiplier: "1.0", duration: 30 * time.Second, wantErr: "must be > 1.0"},
		{name: "too high", multiplier: "11.0", duration: 30 * time.Second, wantErr: "must be <= 10.0"},
		{name: "duration too short", multiplier: "2.0", duration: 5 * time.Second, wantErr: "at least 10s"},
		{name: "duration too long", multiplier: "2.0", duration: 2 * time.Hour, wantErr: "at most 1h"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			validator := &AttunePolicyValidator{}
			policy := validPolicy()
			policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
				Multiplier: tc.multiplier,
				Duration:   metav1.Duration{Duration: tc.duration},
			}

			_, err := validator.ValidateCreate(context.Background(), policy)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateDelete_AlwaysSucceeds(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateDelete(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidate_BearerTokenSecretCrossNamespace(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "other-namespace/my-secret",
			Key:  "token",
		},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain '/'")
}

func TestValidate_BearerTokenSecretSameNamespace(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "my-secret",
			Key:  "token",
		},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_RateWindowTooSmall(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.RateWindow = &metav1.Duration{Duration: 5 * time.Second}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 30s")
}

func TestValidate_RateWindowExceedsHistoryWindow(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: time.Hour}
	policy.Spec.MetricsSource.RateWindow = &metav1.Duration{Duration: 2 * time.Hour}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not exceed historyWindow")
}

func TestValidate_MultipleMetricsSources(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
	}
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-secret", Key: "api-key"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")
}

func TestValidate_DatadogValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		Site:            "datadoghq.eu",
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-secret", Key: "api-key"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_DatadogInvalidSite(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		Site:            "evil.example.com",
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-secret", Key: "api-key"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a recognized Datadog site")
}

func TestValidate_DatadogMissingSecret(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiKeySecretRef.name is required")
}

func TestValidate_DatadogSecretCrossNamespace(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "other-ns/dd-secret", Key: "api-key"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain '/'")
}

func TestValidate_CloudWatchValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.CloudWatch = &attunev1alpha1.CloudWatchConfig{
		Region:      "us-east-1",
		ClusterName: "my-eks-cluster",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_CloudWatchMissingRegion(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.CloudWatch = &attunev1alpha1.CloudWatchConfig{
		ClusterName: "my-cluster",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "region is required")
}

func TestValidate_CloudWatchMissingCluster(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.CloudWatch = &attunev1alpha1.CloudWatchConfig{
		Region: "us-west-2",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clusterName is required")
}

func TestValidate_AllThreeSourcesSet(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "s", Key: "k"},
	}
	policy.Spec.MetricsSource.CloudWatch = &attunev1alpha1.CloudWatchConfig{
		Region: "us-east-1", ClusterName: "c",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")
}

func TestValidate_NoSourceIsValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	// No metricsSource.prometheus/datadog/cloudwatch set; uses defaults.

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

// ---------- Paused warning ----------

func TestValidate_PausedWithAutoModeWarns(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	paused := true
	policy.Spec.Paused = &paused
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "spec.paused is true")
	assert.Contains(t, w[0], "Auto")
}

func TestValidate_PausedWithRecommendModeNoWarning(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	paused := true
	policy.Spec.Paused = &paused
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	// No warning because Recommend mode doesn't resize anyway.
	for _, warning := range w {
		assert.NotContains(t, warning, "spec.paused")
	}
}

// ---------- SLO Guardrail validation ----------

func TestValidate_SLOGuardrailsValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{
			Name:       "p99-latency",
			Query:      `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="{{ .Namespace }}"}[5m]))`,
			Threshold:  "0.5",
			Comparison: "above",
		},
		{
			Name:             "success-rate",
			Query:            `sum(rate(http_requests_total{code=~"2.."}[5m])) / sum(rate(http_requests_total[5m]))`,
			Threshold:        "0.95",
			Comparison:       "below",
			EvaluationWindow: &metav1.Duration{Duration: 10 * time.Minute},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_SLOGuardrailEmptyName(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "", Query: "up", Threshold: "1"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestValidate_SLOGuardrailDuplicateName(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "latency", Query: "up", Threshold: "1"},
		{Name: "latency", Query: "up", Threshold: "2"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicated")
}

func TestValidate_SLOGuardrailEmptyQuery(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "", Threshold: "1"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query is required")
}

func TestValidate_SLOGuardrailEmptyThreshold(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: ""},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threshold is required")
}

func TestValidate_SLOGuardrailInvalidThreshold(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: "not-a-number"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid number")
}

func TestValidate_SLOGuardrailNaNThreshold(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: "NaN"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finite number")
}

func TestValidate_SLOGuardrailInfThreshold(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: "Inf"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finite number")
}

func TestValidate_SLOGuardrailInvalidComparison(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: "1", Comparison: "equals"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `must be "above" or "below"`)
}

func TestValidate_SLOGuardrailEvalWindowTooShort(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{
			Name:             "test",
			Query:            "up",
			Threshold:        "1",
			EvaluationWindow: &metav1.Duration{Duration: 30 * time.Second},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evaluationWindow must be at least 1m")
}

func TestValidate_SLOGuardrailEvalWindowNegative(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{
			Name:             "test",
			Query:            "up",
			Threshold:        "1",
			EvaluationWindow: &metav1.Duration{Duration: -1 * time.Second},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evaluationWindow must be non-negative")
}

func TestValidate_SLOGuardrailDefaultComparisonValid(t *testing.T) {
	// Empty comparison is valid (defaults to "above" at runtime).
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "test", Query: "up", Threshold: "1", Comparison: ""},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

// ---------- VPA validation ----------

func TestValidate_VPAValid(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{
		Name: "my-vpa",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_VPAWithNamespace(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{
		Name:      "my-vpa",
		Namespace: "monitoring",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
}

func TestValidate_VPAMissingName(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{
		Name: "",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metricsSource.vpa.name is required")
}

func TestValidate_VPAWithPrometheusMutuallyExclusive(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{
		Name: "my-vpa",
	}
	policy.Spec.MetricsSource.Prometheus = &attunev1alpha1.PrometheusConfig{
		Address: "http://prometheus:9090",
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")
}

func TestValidate_VPAWithDatadogMutuallyExclusive(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{
		Name: "my-vpa",
	}
	policy.Spec.MetricsSource.Datadog = &attunev1alpha1.DatadogConfig{
		APIKeySecretRef: attunev1alpha1.SecretKeyRef{Name: "dd-secret", Key: "api-key"},
	}

	_, err := validator.ValidateCreate(context.Background(), policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")
}

// ---------- Ineffective settings warnings ----------

func TestWarn_InitialSizingInRecommendMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	policy.Spec.UpdateStrategy.InitialSizing = boolPtr(true)

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "initialSizing has no effect in Recommend mode; it requires Auto, OneShot, or Canary")
}

func TestWarn_AutoRevertInObserveMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(true)

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "autoRevert has no effect in Observe mode; no resizes occur to revert")
}

func TestWarn_SLOGuardrailsInRecommendMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "latency", Query: "up", Threshold: "1"},
	}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "sloGuardrails have no effect in Recommend mode; no resizes occur to guard")
}

func TestWarn_SLOGuardrailsWithAutoRevertFalse(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.AutoRevert = boolPtr(false)
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "latency", Query: "up", Threshold: "1"},
	}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "sloGuardrails are configured but autoRevert is false; SLO breaches will be detected but not acted on")
}

func TestWarn_SLOGuardrailsWithVPASource(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.SLOGuardrails = []attunev1alpha1.SLOGuardrail{
		{Name: "latency", Query: "up", Threshold: "1"},
	}
	policy.Spec.MetricsSource.Prometheus = nil
	policy.Spec.MetricsSource.VPA = &attunev1alpha1.VPAConfig{Name: "my-vpa"}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "sloGuardrails require a Prometheus-compatible metrics source; VPA source does not support PromQL queries")
}

func TestWarn_CanaryConfigInAutoMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	policy.Spec.UpdateStrategy.Canary = &attunev1alpha1.CanaryConfig{
		Percentage:        10,
		ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute},
	}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "canary configuration has no effect in Auto mode")
}

func TestWarn_ScheduleInObserveMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	policy.Spec.UpdateStrategy.Schedule = &attunev1alpha1.ResizeSchedule{
		Timezone: "UTC",
	}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "schedule has no effect in Observe mode; no resizes occur to schedule")
}

func TestWarn_MaxConcurrentInOneShotMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeOneShot
	policy.Spec.UpdateStrategy.MaxConcurrentResizes = 5

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "maxConcurrentResizes > 1 has no effect in OneShot mode; only one pod is resized per cycle")
}

func TestWarn_MemoryFromCpuRatioOverridesPercentile(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	ratio := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio
	policy.Spec.Memory.Percentile = 99

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "memory.percentile has no effect when memoryFromCpuRatio is set; memory is derived from CPU")
}

func TestWarn_MemoryFromCpuRatioOverridesOverhead(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	ratio2 := "2.0"
	policy.Spec.Memory.MemoryFromCPURatio = &ratio2
	policy.Spec.Memory.Overhead = "30"

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "memory.overhead has no effect when memoryFromCpuRatio is set; memory is derived from CPU")
}

func TestWarn_ExportInObserveMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeObserve
	policy.Spec.UpdateStrategy.Export = &attunev1alpha1.ExportConfig{ConfigMap: true}

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "export.configMap has no effect in Observe mode; recommendations are not surfaced")
}

func TestWarn_BudgetCapsInRecommendMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeRecommend
	cpu := resource.MustParse("2")
	policy.Spec.UpdateStrategy.MaxTotalCPUIncrease = &cpu

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	assert.Contains(t, w, "maxTotalCpuIncrease has no effect in Recommend mode; no resizes occur")
}

func TestWarn_NoWarningsInAutoMode(t *testing.T) {
	validator := &AttunePolicyValidator{}
	policy := validPolicy()
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto

	w, err := validator.ValidateCreate(context.Background(), policy)
	assert.NoError(t, err)
	for _, warning := range w {
		assert.NotContains(t, warning, "has no effect")
	}
}
