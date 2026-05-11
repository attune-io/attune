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

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
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
				Mode: "Recommend",
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
	policy.Spec.UpdateStrategy.Mode = "Canary"
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
		// SSRF protection: private/loopback IPs
		{"loopback IPv4", "http://127.0.0.1:9090", true},
		{"loopback IPv6", "http://[::1]:9090", true},
		{"private 10.x", "http://10.0.0.1:9090", true},
		{"private 192.168.x", "http://192.168.1.1:9090", true},
		{"private 172.16.x", "http://172.16.0.1:9090", true},
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

func TestValidateDelete_AlwaysSucceeds(t *testing.T) {
	validator := &RightSizePolicyValidator{}
	policy := validPolicy()

	warnings, err := validator.ValidateDelete(context.Background(), policy)

	assert.NoError(t, err)
	assert.Empty(t, warnings)
}
