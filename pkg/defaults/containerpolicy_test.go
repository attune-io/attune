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

func TestMergeDefaults_DoesNotInheritContainerPolicies(t *testing.T) {
	t.Parallel()
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{{
				ContainerName: "app",
			}},
		},
	}
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 90,
				Overhead:   "20",
			},
		},
	}
	inherited := MergeDefaults(policy, defaults)
	for _, name := range inherited {
		assert.NotContains(t, name, "containerPolicies")
	}
	require.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	require.Equal(t, "20", policy.Spec.CPU.Overhead)
	cpu, _ := attunev1alpha1.EffectiveContainerResources(policy, "app")
	assert.Equal(t, int32(90), cpu.Percentile)
	assert.Equal(t, "20", cpu.Overhead)
	assert.Empty(t, policy.Spec.ContainerPolicies[0].CPU)
}
