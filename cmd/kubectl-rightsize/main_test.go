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
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
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
					"type": "Recommend",
				},
			},
		},
	}

	assert.Equal(t, "Recommend", getNestedString(obj, "spec", "updateStrategy", "type"))
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

func TestGetConditionMessage(t *testing.T) {
	tests := []struct {
		name          string
		obj           unstructured.Unstructured
		conditionType string
		want          string
	}{
		{
			name: "returns message for matching condition",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":    "Ready",
								"status":  "False",
								"message": "Waiting for metrics data",
							},
						},
					},
				},
			},
			conditionType: "Ready",
			want:          "Waiting for metrics data",
		},
		{
			name: "returns empty when condition not found",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{},
					},
				},
			},
			conditionType: "Degraded",
			want:          "",
		},
		{
			name: "returns empty when no conditions field",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{},
				},
			},
			conditionType: "Ready",
			want:          "",
		},
		{
			name: "returns empty when message field missing",
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
			want:          "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getConditionMessage(tt.obj, tt.conditionType)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrintPreview(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "web-app",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{"type": "Recommend"},
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "web-deploy",
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
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printPreview(context.Background(), dynClient, "default", "web-app")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "Preview:")
	assert.Contains(t, output, "web-deploy")
	assert.Contains(t, output, "500m")
	assert.Contains(t, output, "250m")
	assert.Contains(t, output, "CPU")
	assert.Contains(t, output, "Memory")
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
						"method":    "InPlace",
						"result":    "Success",
					},
					map[string]interface{}{
						"timestamp": "2026-05-10T13:00:00Z",
						"workload":  "my-deploy",
						"container": "app",
						"resource":  "memory",
						"from":      "512Mi",
						"to":        "384Mi",
						"method":    "InPlace",
						"result":    "Reverted",
						"reason":    "oomkill",
					},
					map[string]interface{}{
						"timestamp": "2026-05-10T14:00:00Z",
						"workload":  "my-deploy",
						"container": "app",
						"resource":  "cpu+memory",
						"method":    "Eviction",
						"result":    "Evicted",
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
	assert.Contains(t, output, "InPlace")
	assert.Contains(t, output, "Eviction")
	assert.Contains(t, output, "Success")
	assert.Contains(t, output, "Reverted")
	assert.Contains(t, output, "Evicted")
	assert.Contains(t, output, "oomkill")
	assert.Contains(t, output, "REASON")
}

