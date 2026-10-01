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

package defaults

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestApplyBuiltInDefaults_SurgeStaysNilUntilSet(t *testing.T) {
	t.Parallel()
	bare := &attunev1alpha1.AttunePolicy{}
	ApplyBuiltInDefaults(bare)
	assert.Nil(t, bare.Spec.CPU.Surge)
	assert.Nil(t, bare.Spec.Memory.Surge)

	empty := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU:    attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{}},
			Memory: attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{}},
		},
	}
	ApplyBuiltInDefaults(empty)
	for _, block := range []*attunev1alpha1.Surge{empty.Spec.CPU.Surge, empty.Spec.Memory.Surge} {
		require.NotNil(t, block)
		assert.Equal(t, attunev1alpha1.DefaultSurgeTriggerRatio, block.TriggerRatio)
		require.NotNil(t, block.Percentile)
		assert.Equal(t, attunev1alpha1.DefaultSurgePercentile, *block.Percentile)
		require.NotNil(t, block.Window)
		assert.Equal(t, attunev1alpha1.DefaultSurgeWindow, block.Window.Duration)
	}

	zero := int32(0)
	explicit := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{
				TriggerRatio: "3",
				Percentile:   &zero,
			}},
		},
	}
	ApplyBuiltInDefaults(explicit)
	assert.Equal(t, "3", explicit.Spec.CPU.Surge.TriggerRatio)
	require.NotNil(t, explicit.Spec.CPU.Surge.Percentile)
	assert.Equal(t, int32(0), *explicit.Spec.CPU.Surge.Percentile, "a set 0 is not rewritten to 99")
	assert.Nil(t, explicit.Spec.Memory.Surge)
}

func TestMergeDefaults_Surge(t *testing.T) {
	t.Parallel()
	defRatio := "2"
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{TriggerRatio: defRatio},
			},
		},
	}

	empty := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{}},
		},
	}
	notes := MergeDefaults(empty, defaults)
	assert.Equal(t, "2", empty.Spec.CPU.Surge.TriggerRatio)
	assert.Contains(t, notes, "cpu.surge.triggerRatio")
	ApplyBuiltInDefaults(empty)
	require.NotNil(t, empty.Spec.CPU.Surge.Percentile)
	assert.Equal(t, int32(99), *empty.Spec.CPU.Surge.Percentile)
	require.NotNil(t, empty.Spec.CPU.Surge.Window)
	assert.Equal(t, 30*time.Minute, empty.Spec.CPU.Surge.Window.Duration)

	three := "3"
	wins := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{TriggerRatio: three}},
		},
	}
	notes = MergeDefaults(wins, defaults)
	assert.Equal(t, "3", wins.Spec.CPU.Surge.TriggerRatio)
	assert.NotContains(t, notes, "cpu.surge.triggerRatio")

	omitted := &attunev1alpha1.AttunePolicy{}
	notes = MergeDefaults(omitted, defaults)
	require.NotNil(t, omitted.Spec.CPU.Surge)
	assert.Equal(t, "2", omitted.Spec.CPU.Surge.TriggerRatio)
	assert.Contains(t, notes, "cpu.surge")

	neither := &attunev1alpha1.AttunePolicy{}
	MergeDefaults(neither, &attunev1alpha1.AttuneDefaults{})
	ApplyBuiltInDefaults(neither)
	assert.Nil(t, neither.Spec.CPU.Surge)
	assert.Nil(t, neither.Spec.Memory.Surge)
}
