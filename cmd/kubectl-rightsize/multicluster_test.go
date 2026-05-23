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
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// ---------- resolveContexts ----------

func TestResolveContexts_FromFlag(t *testing.T) {
	ctxs, err := resolveContexts("", "prod-east,prod-west", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"prod-east", "prod-west"}, ctxs)
}

func TestResolveContexts_TrimsWhitespace(t *testing.T) {
	ctxs, err := resolveContexts("", " a , b , c ", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, ctxs)
}

func TestResolveContexts_EmptyFlagErrors(t *testing.T) {
	_, err := resolveContexts("", " , , ", false)
	assert.Error(t, err)
}

func TestResolveContexts_NeitherFlagReturnsNil(t *testing.T) {
	ctxs, err := resolveContexts("", "", false)
	require.NoError(t, err)
	assert.Nil(t, ctxs)
}

// ---------- tagItems / itemCluster / hasClusterAnnotation ----------

func TestTagItems(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "a"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "b"}}},
	}
	tagItems(items, "prod-east")
	assert.Equal(t, "prod-east", itemCluster(items[0]))
	assert.Equal(t, "prod-east", itemCluster(items[1]))
	assert.True(t, hasClusterAnnotation(items))
}

func TestHasClusterAnnotation_FalseForUntagged(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "a"}}},
	}
	assert.False(t, hasClusterAnnotation(items))
}

func TestItemCluster_EmptyForUntagged(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "a"},
	}}
	assert.Equal(t, "", itemCluster(item))
}

// ---------- fetchMultiCluster ----------

func fakeMultiContextFactory(t *testing.T, clusters map[string][]runtime.Object) dynamicClientFactory {
	t.Helper()
	return func(kubeconfigPath, ctxName string) (dynamic.Interface, string, error) {
		objects, ok := clusters[ctxName]
		if !ok {
			return nil, "", fmt.Errorf("connection refused")
		}
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"},
			objects...)
		return client, "default", nil
	}
}

func TestFetchMultiCluster_MergesItems(t *testing.T) {
	p1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "api-svc", "namespace": "default"},
	}}
	p2 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "web-svc", "namespace": "default"},
	}}

	factory := fakeMultiContextFactory(t, map[string][]runtime.Object{
		"prod-east": {p1},
		"prod-west": {p2},
	})

	items, warnings := fetchMultiCluster(context.Background(), "", []string{"prod-east", "prod-west"}, "default", false, factory)
	assert.Empty(t, warnings)
	assert.Len(t, items, 2)

	clusters := map[string]bool{}
	for _, item := range items {
		clusters[itemCluster(item)] = true
	}
	assert.True(t, clusters["prod-east"])
	assert.True(t, clusters["prod-west"])
}

func TestFetchMultiCluster_ReportsWarningsForFailedContexts(t *testing.T) {
	p1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "api-svc", "namespace": "default"},
	}}

	factory := fakeMultiContextFactory(t, map[string][]runtime.Object{
		"prod-east": {p1},
		// "broken" is not in the map, so factory returns an error.
	})

	items, warnings := fetchMultiCluster(context.Background(), "", []string{"prod-east", "broken"}, "default", false, factory)
	assert.Len(t, items, 1)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "broken")
	assert.Contains(t, warnings[0], "connection refused")
}

func TestFetchMultiCluster_AllContextsFail(t *testing.T) {
	factory := fakeMultiContextFactory(t, map[string][]runtime.Object{})

	items, warnings := fetchMultiCluster(context.Background(), "", []string{"a", "b"}, "default", false, factory)
	assert.Empty(t, items)
	assert.Len(t, warnings, 2)
}

// ---------- printStatusItems multi-cluster ----------

func TestPrintStatusItems_ShowsClusterColumn(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "api-svc", "namespace": "default",
				"creationTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec":   map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
			"status": map[string]interface{}{"workloads": map[string]interface{}{"discovered": int64(2)}},
		}},
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "web-svc", "namespace": "default",
				"creationTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec":   map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Recommend"}},
			"status": map[string]interface{}{"workloads": map[string]interface{}{"discovered": int64(1)}},
		}},
	}
	tagItems(items[:1], "prod-east")
	tagItems(items[1:], "prod-west")

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatusItems(items, "", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "CLUSTER")
	assert.Contains(t, output, "prod-east")
	assert.Contains(t, output, "prod-west")
	assert.Contains(t, output, "api-svc")
	assert.Contains(t, output, "web-svc")
}

func TestPrintStatusItems_NoClusterColumnForSingleCluster(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "api-svc", "namespace": "default",
				"creationTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec":   map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
			"status": map[string]interface{}{"workloads": map[string]interface{}{"discovered": int64(2)}},
		}},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatusItems(items, "", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.NotContains(t, output, "CLUSTER")
	assert.Contains(t, output, "NAMESPACE")
}

// ---------- printSavingsItems multi-cluster ----------

