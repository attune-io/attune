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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestApplyBuiltInDefaults_HPATargetBoundsStayNil(t *testing.T) {
	t.Parallel()
	bare := &attunev1alpha1.AttunePolicy{}
	ApplyBuiltInDefaults(bare)
	if bare.Spec.UpdateStrategy != nil {
		assert.Nil(t, bare.Spec.UpdateStrategy.HPATargetBounds)
	}

	empty := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				HPATargetBounds: &attunev1alpha1.HPATargetBounds{},
			},
		},
	}
	ApplyBuiltInDefaults(empty)
	require.NotNil(t, empty.Spec.UpdateStrategy.HPATargetBounds)
	assert.Nil(t, empty.Spec.UpdateStrategy.HPATargetBounds.CPU)
	assert.Nil(t, empty.Spec.UpdateStrategy.HPATargetBounds.Memory)
}

func TestMergeDefaults_HPATargetBounds(t *testing.T) {
	t.Parallel()
	cpuMin := int32(50)
	memoryMax := int32(90)
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				HPATargetBounds: &attunev1alpha1.HPATargetBounds{
					CPU:    &attunev1alpha1.HPATargetBound{Min: &cpuMin},
					Memory: &attunev1alpha1.HPATargetBound{Max: &memoryMax},
				},
			},
		},
	}

	omitted := &attunev1alpha1.AttunePolicy{}
	notes := MergeDefaults(omitted, defaults)
	require.NotNil(t, omitted.Spec.UpdateStrategy)
	require.NotNil(t, omitted.Spec.UpdateStrategy.HPATargetBounds)
	assert.Contains(t, notes, "hpaTargetBounds")
	assert.NotSame(t, defaults.Spec.UpdateStrategy.HPATargetBounds, omitted.Spec.UpdateStrategy.HPATargetBounds)
	assert.NotSame(t, defaults.Spec.UpdateStrategy.HPATargetBounds.Memory, omitted.Spec.UpdateStrategy.HPATargetBounds.Memory)
	assert.NotSame(t, defaults.Spec.UpdateStrategy.HPATargetBounds.Memory.Max, omitted.Spec.UpdateStrategy.HPATargetBounds.Memory.Max)
	*omitted.Spec.UpdateStrategy.HPATargetBounds.Memory.Max = 10
	assert.Equal(t, int32(90), *defaults.Spec.UpdateStrategy.HPATargetBounds.Memory.Max)
	assert.Equal(t, int32(50), *defaults.Spec.UpdateStrategy.HPATargetBounds.CPU.Min)

	policyMax := int32(80)
	partial := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				HPATargetBounds: &attunev1alpha1.HPATargetBounds{
					CPU: &attunev1alpha1.HPATargetBound{Max: &policyMax},
				},
			},
		},
	}
	notes = MergeDefaults(partial, defaults)
	assert.Equal(t, int32(80), *partial.Spec.UpdateStrategy.HPATargetBounds.CPU.Max)
	require.NotNil(t, partial.Spec.UpdateStrategy.HPATargetBounds.CPU.Min)
	assert.Equal(t, int32(50), *partial.Spec.UpdateStrategy.HPATargetBounds.CPU.Min)
	assert.NotSame(t, defaults.Spec.UpdateStrategy.HPATargetBounds.CPU.Min, partial.Spec.UpdateStrategy.HPATargetBounds.CPU.Min)
	require.NotNil(t, partial.Spec.UpdateStrategy.HPATargetBounds.Memory)
	assert.Equal(t, int32(90), *partial.Spec.UpdateStrategy.HPATargetBounds.Memory.Max)
	assert.NotSame(t, defaults.Spec.UpdateStrategy.HPATargetBounds.Memory, partial.Spec.UpdateStrategy.HPATargetBounds.Memory)
	assert.Contains(t, notes, "hpaTargetBounds.cpu.min")
	assert.Contains(t, notes, "hpaTargetBounds.memory")
	*partial.Spec.UpdateStrategy.HPATargetBounds.CPU.Min = 10
	assert.Equal(t, int32(50), *defaults.Spec.UpdateStrategy.HPATargetBounds.CPU.Min)
}