func TestPrintHistory_LegacyEntryWithoutMethodDefaultsToInPlace(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "legacy-app",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"resizeHistory": []interface{}{
					map[string]interface{}{
						"timestamp": "2026-05-10T12:00:00Z",
						"workload":  "legacy-deploy",
						"container": "app",
						"resource":  "cpu",
						"from":      "500m",
						"to":        "250m",
						"result":    "Success",
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

	assert.Contains(t, output, "legacy-app")
	assert.Contains(t, output, "legacy-deploy")
	assert.Contains(t, output, "InPlace")
	assert.Contains(t, output, "Success")
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
					"type": "Auto",
				},
			},
			"status": map[string]interface{}{
				"workloads": map[string]interface{}{
					"discovered": int64(3),
					"pending":    int64(1),
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

	printStatus(context.Background(), dynClient, "production", "", "")

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
	assert.Contains(t, output, "PENDING")
	assert.Contains(t, output, "CANARY")
	assert.Contains(t, output, "3           1         2")
}

func TestPrintStatus_ReadyContract(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		status     string
		message    string
		wantOutput string
	}{
		{
			name:       "monitoring reason",
			reason:     "Monitoring",
			status:     "True",
			wantOutput: "Monitoring",
		},
		{
			name:       "insufficient data with message",
			reason:     "InsufficientData",
			status:     "False",
			message:    "Collecting data: 10/48 data points (21%)",
			wantOutput: "Collecting data: 10/48 data points (21%)",
		},
		{
			name:       "insufficient data without message",
			reason:     "InsufficientData",
			status:     "False",
			wantOutput: "InsufficientData",
		},
		{
			name:       "prometheus unavailable actionable message",
			reason:     "PrometheusUnavailable",
			status:     "False",
			message:    "Cannot create metrics collector: TLS handshake timeout",
			wantOutput: "Cannot create metrics collector: TLS handshake timeout",
		},
		{
			name:       "invalid config actionable message",
			reason:     "InvalidConfig",
			status:     "False",
			message:    "Failed to fetch defaults: simulated namespace defaults API failure",
			wantOutput: "Failed to fetch defaults: simulated namespace defaults API failure",
		},
		{
			name:       "workload discovery actionable message",
			reason:     "WorkloadDiscoveryFailed",
			status:     "False",
			message:    "Failed to discover workloads: unsupported kind FooSet",
			wantOutput: "Failed to discover workloads: unsupported kind FooSet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
							"type": "Auto",
						},
					},
					"status": map[string]interface{}{
						"workloads": map[string]interface{}{
							"discovered": int64(3),
							"pending":    int64(1),
							"resized":    int64(2),
						},
						"conditions": []interface{}{
							map[string]interface{}{
								"type":    "Ready",
								"status":  tt.status,
								"reason":  tt.reason,
								"message": tt.message,
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

			printStatus(context.Background(), dynClient, "production", "", "")

			w.Close()
			os.Stdout = old

			var buf bytes.Buffer
			_, err = buf.ReadFrom(r)
			require.NoError(t, err)
			output := buf.String()

			assert.Contains(t, output, tt.wantOutput)
		})
	}
}

func TestPrintStatus_NoPolicies(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"})

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatus(context.Background(), dynClient, "default", "", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "No RightSizePolicies found")
}

func TestPrintStatus_FilterDegraded(t *testing.T) {
	degraded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "bad-app", "namespace": "prod", "creationTimestamp": "2026-01-01T00:00:00Z"},
		"spec":       map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
		"status": map[string]interface{}{
			"workloads": map[string]interface{}{"discovered": int64(1)},
			"conditions": []interface{}{
				map[string]interface{}{"type": "Degraded", "status": "True", "reason": "HighRevertRate"},
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Monitoring"},
			},
		},
	}}
	healthy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "good-app", "namespace": "prod", "creationTimestamp": "2026-01-01T00:00:00Z"},
		"spec":       map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
		"status": map[string]interface{}{
			"workloads": map[string]interface{}{"discovered": int64(2)},
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Monitoring"},
			},
		},
	}}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, degraded, healthy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStatus(context.Background(), dynClient, "prod", "", "degraded")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "bad-app")
	assert.NotContains(t, output, "good-app")
}

func TestSortPolicies_BySavings(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "low"},
			"status":   map[string]interface{}{"savings": map[string]interface{}{"estimatedMonthlySavings": "$5.00"}},
		}},
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "high"},
			"status":   map[string]interface{}{"savings": map[string]interface{}{"estimatedMonthlySavings": "$50.00"}},
		}},
	}
	sortPolicies(items, "savings")
	assert.Equal(t, "high", items[0].GetName())
	assert.Equal(t, "low", items[1].GetName())
}

func TestFormatCanaryStatus(t *testing.T) {
	tests := []struct {
		name     string
		obj      map[string]interface{}
		expected string
	}{
		{
			name: "non-canary mode",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
			},
			expected: "-",
		},
		{
			name: "canary pending",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Canary"}},
			},
			expected: "Pending",
		},
		{
			name: "canary in progress with pods",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Canary"}},
				"status": map[string]interface{}{
					"canary": map[string]interface{}{
						"phase": "CanaryInProgress",
						"pods":  []interface{}{"pod-a", "pod-b"},
					},
				},
			},
			expected: "CanaryInProgress (2 pods)",
		},
		{
			name: "canary full rollout",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Canary"}},
				"status": map[string]interface{}{
					"canary": map[string]interface{}{"phase": "FullRollout"},
				},
			},
			expected: "FullRollout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := unstructured.Unstructured{Object: tt.obj}
			assert.Equal(t, tt.expected, formatCanaryStatus(item))
		})
	}
}

func TestFilterPolicies_EmptyFilterReturnsAll(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "a"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "b"}}},
	}
	result := filterPolicies(items, "")
	assert.Len(t, result, 2)
}

func TestRun_FilterFlagRejectedForNonStatus(t *testing.T) {
	code := run([]string{"savings", "--filter", "degraded"}, func(string, string) (dynamic.Interface, string, error) {
		return nil, "default", nil
	})
	assert.Equal(t, 1, code)
}

