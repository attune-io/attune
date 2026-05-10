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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
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
