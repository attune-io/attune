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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	"github.com/SebTardifLabs/kube-rightsize/internal/operatormetrics"
)

func TestDefaultsValidator_NoPricing(t *testing.T) {
	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
}

func TestDefaultsValidator_ValidPricing(t *testing.T) {
	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
				CPUPerCoreHour:   "0.031",
				MemoryPerGiBHour: "0.004",
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	require.NoError(t, err)
}

func TestDefaultsValidator_InvalidCPUPrice(t *testing.T) {
	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
				CPUPerCoreHour: "banana",
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cpuPerCoreHour")
}

func TestDefaultsValidator_NegativeMemoryPrice(t *testing.T) {
	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
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
	v := &RightSizeDefaultsValidator{}
	old := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	updated := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
				CPUPerCoreHour: "invalid",
			},
		},
	}
	_, err := v.ValidateUpdate(context.Background(), old, updated)
	assert.Error(t, err)
}

func TestDefaultsValidator_Delete(t *testing.T) {
	v := &RightSizeDefaultsValidator{}
	_, err := v.ValidateDelete(context.Background(), &rightsizev1alpha1.RightSizeDefaults{})
	require.NoError(t, err)
}

func TestDefaultsValidator_RecordsMetrics(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()
	operatormetrics.WebhookDuration.Reset()

	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
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

func TestDefaultsValidator_RecordsRejectedMetric(t *testing.T) {
	operatormetrics.WebhookValidationTotal.Reset()

	v := &RightSizeDefaultsValidator{}
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CostPricing: &rightsizev1alpha1.CostPricing{
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