func TestRun_SortByFlagRejectedForHistory(t *testing.T) {
	code := run([]string{"history", "--sort-by", "name"}, func(string, string) (dynamic.Interface, string, error) {
		return nil, "default", nil
	})
	assert.Equal(t, 1, code)
}

// ---------- printStructured ----------

func TestPrintStructured_JSON(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "json-test",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"type": "Recommend",
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

	printStructured(context.Background(), dynClient, "default", "json")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// Should be valid JSON containing the raw policy list.
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &parsed), "output should be valid JSON")
	assert.Equal(t, "RightSizePolicyList", parsed["kind"])
	items, ok := parsed["items"].([]interface{})
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "json-test", item["metadata"].(map[string]interface{})["name"])
	assert.Contains(t, output, `"Recommend"`)
}

func TestPrintStructured_YAML(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "yaml-test",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"type": "Auto",
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

	printStructured(context.Background(), dynClient, "default", "yaml")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// YAML should contain the policy name and mode.
	assert.Contains(t, output, "yaml-test")
	assert.Contains(t, output, "Auto")
	// Should NOT look like JSON.
	assert.NotContains(t, output, `{`)
}

func TestPrintStructured_NoPolicies(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"})

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printStructured(context.Background(), dynClient, "default", "json")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// Empty list should still be valid JSON.
	var parsed interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &parsed), "empty list should be valid JSON")
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
					"cpuRequestTotal":         "1",
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

	printSavings(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "api-svc")
	assert.Contains(t, output, "350m")
	assert.Contains(t, output, "128Mi")
	assert.Contains(t, output, "35%")
	assert.Contains(t, output, "$12.78")
	assert.Contains(t, output, "TOTAL")
}

func TestPrintSavings_MultiplePoliciesShowTotals(t *testing.T) {
	p1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "api-svc", "namespace": "default"},
		"status": map[string]interface{}{
			"savings": map[string]interface{}{
				"cpuRequestReduction":     "350m",
				"cpuRequestTotal":         "1",
				"memoryRequestReduction":  "134217728",
				"estimatedMonthlySavings": "$12.78",
			},
		},
	}}
	p2 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata":   map[string]interface{}{"name": "web-svc", "namespace": "default"},
		"status": map[string]interface{}{
			"savings": map[string]interface{}{
				"cpuRequestReduction":     "150m",
				"cpuRequestTotal":         "500m",
				"memoryRequestReduction":  "67108864",
				"estimatedMonthlySavings": "$5.22",
			},
		},
	}}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"}, p1, p2)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printSavings(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "TOTAL")
	assert.Contains(t, output, "500m")   // 350m + 150m
	assert.Contains(t, output, "192Mi")  // 128Mi + 64Mi
	assert.Contains(t, output, "$18.00") // $12.78 + $5.22
}

func TestParseDollarCents(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"$12.78", 1278},
		{"$0.50", 50},
		{"$100.00", 10000},
		{"$0.00", 0},
		{"-", 0},
		{"", 0},
		{"invalid", 0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, parseDollarCents(tt.input))
		})
	}
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

	printSavings(context.Background(), dynClient, "default", "")

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

	assert.Contains(t, output, "CONFIDENCE / STATUS")
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

	assert.Contains(t, output, "CONFIDENCE / STATUS")
	assert.Contains(t, output, "new-policy")
	assert.Contains(t, output, "Not enough data")
}

func captureRun(t *testing.T, args []string, buildClient dynamicClientFactory) (int, string, string) {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = stdoutW
	os.Stderr = stderrW

	exitCode := run(args, buildClient)

	require.NoError(t, stdoutW.Close())
	require.NoError(t, stderrW.Close())
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var stdoutBuf bytes.Buffer
	_, err = stdoutBuf.ReadFrom(stdoutR)
	require.NoError(t, err)
	var stderrBuf bytes.Buffer
	_, err = stderrBuf.ReadFrom(stderrR)
	require.NoError(t, err)
	return exitCode, stdoutBuf.String(), stderrBuf.String()
}

func fakeDynamicClientFactory(t *testing.T, objects ...runtime.Object) dynamicClientFactory {
	t.Helper()
	return func(kubeconfigPath, context string) (dynamic.Interface, string, error) {
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{
				gvr:                  "RightSizePolicyList",
				namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
				defaultsGVR:          "RightSizeDefaultsList",
			},
			objects...)
		return client, "default", nil
	}
}

