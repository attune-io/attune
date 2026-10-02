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

package main

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func mustQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func TestPrintEffectivePolicySummary_ContainerPolicies(t *testing.T) {
	// Not parallel: capture swaps os.Stdout.
	capture := func(effective *attunev1alpha1.AttunePolicy) string {
		t.Helper()
		reader, writer, err := os.Pipe()
		require.NoError(t, err)
		old := os.Stdout
		os.Stdout = writer
		item := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{},
		}}
		printEffectivePolicySummary(item, effective, selectedDefaults{})
		require.NoError(t, writer.Close())
		os.Stdout = old
		out, err := io.ReadAll(reader)
		require.NoError(t, err)
		return string(out)
	}

	t.Run("omitted list is silent", func(t *testing.T) {
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				CPU:    attunev1alpha1.ResourceConfig{Percentile: 95},
				Memory: attunev1alpha1.ResourceConfig{Percentile: 99},
			},
		}
		applyBuiltInDefaults(policy)
		got := capture(policy)
		assert.NotContains(t, got, "Container policies:")
	})

	t.Run("prints source per field", func(t *testing.T) {
		sideMax := mustQty(t, "200m")
		starMax := mustQty(t, "300m")
		both := attunev1alpha1.ControlledRequestsAndLimits
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				CPU:    attunev1alpha1.ResourceConfig{Percentile: 95},
				Memory: attunev1alpha1.ResourceConfig{Percentile: 99},
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{
						ContainerName: "sidecar",
						CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &sideMax},
					},
					{
						ContainerName: "app",
						CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
					},
					{
						ContainerName: "*",
						CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &starMax},
					},
				},
			},
		}
		applyBuiltInDefaults(policy)
		got := capture(policy)
		assert.Contains(t, got, "  Container policies:")
		assert.Contains(t, got, "    sidecar:")
		assert.Contains(t, got, "      CPU percentile: 95 (source: policy, configured: <unset>)")
		assert.Contains(t, got, "      CPU max allowed: 200m (source: container, configured: 200m)")
		assert.Contains(t, got, "      CPU controlled values: RequestsOnly (source: policy, configured: <unset>)")
		assert.Contains(t, got, "      Memory percentile: 99 (source: policy, configured: <unset>)")
		assert.Contains(t, got, "      Memory max allowed: 8Gi (source: built-in default, configured: <unset>)")
		assert.Contains(t, got, "      Memory controlled values: RequestsOnly (source: policy, configured: <unset>)")
		assert.Contains(t, got, "    app:")
		assert.Contains(t, got, "      CPU max allowed: 300m (source: wildcard, configured: 300m)")
		assert.Contains(t, got, "      CPU controlled values: RequestsAndLimits (source: container, configured: RequestsAndLimits)")
		assert.Contains(t, got, "    *:")
		assert.Contains(t, got, "      CPU max allowed: 300m (source: container, configured: 300m)")
		assert.NotContains(t, got, "4000m")
	})
}
