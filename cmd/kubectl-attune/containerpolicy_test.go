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
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

func mustQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func TestPrintEffectivePolicySummary_ContainerPolicies(t *testing.T) {
	// Not parallel: capture swaps os.Stdout.
	capture := func(item unstructured.Unstructured, effective *attunev1alpha1.AttunePolicy, selected selectedDefaults) string {
		t.Helper()
		reader, writer, err := os.Pipe()
		require.NoError(t, err)
		old := os.Stdout
		os.Stdout = writer
		printEffectivePolicySummary(item, effective, selected)
		require.NoError(t, writer.Close())
		os.Stdout = old
		out, err := io.ReadAll(reader)
		require.NoError(t, err)
		return string(out)
	}
	emptyItem := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}

	t.Run("omitted list is silent", func(t *testing.T) {
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				CPU:    attunev1alpha1.ResourceConfig{Percentile: 95},
				Memory: attunev1alpha1.ResourceConfig{Percentile: 99},
			},
		}
		applyBuiltInDefaults(policy)
		got := capture(emptyItem, policy, selectedDefaults{})
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
		item := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"cpu":    map[string]interface{}{"percentile": int64(95)},
				"memory": map[string]interface{}{"percentile": float64(99)},
			},
		}}
		applyBuiltInDefaults(policy)
		got := capture(item, policy, selectedDefaults{})
		assert.Contains(t, got, "  Container policies:")
		assert.Contains(t, got, "    sidecar:")
		assert.Contains(t, got, "      CPU percentile: 95 (source: policy, configured: 95)")
		assert.Contains(t, got, "      CPU max allowed: 200m (source: container, configured: 200m)")
		assert.Contains(t, got, "      CPU controlled values: RequestsOnly (source: built-in, configured: <unset>)")
		assert.Contains(t, got, "      Memory percentile: 99 (source: policy, configured: 99)")
		assert.Contains(t, got, "      Memory max allowed: none (source: built-in, configured: <unset>)")
		assert.Contains(t, got, "      Memory controlled values: RequestsOnly (source: built-in, configured: <unset>)")
		assert.Contains(t, got, "    app:")
		assert.Contains(t, got, "      CPU max allowed: 300m (source: wildcard, configured: 300m)")
		assert.Contains(t, got, "      CPU controlled values: RequestsAndLimits (source: container, configured: RequestsAndLimits)")
		assert.Contains(t, got, "    *:")
		assert.Contains(t, got, "      CPU max allowed: 300m (source: container, configured: 300m)")
		assert.NotContains(t, got, "4000m")
	})

	t.Run("defaults memory max when policy and container omit it", func(t *testing.T) {
		max := mustQty(t, "1Gi")
		defaults := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &max},
			},
		}
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		mergeDefaultsIntoPolicy(policy, defaults)
		applyBuiltInDefaults(policy)
		got := capture(emptyItem, policy, selectedDefaults{defaults: defaults, source: sourceCluster})
		assert.Contains(t, got, "      Memory max allowed: 1Gi (source: cluster defaults, configured: 1Gi)")
		assert.Contains(t, got, "      CPU controlled values: RequestsOnly (source: built-in, configured: <unset>)")
		assert.NotContains(t, got, "source: policy, configured: 1Gi")
	})

	t.Run("policy max wins over a different defaults max", func(t *testing.T) {
		policyMax := mustQty(t, "2Gi")
		defaultsMax := mustQty(t, "1Gi")
		defaults := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &defaultsMax},
			},
		}
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				Memory: attunev1alpha1.ResourceConfig{MaxAllowed: &policyMax},
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		item := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"memory": map[string]interface{}{"maxAllowed": "2Gi"},
			},
		}}
		mergeDefaultsIntoPolicy(policy, defaults)
		applyBuiltInDefaults(policy)
		got := capture(item, policy, selectedDefaults{defaults: defaults, source: sourceCluster})
		assert.Contains(t, got, "      Memory max allowed: 2Gi (source: policy, configured: 2Gi)")
		assert.NotContains(t, got, "1Gi")
	})

	t.Run("namespace defaults win the label over cluster defaults", func(t *testing.T) {
		nsMax := mustQty(t, "2Gi")
		clusterMax := mustQty(t, "512Mi")
		nsPercentile := int32(90)
		namespace := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU:    &attunev1alpha1.ResourceConfig{Percentile: nsPercentile},
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &nsMax},
			},
		}
		cluster := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU:    &attunev1alpha1.ResourceConfig{Percentile: 50},
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &clusterMax},
			},
		}
		combined := pkgdefaults.CombineDefaultsLayers(cluster, namespace)
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		mergeDefaultsIntoPolicy(policy, combined)
		applyBuiltInDefaults(policy)
		got := capture(emptyItem, policy, selectedDefaults{
			defaults:  combined,
			source:    sourceMergedDefaults,
			namespace: namespace,
			cluster:   cluster,
		})
		assert.Contains(t, got, "      CPU percentile: 90 (source: namespace defaults, configured: 90)")
		assert.Contains(t, got, "      Memory max allowed: 2Gi (source: namespace defaults, configured: 2Gi)")
		assert.NotContains(t, got, "512Mi")
		assert.NotContains(t, got, "source: cluster defaults, configured: 50")
	})

	t.Run("cluster defaults fill a field the namespace omits", func(t *testing.T) {
		clusterMax := mustQty(t, "512Mi")
		namespace := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU: &attunev1alpha1.ResourceConfig{Percentile: 90},
			},
		}
		cluster := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &clusterMax},
			},
		}
		combined := pkgdefaults.CombineDefaultsLayers(cluster, namespace)
		both := attunev1alpha1.ControlledRequestsAndLimits
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		mergeDefaultsIntoPolicy(policy, combined)
		applyBuiltInDefaults(policy)
		selected := selectedDefaults{
			defaults:  combined,
			source:    sourceMergedDefaults,
			namespace: namespace,
			cluster:   cluster,
		}
		got := capture(emptyItem, policy, selected)
		assert.Contains(t, got, "      Memory max allowed: 512Mi (source: cluster defaults, configured: 512Mi)")
		assert.Contains(t, got, "      CPU percentile: 90 (source: namespace defaults, configured: 90)")

		namespace.Spec.Memory = &attunev1alpha1.ResourceConfig{ControlledValues: &both}
		combined = pkgdefaults.CombineDefaultsLayers(cluster, namespace)
		fresh := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		mergeDefaultsIntoPolicy(fresh, combined)
		applyBuiltInDefaults(fresh)
		got = capture(emptyItem, fresh, selectedDefaults{
			defaults:  combined,
			source:    sourceMergedDefaults,
			namespace: namespace,
			cluster:   cluster,
		})
		assert.Contains(t, got, "      Memory controlled values: RequestsAndLimits (source: namespace defaults, configured: RequestsAndLimits)")
	})

	t.Run("policy RequestsOnly stays policy", func(t *testing.T) {
		requests := attunev1alpha1.ControlledRequestsOnly
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				CPU: attunev1alpha1.ResourceConfig{ControlledValues: &requests},
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		item := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"cpu": map[string]interface{}{"controlledValues": attunev1alpha1.ControlledRequestsOnly},
			},
		}}
		applyBuiltInDefaults(policy)
		got := capture(item, policy, selectedDefaults{})
		assert.Contains(t, got, "      CPU controlled values: RequestsOnly (source: policy, configured: RequestsOnly)")
		assert.Contains(t, got, "      Memory controlled values: RequestsOnly (source: built-in, configured: <unset>)")
	})

	t.Run("combined defaults without a layer pointer say defaults", func(t *testing.T) {
		max := mustQty(t, "1Gi")
		defaults := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				Memory: &attunev1alpha1.ResourceConfig{MaxAllowed: &max},
			},
		}
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app"},
				},
			},
		}
		mergeDefaultsIntoPolicy(policy, defaults)
		applyBuiltInDefaults(policy)
		got := capture(emptyItem, policy, selectedDefaults{defaults: defaults, source: sourceMergedDefaults})
		assert.Contains(t, got, "      Memory max allowed: 1Gi (source: defaults, configured: 1Gi)")
	})

	t.Run("named container max wins over defaults max", func(t *testing.T) {
		containerMax := mustQty(t, "200m")
		defaultsMax := mustQty(t, "1")
		defaults := &attunev1alpha1.AttuneDefaults{
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				CPU: &attunev1alpha1.ResourceConfig{MaxAllowed: &defaultsMax},
			},
		}
		policy := &attunev1alpha1.AttunePolicy{
			Spec: attunev1alpha1.AttunePolicySpec{
				ContainerPolicies: []attunev1alpha1.ContainerResourcePolicy{
					{ContainerName: "app", CPU: &attunev1alpha1.ResourceConfig{MaxAllowed: &containerMax}},
				},
			},
		}
		mergeDefaultsIntoPolicy(policy, defaults)
		applyBuiltInDefaults(policy)
		got := capture(emptyItem, policy, selectedDefaults{defaults: defaults, source: sourceNamespace})
		assert.Contains(t, got, "      CPU max allowed: 200m (source: container, configured: 200m)")
		assert.NotContains(t, got, "source: namespace defaults, configured: 1")
	})
}