func failingDynamicClientFactory(err error) dynamicClientFactory {
	return func(kubeconfigPath, context string) (dynamic.Interface, string, error) {
		return nil, "", err
	}
}

func TestRun_MainWiring(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "api-svc",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"updateStrategy": map[string]interface{}{
				"type": "Recommend",
			},
		},
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

	tests := []struct {
		name         string
		args         []string
		factory      dynamicClientFactory
		wantExitCode int
		wantStdout   string
		wantStderr   string
		wantNoStderr bool
	}{
		{
			name:         "status json succeeds through main wiring",
			args:         []string{"status", "-o", "json"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 0,
			wantStdout:   "\"kind\": \"RightSizePolicyList\"",
			wantNoStderr: true,
		},
		{
			name:         "status rejects leftover positional args",
			args:         []string{"status", "extra"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "status accepts no positional arguments",
		},
		{
			name:         "explain rejects trailing args after policy name",
			args:         []string{"explain", "api-svc", "extra"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "explain accepts exactly one policy name",
		},
		{
			name:         "savings rejects misleading structured output",
			args:         []string{"savings", "-o", "json"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "supported only with the status command",
		},
		{
			name:         "unsupported output format returns parse-level validation error",
			args:         []string{"status", "-o", "table"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "unsupported output format \"table\"",
		},
		{
			name:         "unknown command exits before client construction",
			args:         []string{"wat"},
			factory:      failingDynamicClientFactory(fmt.Errorf("should not be called")),
			wantExitCode: 1,
			wantStderr:   "Unknown command: wat",
		},
		{
			name:         "watch flag rejected for non-status command",
			args:         []string{"savings", "--watch"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "--watch is supported only with the status command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitCode, stdout, stderr := captureRun(t, tt.args, tt.factory)
			assert.Equal(t, tt.wantExitCode, exitCode)
			if tt.wantStdout != "" {
				assert.Contains(t, stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" {
				assert.Contains(t, stderr, tt.wantStderr)
			}
			if tt.wantNoStderr {
				assert.Empty(t, stderr)
			}
		})
	}
}

func TestIsZeroArgCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{cmd: "status", want: true},
		{cmd: "savings", want: true},
		{cmd: "recommendations", want: true},
		{cmd: "history", want: true},
		{cmd: "explain", want: false},
		{cmd: "version", want: false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, isZeroArgCommand(tt.cmd), tt.cmd)
	}
}

func TestZeroArgCommandArgs(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		args    []string
		wantErr string
	}{
		{
			name: "status accepts no args",
			cmd:  "status",
		},
		{
			name:    "status rejects positional arg",
			cmd:     "status",
			args:    []string{"extra"},
			wantErr: "status accepts no positional arguments",
		},
		{
			name:    "savings rejects positional arg",
			cmd:     "savings",
			args:    []string{"extra"},
			wantErr: "savings accepts no positional arguments",
		},
		{
			name:    "recommendations rejects positional arg",
			cmd:     "recommendations",
			args:    []string{"extra"},
			wantErr: "recommendations accepts no positional arguments",
		},
		{
			name:    "history rejects positional arg",
			cmd:     "history",
			args:    []string{"extra"},
			wantErr: "history accepts no positional arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := zeroArgCommandArgs(tt.cmd, tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestStructuredOutputCommandError(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		output  string
		wantErr string
	}{
		{
			name:   "empty output allowed",
			cmd:    "savings",
			output: "",
		},
		{
			name:   "status supports json",
			cmd:    "status",
			output: "json",
		},
		{
			name:   "status supports yaml",
			cmd:    "status",
			output: "yaml",
		},
		{
			name:    "reject unsupported format",
			cmd:     "status",
			output:  "table",
			wantErr: "unsupported output format",
		},
		{
			name:    "reject savings json",
			cmd:     "savings",
			output:  "json",
			wantErr: "supported only with the status command",
		},
		{
			name:    "reject explain yaml",
			cmd:     "explain",
			output:  "yaml",
			wantErr: "use kubectl get rightsizepolicy -o yaml",
		},
		{
			name:    "reject history json",
			cmd:     "history",
			output:  "json",
			wantErr: "use kubectl get rightsizepolicy -o json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := structuredOutputCommandError(tt.cmd, tt.output)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestExplainPolicyName(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantErr  string
	}{
		{
			name:    "missing policy name",
			args:    nil,
			wantErr: "explain requires a policy name",
		},
		{
			name:     "single policy name",
			args:     []string{"api-services"},
			wantName: "api-services",
		},
		{
			name:    "trailing namespace flag rejected",
			args:    []string{"api-services", "-n", "production"},
			wantErr: "Put flags before the policy name",
		},
		{
			name:    "multiple positional args rejected",
			args:    []string{"api-services", "other-policy"},
			wantErr: "exactly one policy name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := explainPolicyName(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, got)
		})
	}
}

func TestPrintExplain(t *testing.T) {
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
									"memoryRequest": "512Mi",
								},
								"explanation": map[string]interface{}{
									"cpu": map[string]interface{}{
										"rawPercentile":       "200m",
										"overhead":            20.0,
										"afterOverhead":       "240m",
										"confidence":          0.85,
										"confidenceFactor":    4.0,
										"afterConfidence":     "960m",
										"bounds":              map[string]interface{}{"min": "50m", "max": "4000m"},
										"afterBounds":         "960m",
										"minChangePercent":    10.0,
										"maxChangePercent":    50.0,
										"changeFilterApplied": "max_change_capped",
										"afterChangeFilter":   "250m",
										"final":               "250m",
									},
									"memory": map[string]interface{}{
										"rawPercentile":     "256Mi",
										"overhead":          30.0,
										"afterOverhead":     "333Mi",
										"confidence":        0.85,
										"confidenceFactor":  4.0,
										"afterConfidence":   "1332Mi",
										"bounds":            map[string]interface{}{"min": "64Mi", "max": "8Gi"},
										"afterBounds":       "1332Mi",
										"minChangePercent":  10.0,
										"maxChangePercent":  30.0,
										"afterChangeFilter": "512Mi",
										"final":             "512Mi",
										"finalAdjustment":   "memory decrease blocked by allowDecrease=false",
									},
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
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "my-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "Policy: default/my-policy")
	assert.Contains(t, output, "Workload: web-deploy")
	assert.Contains(t, output, "Container: app")
	assert.Contains(t, output, "Raw percentile:              200m")
	assert.Contains(t, output, "Change filter [10.00%, 50.00%]: 250m (max_change_capped)")
	assert.Contains(t, output, "Final adjustment:           memory decrease blocked by allowDecrease=false")
}

func TestPrintExplain_NoRecommendations(t *testing.T) {
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
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "new-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "default/new-policy has no recommendations yet (Not enough data).")
	assert.Contains(t, output, "Effective values:")
	assert.Contains(t, output, "Type: Recommend (source: built-in default, configured: <unset>)")
}

func TestPrintExplain_ShowsPolicyNamespaceAndBuiltInEffectiveValues(t *testing.T) {
	cooldown := "30m"
	queryStep := "10m"
	minimumDataPoints := int64(120)
	maxCPUChangePercent := int64(70)
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "effective-policy",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"updateStrategy": map[string]interface{}{
				"type":                "Auto",
				"cooldown":            cooldown,
				"maxCpuChangePercent": maxCPUChangePercent,
			},
			"metricsSource": map[string]interface{}{
				"queryStep":         queryStep,
				"minimumDataPoints": minimumDataPoints,
			},
		},
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

	nsCooldown := &metav1.Duration{Duration: 45 * time.Minute}
	nsMode := rightsizev1alpha1.UpdateTypeCanary
	nsResizeMethod := rightsizev1alpha1.ResizeMethodInPlaceOrRecreate
	nsDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rightsizev1alpha1.RightSizeNamespaceDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rightsize.io/v1alpha1", Kind: "RightSizeNamespaceDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-defaults", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Type:         nsMode,
				Cooldown:     nsCooldown,
				ResizeMethod: nsResizeMethod,
			},
		},
	})
	require.NoError(t, err)
	nsDefaults := &unstructured.Unstructured{Object: nsDefaultsObj}

	clusterQueryStep := &metav1.Duration{Duration: 2 * time.Minute}
	clusterMaxCPU := int32(80)
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rightsizev1alpha1.RightSizeDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rightsize.io/v1alpha1", Kind: "RightSizeDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource:  &rightsizev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{MaxCPUChangePercent: &clusterMaxCPU},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		},
		policy)
	_, err = dynClient.Resource(namespaceDefaultsGVR).Namespace("default").Create(context.Background(), nsDefaults, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = dynClient.Resource(defaultsGVR).Create(context.Background(), clusterDefaults, metav1.CreateOptions{})
	require.NoError(t, err)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "effective-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "Type: Auto (source: policy, configured: Auto)")
	assert.Contains(t, output, "Cooldown: 30m0s (source: policy, configured: 30m)")
	assert.Contains(t, output, "Query step: 10m0s (source: policy, configured: 10m)")
	assert.Contains(t, output, "Minimum data points: 120 (source: policy, configured: 120)")
	assert.Contains(t, output, "Resize method: InPlaceOrRecreate (source: namespace default, configured: <unset>)")
	assert.Contains(t, output, "Max CPU change: 70% (source: policy, configured: 70%)")
	assert.Contains(t, output, "Max memory change: 30% (source: built-in default, configured: <unset>)")
	assert.Contains(t, output, "Observation period: 5m0s (source: built-in default, configured: <unset>)")
	assert.NotContains(t, output, "source: cluster default")
}

