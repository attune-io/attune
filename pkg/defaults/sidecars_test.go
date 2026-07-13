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

func boolPtr(v bool) *bool { return &v }

func TestEffectiveExcludedContainers_DefaultKnownOn(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	set := EffectiveExcludedContainers(policy)
	require.True(t, set["istio-proxy"])
	require.True(t, set["linkerd-proxy"])
	require.True(t, set["vault-agent"])
	assert.False(t, set["main"])
	assert.False(t, set["envoy"], "generic names must not be on the known list")
}

func TestEffectiveExcludedContainers_UnionWithUserList(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ExcludedContainers:   []string{"my-agent"},
			ExcludeKnownSidecars: boolPtr(true),
		},
	}
	set := EffectiveExcludedContainers(policy)
	assert.True(t, set["istio-proxy"])
	assert.True(t, set["my-agent"])
}

func TestEffectiveExcludedContainers_OptOutRestoresListOnly(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ExcludedContainers:   []string{"my-agent"},
			ExcludeKnownSidecars: boolPtr(false),
		},
	}
	set := EffectiveExcludedContainers(policy)
	assert.False(t, set["istio-proxy"])
	assert.True(t, set["my-agent"])
	assert.Len(t, set, 1)
}

func TestEffectiveExcludedContainers_NilPolicy(t *testing.T) {
	set := EffectiveExcludedContainers(nil)
	assert.Empty(t, set)
}

func TestExclusionReason(t *testing.T) {
	on := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ExcludeKnownSidecars: boolPtr(true),
			ExcludedContainers:   []string{"custom"},
		},
	}
	assert.Equal(t, "known sidecar auto-exclude", ExclusionReason(on, "istio-proxy"))
	assert.Equal(t, "listed in excludedContainers", ExclusionReason(on, "custom"))

	off := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			ExcludeKnownSidecars: boolPtr(false),
			ExcludedContainers:   []string{"istio-proxy"},
		},
	}
	assert.Equal(t, "listed in excludedContainers", ExclusionReason(off, "istio-proxy"))
}

func TestKnownSidecarContainers_NoGenericNames(t *testing.T) {
	for _, name := range attunev1alpha1.KnownSidecarContainers {
		assert.NotEqual(t, "envoy", name)
		assert.NotEqual(t, "proxy", name)
		assert.NotEqual(t, "sidecar", name)
	}
	require.NotEmpty(t, attunev1alpha1.KnownSidecarContainers)
}
