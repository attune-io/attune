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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name    string
		created time.Time
		want    string
	}{
		{
			name:    "seconds ago",
			created: time.Now().Add(-30 * time.Second),
			want:    "30s",
		},
		{
			name:    "minutes ago",
			created: time.Now().Add(-5 * time.Minute),
			want:    "5m",
		},
		{
			name:    "hours ago",
			created: time.Now().Add(-3 * time.Hour),
			want:    "3h",
		},
		{
			name:    "days ago",
			created: time.Now().Add(-48 * time.Hour),
			want:    "2d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAge(tt.created)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty string", input: "", want: ""},
		{name: "dash", input: "-", want: "-"},
		{name: "non-numeric", input: "128Mi", want: "128Mi"},
		{name: "bytes in GiB", input: "2147483648", want: "2.0Gi"},
		{name: "bytes in MiB", input: "134217728", want: "128Mi"},
		{name: "bytes in KiB", input: "8192", want: "8Ki"},
		{name: "small bytes", input: "512", want: "512B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatMemory(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetNestedString(t *testing.T) {
	obj := unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"mode": "Recommend",
				},
			},
		},
	}

	assert.Equal(t, "Recommend", getNestedString(obj, "spec", "updateStrategy", "mode"))
	assert.Equal(t, "", getNestedString(obj, "spec", "nonexistent"))
	assert.Equal(t, "", getNestedString(obj, "missing", "path"))
}

func TestGetNestedInt64(t *testing.T) {
	obj := unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"workloads": map[string]interface{}{
					"discovered": int64(5),
					"resized":    int64(3),
				},
			},
		},
	}

	assert.Equal(t, int64(5), getNestedInt64(obj, "status", "workloads", "discovered"))
	assert.Equal(t, int64(3), getNestedInt64(obj, "status", "workloads", "resized"))
	assert.Equal(t, int64(0), getNestedInt64(obj, "status", "workloads", "missing"))
	assert.Equal(t, int64(0), getNestedInt64(obj, "missing"))
}

func TestGetConditionReason(t *testing.T) {
	tests := []struct {
		name          string
		obj           unstructured.Unstructured
		conditionType string
		want          string
	}{
		{
			name: "ready with reason",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
								"reason": "Monitoring",
							},
						},
					},
				},
			},
			conditionType: "Ready",
			want:          "Monitoring",
		},
		{
			name: "ready without reason returns status",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
							},
						},
					},
				},
			},
			conditionType: "Ready",
			want:          "True",
		},
		{
			name: "condition not found",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{},
					},
				},
			},
			conditionType: "Degraded",
			want:          "-",
		},
		{
			name: "no conditions",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{},
				},
			},
			conditionType: "Ready",
			want:          "-",
		},
		{
			name: "degraded with reason",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Degraded",
								"status": "True",
								"reason": "HighRevertRate",
							},
						},
					},
				},
			},
			conditionType: "Degraded",
			want:          "HighRevertRate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getConditionReason(tt.obj, tt.conditionType)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrintHistory(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "my-app",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"resizeHistory": []interface{}{
					map[string]interface{}{
						"timestamp": "2026-05-10T12:00:00Z",
						"workload":  "my-deploy",
						"container": "app",
						"resource":  "cpu",
						"from":      "500m",
						"to":        "250m",
						"result":    "Success",
					},
					map[string]interface{}{
						"timestamp": "2026-05-10T13:00:00Z",
						"workload":  "my-deploy",
						"container": "app",
						"resource":  "memory",
						"from":      "512Mi",
						"to":        "384Mi",
						"result":    "Reverted",
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr: "RightSizePolicyList",
		}, policy)

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printHistory(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "NAMESPACE")
	assert.Contains(t, output, "POLICY")
	assert.Contains(t, output, "my-app")
	assert.Contains(t, output, "my-deploy")
	assert.Contains(t, output, "500m")
	assert.Contains(t, output, "250m")
	assert.Contains(t, output, "Success")
	assert.Contains(t, output, "Reverted")
}

func TestPrintHistory_NoHistory(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "empty-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr: "RightSizePolicyList",
		}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printHistory(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// Should have header but no data rows.
	assert.Contains(t, output, "NAMESPACE")
	assert.NotContains(t, output, "empty-policy")
}

// ---------- printStatus ----------

func TestPrintStatus(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":              "web-app",
				"namespace":         "production",
				"creationTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"mode": "Auto",
				},
			},
			"status": map[string]interface{}{
				"workloads": map[string]interface{}{
					"discovered": int64(3),
					"resized":    int64(2),
				},
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
						"reason": "Monitoring",
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatus(context.Background(), dynClient, "production")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "web-app")
	assert.Contains(t, output, "Auto")
	assert.Contains(t, output, "Monitoring")
	assert.Contains(t, output, "production")
}

func TestPrintStatus_NoPolicies(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"})

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatus(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "No RightSizePolicies found")
}

// ---------- printSavings ----------

func TestPrintSavings(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "api-svc",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"savings": map[string]interface{}{
					"cpuRequestReduction":     "350m",
					"memoryRequestReduction":  "134217728",
					"estimatedMonthlySavings": "$12.78",
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printSavings(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "api-svc")
	assert.Contains(t, output, "350m")
	assert.Contains(t, output, "128Mi")
	assert.Contains(t, output, "$12.78")
}

func TestPrintSavings_NoSavings(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "fresh-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printSavings(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "fresh-policy")
	assert.Contains(t, output, "-")
}

// ---------- printRecommendations ----------

func TestPrintRecommendations(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "my-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "web-deploy",
						"containers": []interface{}{
							map[string]interface{}{
								"name":       "app",
								"confidence": 0.85,
								"current": map[string]interface{}{
									"cpuRequest":    "500m",
									"memoryRequest": "512Mi",
								},
								"recommended": map[string]interface{}{
									"cpuRequest":    "250m",
									"memoryRequest": "384Mi",
								},
							},
						},
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printRecommendations(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "web-deploy")
	assert.Contains(t, output, "app")
	assert.Contains(t, output, "500m")
	assert.Contains(t, output, "250m")
	assert.Contains(t, output, "85.0%")
}

func TestPrintRecommendations_CollectingData(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "new-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":    "Ready",
						"status":  "False",
						"reason":  "InsufficientData",
						"message": "Not enough data",
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printRecommendations(context.Background(), dynClient, "default")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "new-policy")
	assert.Contains(t, output, "Collecting data")
}

// ---------- policyReadyReason ----------

func TestPolicyReadyReason_NoConditions(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{},
	}}
	assert.Equal(t, "Pending", policyReadyReason(item))
}

func TestPolicyReadyReason_InsufficientDataWithMessage(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InsufficientData",
					"message": "No matching workloads found",
				},
			},
		},
	}}
	assert.Equal(t, "Collecting data", policyReadyReason(item))
}

func TestPolicyReadyReason_OtherReason(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
					"reason": "Monitoring",
				},
			},
		},
	}}
	assert.Equal(t, "Monitoring", policyReadyReason(item))
}

func TestPolicyReadyReason_InsufficientDataNoMessage(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "False",
					"reason": "InsufficientData",
				},
			},
		},
	}}
	assert.Equal(t, "InsufficientData", policyReadyReason(item))
}