func TestPrintExplain_ObservationPeriodFromCanaryShowsConfigured(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "canary-obs-policy",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"updateStrategy": map[string]interface{}{
				"type": "Canary",
				"canary": map[string]interface{}{
					"percentage":        int64(10),
					"observationPeriod": "10m",
				},
			},
		},
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

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		},
		policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "canary-obs-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// When canary.observationPeriod is set but safetyObservationPeriod is not,
	// the configured value should show the canary period, not <unset>.
	assert.Contains(t, output, "Observation period: 10m0s (source: policy, configured: 10m)")
}

func TestPrintExplain_UsesClusterDefaultsWhenNoNamespaceDefaultsExist(t *testing.T) {
	clusterQueryStep := &metav1.Duration{Duration: 2 * time.Minute}
	clusterMode := rightsizev1alpha1.UpdateTypeAuto
	clusterResizeMethod := rightsizev1alpha1.ResizeMethodInPlaceOrRecreate
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rightsizev1alpha1.RightSizeDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rightsize.io/v1alpha1", Kind: "RightSizeDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource: &rightsizev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Type:         clusterMode,
				ResizeMethod: clusterResizeMethod,
			},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "cluster-default-policy",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InsufficientData",
					"message": "Still collecting",
				},
			},
		},
	}}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		},
		policy)
	_, err = dynClient.Resource(defaultsGVR).Create(context.Background(), clusterDefaults, metav1.CreateOptions{})
	require.NoError(t, err)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "cluster-default-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "Type: Auto (source: cluster default, configured: <unset>)")
	assert.Contains(t, output, "Query step: 2m0s (source: cluster default, configured: <unset>)")
	assert.Contains(t, output, "Resize method: InPlaceOrRecreate (source: cluster default, configured: <unset>)")
}

