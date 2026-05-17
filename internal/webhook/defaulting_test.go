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

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

func TestDefault_DoesNotPreFillResourceDefaults(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Zero(t, policy.Spec.CPU.Percentile)
	assert.Empty(t, policy.Spec.CPU.SafetyMargin)
	assert.Zero(t, policy.Spec.Memory.Percentile)
	assert.Empty(t, policy.Spec.Memory.SafetyMargin)
}

func TestDefault_PreservesExisting(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}
	policy.Spec.CPU.Percentile = 90
	policy.Spec.CPU.SafetyMargin = "1.5"

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
	// Unset resource fields should remain available for defaults resources to supply.
	assert.Zero(t, policy.Spec.Memory.Percentile)
	assert.Empty(t, policy.Spec.Memory.SafetyMargin)
}

func TestDefault_DoesNotSetMode(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	// Mode is NOT set by the webhook; it's deferred to the controller's
	// applyBuiltInDefaults so that RightSizeDefaults can override it.
	assert.Empty(t, policy.Spec.UpdateStrategy.Mode)
	assert.Nil(t, policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	assert.Nil(t, policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
}

func TestDefault_SetsWeight(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	assert.NoError(t, err)
	assert.Equal(t, int32(100), policy.Spec.Weight)
}

func TestDefault_DoesNotSetControllerDefaultedFields(t *testing.T) {
	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)

	require.NoError(t, err)
	// All these fields are deferred to the controller's applyBuiltInDefaults
	// so that RightSizeDefaults/RightSizeNamespaceDefaults can override them.
	assert.Nil(t, policy.Spec.CPU.ControlledValues)
	assert.Nil(t, policy.Spec.Memory.ControlledValues)
	assert.Nil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Nil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Nil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.Nil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Empty(t, policy.Spec.UpdateStrategy.ResizeMethod)
}

func TestDefault_RecordsWebhookMetrics(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()
	operatormetrics.WebhookDuration.Reset()

	defaulter := &RightSizePolicyDefaulter{}
	policy := &rightsizev1alpha1.RightSizePolicy{}

	err := defaulter.Default(context.Background(), policy)
	require.NoError(t, err)

	// Verify duration histogram was recorded.
	var metric io_prometheus_client.Metric
	observer, err := operatormetrics.WebhookDuration.GetMetricWithLabelValues("defaulting")
	require.NoError(t, err)
	h := observer.(prometheus.Histogram)
	require.NoError(t, h.Write(&metric))
	assert.Equal(t, uint64(1), metric.GetHistogram().GetSampleCount(),
		"defaulting should record one duration observation")

	// Verify validation counter was incremented with "allowed".
	counter, cErr := operatormetrics.WebhookValidationTotal.GetMetricWithLabelValues("defaulting", "allowed")
	require.NoError(t, cErr)
	var cMetric io_prometheus_client.Metric
	require.NoError(t, counter.Write(&cMetric))
	assert.Equal(t, 1.0, cMetric.GetCounter().GetValue(),
		"defaulting should record one validation_total with result=allowed")
}