func TestPrintSavingsItems_CrossClusterTotals(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "east-policy", "namespace": "default"},
			"status": map[string]interface{}{
				"savings": map[string]interface{}{
					"cpuRequestReduction":     "350m",
					"cpuRequestTotal":         "1",
					"memoryRequestReduction":  "134217728",
					"estimatedMonthlySavings": "$12.78",
				},
			},
		}},
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "west-policy", "namespace": "default"},
			"status": map[string]interface{}{
				"savings": map[string]interface{}{
					"cpuRequestReduction":     "150m",
					"cpuRequestTotal":         "500m",
					"memoryRequestReduction":  "67108864",
					"estimatedMonthlySavings": "$5.22",
				},
			},
		}},
	}
	tagItems(items[:1], "prod-east")
	tagItems(items[1:], "prod-west")

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printSavingsItems(items, "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "CLUSTER")
	assert.Contains(t, output, "prod-east")
	assert.Contains(t, output, "prod-west")
	assert.Contains(t, output, "TOTAL")
	assert.Contains(t, output, "500m")   // 350m + 150m
	assert.Contains(t, output, "$18.00") // $12.78 + $5.22
}

// ---------- printHistoryItems multi-cluster ----------

func TestPrintHistoryItems_ShowsClusterColumn(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "east-app", "namespace": "default"},
			"status": map[string]interface{}{
				"resizeHistory": []interface{}{
					map[string]interface{}{
						"timestamp": "2026-05-10T12:00:00Z",
						"workload":  "api-deploy",
						"container": "app",
						"resource":  "cpu",
						"from":      "500m",
						"to":        "250m",
						"method":    "InPlace",
						"result":    "Success",
					},
				},
			},
		}},
	}
	tagItems(items, "prod-east")

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printHistoryItems(items)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "CLUSTER")
	assert.Contains(t, output, "prod-east")
	assert.Contains(t, output, "api-deploy")
	assert.Contains(t, output, "500m")
}

// ---------- printRecommendationsItems multi-cluster ----------

func TestPrintRecommendationsItems_ShowsClusterColumn(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "east-policy", "namespace": "default"},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api-deploy",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "256Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "256Mi"},
								"confidence":  0.95,
							},
						},
					},
				},
			},
		}},
	}
	tagItems(items, "prod-east")

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printRecommendationsItems(items)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "CLUSTER")
	assert.Contains(t, output, "prod-east")
	assert.Contains(t, output, "api-deploy")
	assert.Contains(t, output, "250m")
	assert.Contains(t, output, "95.0%")
}

// ---------- run() multi-cluster validation ----------

func TestRun_MultiClusterRejectsExplain(t *testing.T) {
	exitCode, _, stderr := captureRun(t, []string{"explain", "--all-contexts", "my-policy"},
		fakeDynamicClientFactory(t))
	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "explain requires a single cluster context")
}

func TestRun_MultiClusterRejectsWizard(t *testing.T) {
	exitCode, _, stderr := captureRun(t, []string{"wizard", "--all-contexts"},
		fakeDynamicClientFactory(t))
	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "wizard requires a single cluster context")
}

func TestRun_MultiClusterRejectsPreview(t *testing.T) {
	exitCode, _, stderr := captureRun(t, []string{"preview", "--contexts", "a,b", "my-policy"},
		fakeDynamicClientFactory(t))
	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "preview requires a single cluster context")
}

func TestRun_MultiClusterRejectsStructuredOutput(t *testing.T) {
	exitCode, _, stderr := captureRun(t, []string{"status", "--all-contexts", "-o", "json"},
		fakeDynamicClientFactory(t))
	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "not supported with --contexts or --all-contexts")
}

func TestRun_MultiClusterRejectsWatch(t *testing.T) {
	exitCode, _, stderr := captureRun(t, []string{"status", "--all-contexts", "--watch"},
		fakeDynamicClientFactory(t))
	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "--watch is not supported with --contexts or --all-contexts")
}

// ---------- run() multi-cluster end-to-end ----------

func TestRun_MultiClusterStatus(t *testing.T) {
	p1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name": "east-policy", "namespace": "default",
			"creationTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
		"status": map[string]interface{}{
			"workloads":  map[string]interface{}{"discovered": int64(3)},
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "reason": "Monitoring"}},
		},
	}}

	// The factory returns the same client regardless of context (for simplicity),
	// but the multi-cluster code path tags each item with the context name.
	factory := fakeMultiContextFactory(t, map[string][]runtime.Object{
		"prod-east": {p1},
		"prod-west": {},
	})

	exitCode, stdout, stderr := captureRun(t,
		[]string{"status", "--contexts", "prod-east,prod-west"},
		factory)
	assert.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "CLUSTER")
	assert.Contains(t, stdout, "prod-east")
	assert.Contains(t, stdout, "east-policy")
	assert.Empty(t, stderr)
}

func TestRun_MultiClusterWithWarnings(t *testing.T) {
	p1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name": "ok-policy", "namespace": "default",
			"creationTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec":   map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
		"status": map[string]interface{}{"workloads": map[string]interface{}{"discovered": int64(1)}},
	}}

	// "broken" context is not in the map, so factory returns an error.
	factory := fakeMultiContextFactory(t, map[string][]runtime.Object{
		"good": {p1},
	})

	exitCode, stdout, stderr := captureRun(t,
		[]string{"status", "--contexts", "good,broken"},
		factory)
	assert.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "ok-policy")
	assert.Contains(t, stderr, "WARNING")
	assert.Contains(t, stderr, "broken")
}