func TestPrintExplain_NamespaceDefaultsDoNotInheritMissingFieldsFromClusterDefaults(t *testing.T) {
	nsMinimumDataPoints := int32(96)
	nsDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rightsizev1alpha1.RightSizeNamespaceDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rightsize.io/v1alpha1", Kind: "RightSizeNamespaceDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-defaults", Namespace: "default"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource: &rightsizev1alpha1.MetricsSource{MinimumDataPoints: &nsMinimumDataPoints},
		},
	})
	require.NoError(t, err)
	nsDefaults := &unstructured.Unstructured{Object: nsDefaultsObj}

	clusterQueryStep := &metav1.Duration{Duration: 1 * time.Minute}
	clusterMode := rightsizev1alpha1.UpdateTypeAuto
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rightsizev1alpha1.RightSizeDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rightsize.io/v1alpha1", Kind: "RightSizeDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			MetricsSource:  &rightsizev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{Type: clusterMode},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "fallback-policy",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InsufficientData",
					"message": "Still collecting",
				},
			},
		},
	}}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "RightSizePolicyList",
			namespaceDefaultsGVR: "RightSizeNamespaceDefaultsList",
			defaultsGVR:          "RightSizeDefaultsList",
		},
		policy)
	_, err = dynClient.Resource(namespaceDefaultsGVR).Namespace("default").Create(context.Background(), nsDefaults, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = dynClient.Resource(defaultsGVR).Create(context.Background(), clusterDefaults, metav1.CreateOptions{})
	require.NoError(t, err)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printExplain(context.Background(), dynClient, "default", "fallback-policy")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "Minimum data points: 96 (source: namespace default, configured: <unset>)")
	assert.Contains(t, output, "Query step: 5m0s (source: built-in default, configured: <unset>)")
	assert.Contains(t, output, "Type: Recommend (source: built-in default, configured: <unset>)")
	assert.NotContains(t, output, "Query step: 1m0s")
	assert.NotContains(t, output, "Type: Auto")
}

