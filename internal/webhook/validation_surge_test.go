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
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestValidate_Surge(t *testing.T) {
	validator := &AttunePolicyValidator{}

	t.Run("omitted surge stays valid", func(t *testing.T) {
		_, err := validator.ValidateCreate(context.Background(), validPolicy())
		assert.NoError(t, err)
	})

	t.Run("empty block is valid on cpu and memory", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{}
		policy.Spec.Memory.Surge = &attunev1alpha1.Surge{}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	t.Run("window equal to the default history is accepted", func(t *testing.T) {
		policy := validPolicy()
		pct := int32(50)
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{
			TriggerRatio: "100",
			Percentile:   &pct,
			Window:       &metav1.Duration{Duration: 168 * time.Hour},
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	t.Run("window equal to a configured history is accepted", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 2 * time.Hour}
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{
			Window: &metav1.Duration{Duration: 2 * time.Hour},
		}
		policy.Spec.Memory.Surge = &attunev1alpha1.Surge{
			TriggerRatio: "1.5",
			Window:       &metav1.Duration{Duration: 2 * time.Hour},
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	cases := []struct {
		name    string
		mutate  func(*attunev1alpha1.AttunePolicy)
		wantErr string
	}{
		{
			name: "trigger ratio 1",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "1"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "0"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio NaN",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{TriggerRatio: "NaN"}
			},
			wantErr: "memory.surge.triggerRatio must be a finite number",
		},
		{
			name: "trigger ratio Inf",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "Inf"}
			},
			wantErr: "cpu.surge.triggerRatio must be a finite number",
		},
		{
			name: "trigger ratio negative",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "-1"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio above 100",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "100.1"}
			},
			wantErr: "cpu.surge.triggerRatio must be <= 100",
		},
		{
			name: "percentile 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				zero := int32(0)
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Percentile: &zero}
			},
			wantErr: "cpu.surge.percentile 0",
		},
		{
			name: "percentile 75",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				bad := int32(75)
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{Percentile: &bad}
			},
			wantErr: "memory.surge.percentile 75",
		},
		{
			name: "window 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{}}
			},
			wantErr: "cpu.surge.window must be at least 5m, or omit it",
		},
		{
			name: "window below 5m",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 4 * time.Minute}}
			},
			wantErr: "cpu.surge.window must be at least 5m",
		},
		{
			name: "window longer than default history",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 168*time.Hour + time.Minute}}
			},
			wantErr: "must not exceed historyWindow",
		},
		{
			name: "window longer than configured history",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 2 * time.Hour}
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 2*time.Hour + time.Minute}}
			},
			wantErr: "memory.surge.window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validPolicy()
			tc.mutate(policy)
			_, err := validator.ValidateCreate(context.Background(), policy)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateDefaults_Surge(t *testing.T) {
	t.Run("empty block is valid", func(t *testing.T) {
		_, err := validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{}},
		})
		assert.NoError(t, err)
	})

	t.Run("rejects the same edges as a policy", func(t *testing.T) {
		zero := int32(0)
		_, err := validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			Memory: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{TriggerRatio: "NaN", Percentile: &zero},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "memory.surge.triggerRatio")

		_, err = validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				HistoryWindow: &metav1.Duration{Duration: time.Hour},
			},
			CPU: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 2 * time.Hour}},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cpu.surge.window")
		assert.Contains(t, err.Error(), "must not exceed historyWindow")

		_, err = validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 168 * time.Hour}},
			},
		})
		assert.NoError(t, err)
	})
}