// ---------- policyReadyReason ----------

func TestPolicyReadyReason_NoConditions(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{},
	}}
	assert.Equal(t, "Pending", policyReadyReason(item))
}

func TestPolicyReadyReason_NoWorkloadsFoundWithMessage(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "NoWorkloadsFound",
					"message": "No matching workloads found; check that targetRef name or selector matches an existing workload in this namespace",
				},
			},
		},
	}}
	assert.Equal(t, "No matching workloads found; check that targetRef name or selector matches an existing workload in this namespace", policyReadyReason(item))
}

func TestPolicyReadyReason_ActionableFailureWithMessage(t *testing.T) {
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "PrometheusUnavailable",
					"message": "Cannot create metrics collector: TLS handshake timeout",
				},
			},
		},
	}}
	assert.Equal(t, "Cannot create metrics collector: TLS handshake timeout", policyReadyReason(item))
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

// ---------- printEffectivePolicySummary smoke test ----------

func TestPrintEffectivePolicySummary_DoesNotPanic(t *testing.T) {
	cv := "RequestsOnly"
	autoRevert := true
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &cv,
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				QueryStep:  &metav1.Duration{Duration: 5 * time.Minute},
				RateWindow: &metav1.Duration{Duration: 10 * time.Minute},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Type:                 rightsizev1alpha1.UpdateTypeAuto,
				Cooldown:             &metav1.Duration{Duration: time.Hour},
				AutoRevert:           &autoRevert,
				MaxConcurrentResizes: 5,
			},
		},
	}
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"updateStrategy": map[string]interface{}{
				"type": "Auto",
			},
		},
	}}
	// Should not panic with nil defaults.
	printEffectivePolicySummary(item, policy, selectedDefaults{})

	// Should not panic with non-nil defaults.
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU:            &rightsizev1alpha1.ResourceConfig{Percentile: 90},
			Memory:         &rightsizev1alpha1.ResourceConfig{Percentile: 95},
			MetricsSource:  &rightsizev1alpha1.MetricsSource{},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{Type: rightsizev1alpha1.UpdateTypeAuto},
		},
	}
	printEffectivePolicySummary(item, policy, selectedDefaults{defaults: defaults, source: "cluster"})
}

// ---------- mergeDefaultsIntoPolicy parity with controller ----------

func TestMergeDefaultsIntoPolicy_AllFieldsInherited(t *testing.T) {
	allowDecrease := true
	burstSensitivity := "0.2"
	cv := "RequestsAndLimits"
	boostMultiplier := "3.0"
	boostDuration := metav1.Duration{Duration: 2 * time.Minute}

	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:       90,
				Overhead:         "50",
				ControlledValues: &cv,
				BurstSensitivity: &burstSensitivity,
				AllowDecrease:    &allowDecrease,
				StartupBoost:     &rightsizev1alpha1.StartupBoost{Multiplier: boostMultiplier, Duration: boostDuration},
			},
			Memory: &rightsizev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "40",
				ControlledValues: &cv,
			},
			MetricsSource: &rightsizev1alpha1.MetricsSource{
				HistoryWindow:     &metav1.Duration{Duration: 336 * time.Hour},
				MinimumDataPoints: ptrInt32(96),
				QueryStep:         &metav1.Duration{Duration: 10 * time.Minute},
				RateWindow:        &metav1.Duration{Duration: 15 * time.Minute},
			},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Type:                    rightsizev1alpha1.UpdateTypeAuto,
				Cooldown:                &metav1.Duration{Duration: 30 * time.Minute},
				AutoRevert:              ptrBool(false),
				ResizeMethod:            rightsizev1alpha1.ResizeMethodInPlaceOrRecreate,
				MaxCPUChangePercent:     ptrInt32(80),
				MaxMemoryChangePercent:  ptrInt32(40),
				SafetyObservationPeriod: &metav1.Duration{Duration: 10 * time.Minute},
				MaxConcurrentResizes:    5,
				Schedule:                &rightsizev1alpha1.ResizeSchedule{Timezone: "UTC"},
				Export:                  &rightsizev1alpha1.ExportConfig{ConfigMap: true},
				Canary:                  &rightsizev1alpha1.CanaryConfig{Percentage: 10, ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute}},
			},
		},
	}

	policy := &rightsizev1alpha1.RightSizePolicy{}
	mergeDefaultsIntoPolicy(policy, defaults)

	// CPU resource config
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "50", policy.Spec.CPU.Overhead)
	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, "RequestsAndLimits", *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.CPU.BurstSensitivity)
	assert.Equal(t, "0.2", *policy.Spec.CPU.BurstSensitivity)
	require.NotNil(t, policy.Spec.CPU.AllowDecrease)
	assert.True(t, *policy.Spec.CPU.AllowDecrease)
	require.NotNil(t, policy.Spec.CPU.StartupBoost)
	assert.Equal(t, "3.0", policy.Spec.CPU.StartupBoost.Multiplier)

	// Memory resource config
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "40", policy.Spec.Memory.Overhead)
	require.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, "RequestsAndLimits", *policy.Spec.Memory.ControlledValues)

	// MetricsSource
	require.NotNil(t, policy.Spec.MetricsSource.HistoryWindow)
	assert.Equal(t, 336*time.Hour, policy.Spec.MetricsSource.HistoryWindow.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.MinimumDataPoints)
	assert.Equal(t, int32(96), *policy.Spec.MetricsSource.MinimumDataPoints)
	require.NotNil(t, policy.Spec.MetricsSource.QueryStep)
	assert.Equal(t, 10*time.Minute, policy.Spec.MetricsSource.QueryStep.Duration)
	require.NotNil(t, policy.Spec.MetricsSource.RateWindow)
	assert.Equal(t, 15*time.Minute, policy.Spec.MetricsSource.RateWindow.Duration)

	// UpdateStrategy
	assert.Equal(t, rightsizev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, rightsizev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.UpdateStrategy.MaxCPUChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
	assert.Equal(t, int32(40), *policy.Spec.UpdateStrategy.MaxMemoryChangePercent)
	require.NotNil(t, policy.Spec.UpdateStrategy.SafetyObservationPeriod)
	assert.Equal(t, 10*time.Minute, policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration)
	assert.Equal(t, int32(5), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	require.NotNil(t, policy.Spec.UpdateStrategy.Schedule)
	assert.Equal(t, "UTC", policy.Spec.UpdateStrategy.Schedule.Timezone)
	require.NotNil(t, policy.Spec.UpdateStrategy.Export)
	assert.True(t, policy.Spec.UpdateStrategy.Export.ConfigMap)
	require.NotNil(t, policy.Spec.UpdateStrategy.Canary)
	assert.Equal(t, int32(10), policy.Spec.UpdateStrategy.Canary.Percentage)
}

func TestMergeDefaultsIntoPolicy_PolicyFieldsNotOverwritten(t *testing.T) {
	cv := "RequestsAndLimits"
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:       50,
				Overhead:         "100",
				ControlledValues: &cv,
			},
			UpdateStrategy: &rightsizev1alpha1.UpdateStrategy{
				Type:                 rightsizev1alpha1.UpdateTypeAuto,
				MaxConcurrentResizes: 10,
			},
			MetricsSource: &rightsizev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 20 * time.Minute},
			},
		},
	}

	policyCV := "RequestsOnly"
	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &policyCV,
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Type:                 rightsizev1alpha1.UpdateTypeRecommend,
				MaxConcurrentResizes: 3,
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 5 * time.Minute},
			},
		},
	}
	mergeDefaultsIntoPolicy(policy, defaults)

	// Policy fields should be preserved.
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	assert.Equal(t, "RequestsOnly", *policy.Spec.CPU.ControlledValues)
	assert.Equal(t, rightsizev1alpha1.UpdateTypeRecommend, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(3), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	assert.Equal(t, 5*time.Minute, policy.Spec.MetricsSource.RateWindow.Duration)
}

func TestApplyBuiltInDefaults_SetsControlledValues(t *testing.T) {
	policy := &rightsizev1alpha1.RightSizePolicy{}
	applyBuiltInDefaults(policy)

	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, rightsizev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, rightsizev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
}

func ptrInt32(v int32) *int32 { return &v }
func ptrBool(v bool) *bool    { return &v }
