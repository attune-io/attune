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
	"io"
	"os"
	"strings"
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

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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

func TestPrintPreview_StaleSkipped(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
						"stale":    true,
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "256Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "256Mi"},
							},
						},
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = outW, errW

	printPreview(context.Background(), dynClient, "default", "web-app")

	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	os.Stdout, os.Stderr = oldOut, oldErr

	var outBuf, errBuf bytes.Buffer
	_, err = outBuf.ReadFrom(outR)
	require.NoError(t, err)
	_, err = errBuf.ReadFrom(errR)
	require.NoError(t, err)

	assert.NotContains(t, outBuf.String(), "500m")
	assert.NotContains(t, outBuf.String(), "250m")
	assert.Contains(t, errBuf.String(), "stale")
	assert.Contains(t, errBuf.String(), "web-deploy")
}

func TestPrintHistory(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
			gvr: "AttunePolicyList",
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
			gvr: "AttunePolicyList",
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
			gvr: "AttunePolicyList",
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
			"metadata": map[string]interface{}{
				"name":              "web-app",
				"namespace":         "production",
				"creationTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"type": "Auto",
					"export": map[string]interface{}{
						"configMap": true,
					},
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
	assert.Contains(t, output, "EXPORT")
	assert.Regexp(t, `(?m)production\s+web-app\s+.*\sCM\s`, output)
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
			name:       "prometheus unavailable alias actionable message",
			reason:     "PrometheusUnavailable",
			status:     "False",
			message:    "Cannot create metrics collector: TLS handshake timeout",
			wantOutput: "Cannot create metrics collector: TLS handshake timeout",
		},
		{
			name:       "metrics unavailable actionable message",
			reason:     "MetricsUnavailable",
			status:     "False",
			message:    "Cannot resolve metrics source: reading secret attune/dd-key",
			wantOutput: "Cannot resolve metrics source: reading secret attune/dd-key",
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
					"apiVersion": "attune.io/v1alpha1",
					"kind":       "AttunePolicy",
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
				map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"})

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

	assert.Contains(t, output, "No AttunePolicies found")
}

func TestPrintStatus_FilterDegraded(t *testing.T) {
	degraded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, degraded, healthy)

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

func TestSortPolicies_ByName(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "charlie"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "alpha"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "bravo"}}},
	}
	sortPolicies(items, "name")
	assert.Equal(t, "alpha", items[0].GetName())
	assert.Equal(t, "bravo", items[1].GetName())
	assert.Equal(t, "charlie", items[2].GetName())
}

func TestSortPolicies_ByNamespace(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "b", "namespace": "prod"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "a", "namespace": "prod"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "c", "namespace": "dev"}}},
	}
	sortPolicies(items, "namespace")
	// dev < prod, then by name within same namespace.
	assert.Equal(t, "c", items[0].GetName())
	assert.Equal(t, "a", items[1].GetName())
	assert.Equal(t, "b", items[2].GetName())
}

func TestSortPolicies_ByAge(t *testing.T) {
	now := time.Now()
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":              "newer",
				"creationTimestamp": now.Add(-1 * time.Hour).Format(time.RFC3339),
			},
		}},
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":              "oldest",
				"creationTimestamp": now.Add(-24 * time.Hour).Format(time.RFC3339),
			},
		}},
	}
	sortPolicies(items, "age")
	assert.Equal(t, "oldest", items[0].GetName())
	assert.Equal(t, "newer", items[1].GetName())
}

func TestSortPolicies_UnknownKey(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "b"}}},
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "a"}}},
	}
	sortPolicies(items, "unknown")
	// Unknown key is a no-op; order preserved.
	assert.Equal(t, "b", items[0].GetName())
	assert.Equal(t, "a", items[1].GetName())
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
		{
			name: "canary mixed per-app promote",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Canary"}},
				"status": map[string]interface{}{
					"canary": map[string]interface{}{
						"phase": "CanaryInProgress",
						"workloads": []interface{}{
							map[string]interface{}{"workload": "a", "phase": "FullRollout"},
							map[string]interface{}{"workload": "b", "phase": "CanaryInProgress"},
						},
					},
				},
			},
			expected: "CanaryInProgress (1/2 apps)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := unstructured.Unstructured{Object: tt.obj}
			assert.Equal(t, tt.expected, formatCanaryStatus(item))
		})
	}
}

func TestFormatExportMode(t *testing.T) {
	tests := []struct {
		name     string
		obj      map[string]interface{}
		expected string
	}{
		{
			name: "no export",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "Auto"}},
			},
			expected: "-",
		},
		{
			name: "export disabled explicitly",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"updateStrategy": map[string]interface{}{
						"type":   "Recommend",
						"export": map[string]interface{}{"configMap": false},
					},
				},
			},
			expected: "-",
		},
		{
			name: "export enabled for GitOps",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"updateStrategy": map[string]interface{}{
						"type":   "Recommend",
						"export": map[string]interface{}{"configMap": true},
					},
				},
			},
			expected: "CM",
		},
		{
			name: "export with Auto mode (resizes + export)",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"updateStrategy": map[string]interface{}{
						"type":   "Auto",
						"export": map[string]interface{}{"configMap": true},
					},
				},
			},
			expected: "CM",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := unstructured.Unstructured{Object: tt.obj}
			assert.Equal(t, tt.expected, formatExportMode(item))
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

func TestFilterPolicies_ConflictCheckFailed(t *testing.T) {
	// policyReadyReason returns the Ready=False message, which does not
	// contain "conflictcheckfailed". Matching only that display string
	// (main-branch filterPolicies) misses this policy.
	conflict := *conflictCheckFailedPolicy("conflict-policy", "default", nil, nil)
	collecting := unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "collecting-policy", "namespace": "default"},
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
	}}
	invalid := unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "invalid-policy", "namespace": "default"},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InvalidConfig",
					"message": "cpu.maxAllowed must be greater than minAllowed",
				},
			},
		},
	}}
	items := []unstructured.Unstructured{conflict, collecting, invalid}

	got := filterPolicies(items, "conflictcheckfailed")
	require.Len(t, got, 1)
	assert.Equal(t, "conflict-policy", got[0].GetName())

	got = filterPolicies(items, "collecting")
	require.Len(t, got, 1)
	assert.Equal(t, "collecting-policy", got[0].GetName())

	got = filterPolicies(items, "invalidconfig")
	require.Len(t, got, 1)
	assert.Equal(t, "invalid-policy", got[0].GetName())
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
	assert.Equal(t, "AttunePolicyList", parsed["kind"])
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"})

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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, p1, p2)

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
		{"NaN", 0},
		{"Inf", 0},
		{"-Inf", 0},
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
			"metadata": map[string]interface{}{
				"name":      "fresh-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{},
		},
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
	// Empty savings cells render as table placeholders for each metric column.
	assert.Regexp(t, `fresh-policy\s+-\s+-\s+-\s+-`, output)
}

// ---------- wasteGrade ----------

func TestWasteGrade(t *testing.T) {
	tests := []struct {
		name           string
		curCPU, recCPU string
		curMem, recMem string
		want           string
	}{
		{name: "A under 10 percent cpu", curCPU: "105m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "A"},
		{name: "A exact match", curCPU: "100m", recCPU: "100m", curMem: "256Mi", recMem: "256Mi", want: "A"},
		{name: "A within 10 percent under", curCPU: "240m", recCPU: "250m", curMem: "100Mi", recMem: "100Mi", want: "A"},
		{name: "A at 10 percent under", curCPU: "90m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "A"},
		{name: "U just beyond 10 percent under", curCPU: "89m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "U"},
		{name: "U under-provisioned", curCPU: "80m", recCPU: "100m", curMem: "128Mi", recMem: "256Mi", want: "U"},
		{name: "U zero request", curCPU: "0", recCPU: "250m", curMem: "0", recMem: "512Mi", want: "U"},
		{name: "U scale-up", curCPU: "100m", recCPU: "2000m", curMem: "128Mi", recMem: "128Mi", want: "U"},
		{name: "U wins cpu over mem under", curCPU: "180m", recCPU: "100m", curMem: "40Mi", recMem: "100Mi", want: "U"},
		{name: "U wins cpu under mem over", curCPU: "40m", recCPU: "100m", curMem: "180Mi", recMem: "100Mi", want: "U"},
		{name: "B at 10 percent", curCPU: "110m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "B"},
		{name: "B just under 25 percent", curCPU: "124m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "B"},
		{name: "C at 25 percent", curCPU: "125m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "C"},
		{name: "C just under 50 percent", curCPU: "149m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "C"},
		{name: "D at 50 percent", curCPU: "150m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "D"},
		{name: "D just under 75 percent", curCPU: "174m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "D"},
		{name: "F at 75 percent", curCPU: "175m", recCPU: "100m", curMem: "100Mi", recMem: "100Mi", want: "F"},
		{name: "F double request", curCPU: "500m", recCPU: "250m", curMem: "256Mi", recMem: "128Mi", want: "F"},
		{name: "worse of cpu and memory", curCPU: "105m", recCPU: "100m", curMem: "200Mi", recMem: "100Mi", want: "F"},
		{name: "memory only when cpu missing", curCPU: "", recCPU: "", curMem: "125Mi", recMem: "100Mi", want: "C"},
		{name: "cpu only when memory missing", curCPU: "110m", recCPU: "100m", curMem: "", recMem: "", want: "B"},
		{name: "mixed units 1Gi vs 512Mi", curCPU: "100m", recCPU: "100m", curMem: "1Gi", recMem: "512Mi", want: "F"},
		{name: "cores vs millicores", curCPU: "1", recCPU: "500m", curMem: "100Mi", recMem: "100Mi", want: "F"},
		{name: "collecting empty", curCPU: "", recCPU: "", curMem: "", recMem: "", want: "-"},
		{name: "missing recommended", curCPU: "500m", recCPU: "", curMem: "256Mi", recMem: "", want: "-"},
		{name: "unparseable", curCPU: "not-a-qty", recCPU: "100m", curMem: "nope", recMem: "128Mi", want: "-"},
		{name: "zero recommended", curCPU: "100m", recCPU: "0", curMem: "", recMem: "", want: "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, wasteGrade(tt.curCPU, tt.recCPU, tt.curMem, tt.recMem))
		})
	}
}

func TestRecommendationGrade_StaleOverridesWaste(t *testing.T) {
	rec := map[string]interface{}{"stale": true}
	assert.Equal(t, "-", recommendationGrade(rec, "0", "250m", "0", "512Mi"))
	assert.Equal(t, "-", recommendationGrade(rec, "500m", "250m", "256Mi", "128Mi"))
	fresh := map[string]interface{}{"stale": false}
	assert.Equal(t, "U", recommendationGrade(fresh, "0", "250m", "0", "512Mi"))
	assert.Equal(t, "F", recommendationGrade(map[string]interface{}{}, "500m", "250m", "256Mi", "128Mi"))
}

func TestPrintRecommendations(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
	assert.Contains(t, output, "GRADE")
	assert.Contains(t, output, "web-deploy")
	assert.Contains(t, output, "app")
	assert.Contains(t, output, "500m")
	assert.Contains(t, output, "250m")
	assert.Contains(t, output, "85.0%")
	assert.Regexp(t, `(?m)\bF\b`, output)
}

func TestPrintRecommendations_UnderProvisionedGradeU(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
			"metadata": map[string]interface{}{
				"name":      "web",
				"namespace": "prod",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api",
						"containers": []interface{}{
							map[string]interface{}{
								"name":       "app",
								"confidence": 0.92,
								"current": map[string]interface{}{
									"cpuRequest":    "0",
									"memoryRequest": "0",
								},
								"recommended": map[string]interface{}{
									"cpuRequest":    "250m",
									"memoryRequest": "512Mi",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printRecommendations(context.Background(), dynClient, "prod")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "GRADE")
	assert.Contains(t, output, "250m")
	assert.Regexp(t, `(?m)\bU\b`, output)
	assert.NotRegexp(t, `(?m)\bA\b`, output)
}

func TestPrintRecommendations_StaleGradeDash(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
			"metadata": map[string]interface{}{
				"name":      "web",
				"namespace": "prod",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api",
						"stale":    true,
						"containers": []interface{}{
							map[string]interface{}{
								"name":       "app",
								"confidence": 0.92,
								"current": map[string]interface{}{
									"cpuRequest":    "0",
									"memoryRequest": "0",
								},
								"recommended": map[string]interface{}{
									"cpuRequest":    "250m",
									"memoryRequest": "512Mi",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printRecommendations(context.Background(), dynClient, "prod")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "250m")
	assert.Regexp(t, `(?m)\s-\s+stale\b`, output)
	assert.NotRegexp(t, `(?m)\bU\b`, output)
	assert.NotContains(t, output, "92.0%")
}

func TestPrintRecommendations_CollectingData(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

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
	assert.Contains(t, output, "GRADE")
	assert.Contains(t, output, "new-policy")
	assert.Contains(t, output, "Not enough data")
	assert.Regexp(t, `(?m)new-policy\s+-\s+-\s+-\s+-\s+-\s+-\s+-\s+Not enough data`, output)
}

func conflictCheckFailedPolicy(name, ns string, recs []interface{}, extraErrors []interface{}) *unstructured.Unstructured {
	errors := []interface{}{
		map[string]interface{}{
			"workload": "*",
			"error":    "Failed to list AttunePolicies for conflict detection; recommendations not computed",
		},
	}
	errors = append(errors, extraErrors...)
	status := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":    "Ready",
				"status":  "False",
				"reason":  attunev1alpha1.ReasonConflictCheckFailed,
				"message": "Failed to list AttunePolicies for conflict detection; recommendations not computed",
			},
		},
		"workloadErrors": errors,
	}
	if recs != nil {
		status["recommendations"] = recs
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"status":     status,
	}}
}

func captureStdoutStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = outW, errW
	fn()
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	os.Stdout, os.Stderr = oldOut, oldErr
	var outBuf, errBuf bytes.Buffer
	_, err = outBuf.ReadFrom(outR)
	require.NoError(t, err)
	_, err = errBuf.ReadFrom(errR)
	require.NoError(t, err)
	return outBuf.String(), errBuf.String()
}

func TestPrintRecommendations_ConflictCheckFailedEmptyRecs(t *testing.T) {
	policy := conflictCheckFailedPolicy("web", "default", nil, nil)
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

	stdout, stderr := captureStdoutStderr(t, func() {
		printRecommendations(context.Background(), dynClient, "default")
	})

	assert.Contains(t, stdout, "web")
	assert.Contains(t, stdout, "Failed to list AttunePolicies for conflict detection")
	assert.NotContains(t, stderr, "collecting data")
	assert.Contains(t, stderr, "Warning: default/web Ready=False (ConflictCheckFailed)")
	assert.Contains(t, stderr, "See docs/guides/troubleshooting.md#conflictcheckfailed")
}

func TestPrintRecommendations_ConflictCheckFailedWithRecs(t *testing.T) {
	recs := []interface{}{
		map[string]interface{}{
			"workload": "api",
			"containers": []interface{}{
				map[string]interface{}{
					"name":       "app",
					"confidence": 0.9,
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
	}
	policy := conflictCheckFailedPolicy("web", "default", recs, nil)
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "AttunePolicyList"}, policy)

	stdout, stderr := captureStdoutStderr(t, func() {
		printRecommendations(context.Background(), dynClient, "default")
	})

	assert.Contains(t, stdout, "api")
	assert.Contains(t, stdout, "250m")
	assert.Contains(t, stdout, "90.0%")
	assert.NotContains(t, stderr, "collecting data")
	assert.Contains(t, stderr, "Warning: default/web Ready=False (ConflictCheckFailed)")
	assert.Contains(t, stderr, "Failed to list AttunePolicies for conflict detection")
	assert.Contains(t, stderr, "See docs/guides/troubleshooting.md#conflictcheckfailed")
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
				gvr:                  "AttunePolicyList",
				namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
				defaultsGVR:          "AttuneDefaultsList",
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
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
			wantStdout:   "\"kind\": \"AttunePolicyList\"",
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
			name:         "doctor rejects leftover positional args",
			args:         []string{"doctor", "extra"},
			factory:      fakeDynamicClientFactory(t, policy),
			wantExitCode: 1,
			wantStderr:   "doctor accepts no positional arguments",
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
		{cmd: "doctor", want: true},
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
		{
			name:    "doctor rejects positional arg",
			cmd:     "doctor",
			args:    []string{"extra"},
			wantErr: "doctor accepts no positional arguments",
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
			name:   "savings supports csv",
			cmd:    "savings",
			output: "csv",
		},
		{
			name:   "recommendations supports csv",
			cmd:    "recommendations",
			output: "csv",
		},
		{
			name:    "reject status csv",
			cmd:     "status",
			output:  "csv",
			wantErr: "csv is supported only with savings and recommendations",
		},
		{
			name:    "reject explain yaml",
			cmd:     "explain",
			output:  "yaml",
			wantErr: "use kubectl get attunepolicy -o yaml",
		},
		{
			name:    "reject history json",
			cmd:     "history",
			output:  "json",
			wantErr: "use kubectl get attunepolicy -o json",
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
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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

func TestPrintExplain_StaleNote(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
			"metadata": map[string]interface{}{
				"name":      "my-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "web-deploy",
						"stale":    true,
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"confidence":  0.85,
								"current":     map[string]interface{}{"cpuRequest": "500m"},
								"recommended": map[string]interface{}{"cpuRequest": "250m"},
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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
	assert.Contains(t, buf.String(), "stale (no fresh Prometheus data")
}

func TestPrintExplain_NoRecommendations(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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
	assert.Contains(t, output, "Metrics source: prometheus (auto-discover)")
}

func TestPrintExplain_ConflictCheckFailedEmptyRecs(t *testing.T) {
	policy := conflictCheckFailedPolicy("web", "default", nil, nil)
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
		}, policy)

	stdout, stderr := captureStdoutStderr(t, func() {
		printExplain(context.Background(), dynClient, "default", "web")
	})

	assert.Contains(t, stdout, "default/web has no recommendations (ConflictCheckFailed): Failed to list AttunePolicies for conflict detection; recommendations not computed")
	assert.NotContains(t, stdout, "no recommendations yet")
	assert.Contains(t, stdout, "Effective values:")
	assert.Contains(t, stderr, "Warning: default/web Ready=False (ConflictCheckFailed)")
	assert.Contains(t, stderr, "See docs/guides/troubleshooting.md#conflictcheckfailed")
}

func TestPrintExplain_ConflictCheckFailedWithRecs(t *testing.T) {
	recs := []interface{}{
		map[string]interface{}{
			"workload": "api",
			"containers": []interface{}{
				map[string]interface{}{
					"name":        "app",
					"confidence":  0.9,
					"current":     map[string]interface{}{"cpuRequest": "500m"},
					"recommended": map[string]interface{}{"cpuRequest": "250m"},
				},
			},
		},
	}
	policy := conflictCheckFailedPolicy("web", "default", recs, nil)
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
		}, policy)

	stdout, stderr := captureStdoutStderr(t, func() {
		printExplain(context.Background(), dynClient, "default", "web")
	})

	assert.Contains(t, stdout, "Policy: default/web")
	assert.Contains(t, stdout, "Workload: api")
	assert.Contains(t, stdout, "Container: app")
	assert.Contains(t, stderr, "Warning: default/web Ready=False (ConflictCheckFailed)")
	assert.Contains(t, stderr, "See docs/guides/troubleshooting.md#conflictcheckfailed")
}

func TestPrintExplain_ShowsPolicyNamespaceAndBuiltInEffectiveValues(t *testing.T) {
	cooldown := "30m"
	queryStep := "10m"
	minimumDataPoints := int64(120)
	maxCPUChangePercent := int64(70)
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
		"metadata": map[string]interface{}{
			"name":      "effective-policy",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"cpu": map[string]interface{}{
				"maxChangePercent": maxCPUChangePercent,
			},
			"updateStrategy": map[string]interface{}{
				"type":     "Auto",
				"cooldown": cooldown,
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
	nsMode := attunev1alpha1.UpdateTypeCanary
	nsResizeMethod := attunev1alpha1.ResizeMethodInPlaceOrRecreate
	nsDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneNamespaceDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneNamespaceDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-defaults", Namespace: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
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
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			CPU:           &attunev1alpha1.ResourceConfig{MaxChangePercent: &clusterMaxCPU},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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
	// Both cluster and namespace defaults exist → merged source label (3-tier).
	assert.Contains(t, output, "Resize method: InPlaceOrRecreate (source: merged defaults (namespace+cluster), configured: <unset>)")
	assert.Contains(t, output, "Max change: 70% (source: policy, configured: 70%)")
	assert.Contains(t, output, "Max change: 30% (source: built-in default, configured: <unset>)")
	assert.Contains(t, output, "Observation period: 5m0s (source: built-in default, configured: <unset>)")
	assert.NotContains(t, output, "source: cluster default")
}

func TestPrintExplain_ObservationPeriodFromCanaryShowsConfigured(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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
	clusterMode := attunev1alpha1.UpdateTypeAuto
	clusterResizeMethod := attunev1alpha1.ResizeMethodInPlaceOrRecreate
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:         clusterMode,
				ResizeMethod: clusterResizeMethod,
			},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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

func TestPrintExplain_NamespaceDefaultsInheritMissingFieldsFromClusterDefaults(t *testing.T) {
	// Issue #394: 3-tier merge — namespace min data points; cluster fills queryStep and type.
	nsMinimumDataPoints := int32(96)
	nsDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneNamespaceDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneNamespaceDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-defaults", Namespace: "default"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{MinimumDataPoints: &nsMinimumDataPoints},
		},
	})
	require.NoError(t, err)
	nsDefaults := &unstructured.Unstructured{Object: nsDefaultsObj}

	clusterQueryStep := &metav1.Duration{Duration: 1 * time.Minute}
	clusterMode := attunev1alpha1.UpdateTypeAuto
	clusterDefaultsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource:  &attunev1alpha1.MetricsSource{QueryStep: clusterQueryStep},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: clusterMode},
		},
	})
	require.NoError(t, err)
	clusterDefaults := &unstructured.Unstructured{Object: clusterDefaultsObj}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
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
			gvr:                  "AttunePolicyList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
			defaultsGVR:          "AttuneDefaultsList",
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

	assert.Contains(t, output, "Minimum data points: 96 (source: merged defaults (namespace+cluster), configured: <unset>)")
	// Cluster fills gaps left by the sparse namespace object (3-tier merge).
	assert.Contains(t, output, "Query step: 1m0s (source: merged defaults (namespace+cluster), configured: <unset>)")
	assert.Contains(t, output, "Type: Auto (source: merged defaults (namespace+cluster), configured: <unset>)")
	assert.NotContains(t, output, "Query step: 5m0s (source: built-in default")
	assert.NotContains(t, output, "Type: Recommend (source: built-in default")
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
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &cv,
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
			},
			MetricsSource: attunev1alpha1.MetricsSource{
				QueryStep:  &metav1.Duration{Duration: 5 * time.Minute},
				RateWindow: &metav1.Duration{Duration: 10 * time.Minute},
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                 attunev1alpha1.UpdateTypeAuto,
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
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU:            &attunev1alpha1.ResourceConfig{Percentile: 90},
			Memory:         &attunev1alpha1.ResourceConfig{Percentile: 95},
			MetricsSource:  &attunev1alpha1.MetricsSource{},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}
	printEffectivePolicySummary(item, policy, selectedDefaults{defaults: defaults, source: "cluster"})
}

func TestPrintEffectivePolicySummary_CostPricing(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}

	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{})
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "CPU per core-hour: 0.031 (source: built-in default, configured: <unset>)")
	assert.Contains(t, s, "Memory per GiB-hour: 0.004 (source: built-in default, configured: <unset>)")

	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{
				CPUPerCoreHour:   "0.05",
				MemoryPerGiBHour: "0.009",
			},
		},
	}
	r, w, err = os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{defaults: defaults, source: sourceCluster})
	_ = w.Close()
	os.Stdout = old
	out, err = io.ReadAll(r)
	require.NoError(t, err)
	s = string(out)
	assert.Contains(t, s, "CPU per core-hour: 0.05 (source: cluster default, configured: <unset>)")
	assert.Contains(t, s, "Memory per GiB-hour: 0.009 (source: cluster default, configured: <unset>)")

	// Per-field inherit: namespace can set CPU only; memory stays built-in.
	cpuOnly := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CostPricing: &attunev1alpha1.CostPricing{CPUPerCoreHour: "0.01"},
		},
	}
	r, w, err = os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{defaults: cpuOnly, source: sourceNamespace})
	_ = w.Close()
	os.Stdout = old
	out, err = io.ReadAll(r)
	require.NoError(t, err)
	s = string(out)
	assert.Contains(t, s, "CPU per core-hour: 0.01 (source: namespace default, configured: <unset>)")
	assert.Contains(t, s, "Memory per GiB-hour: 0.004 (source: built-in default, configured: <unset>)")
}

func TestPrintEffectivePolicySummary_PodAggregationDefault(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
			// PodAggregation intentionally empty: explain should show Max default.
		},
	}
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{})
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Contains(t, string(out), "Pod aggregation")
	assert.Contains(t, string(out), attunev1alpha1.DefaultPodAggregation)
}

func TestPrintEffectivePolicySummary_StatusBudgetFields(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
		},
	}
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}

	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{})
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "Max status recommendations: 100 (source: built-in default, configured: <unset>)")
	assert.Contains(t, s, "Include explanations in status: true (source: built-in default, configured: <unset>)")

	maxRecs := int32(50)
	include := false
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				MaxStatusRecommendations:    &maxRecs,
				IncludeExplanationsInStatus: &include,
			},
		},
	}
	inherited := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                        attunev1alpha1.UpdateTypeAuto,
				MaxStatusRecommendations:    &maxRecs,
				IncludeExplanationsInStatus: &include,
			},
		},
	}
	r, w, err = os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printEffectivePolicySummary(item, inherited, selectedDefaults{defaults: defaults, source: sourceCluster})
	_ = w.Close()
	os.Stdout = old
	out, err = io.ReadAll(r)
	require.NoError(t, err)
	s = string(out)
	assert.Contains(t, s, "Max status recommendations: 50 (source: cluster default, configured: <unset>)")
	assert.Contains(t, s, "Include explanations in status: false (source: cluster default, configured: <unset>)")
}

func TestPrintSavingsCSV_HeaderAndRow(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "api", "namespace": "prod"},
			"status": map[string]interface{}{
				"savings": map[string]interface{}{
					"cpuRequestReduction":     "350m",
					"cpuRequestTotal":         "1",
					"memoryRequestReduction":  "134217728",
					"estimatedMonthlySavings": "$12.78",
				},
			},
		}},
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printSavingsCSV(items, "")
	require.NoError(t, w.Close())
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "namespace,name,cpu_saved,memory_saved,pct_saved,est_monthly")
	assert.Contains(t, s, "prod,api,350m")
	assert.Contains(t, s, "$12.78")
	assert.NotContains(t, s, "TOTAL")
}

func TestPrintRecommendationsCSV_HeaderAndRow(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "p", "namespace": "ns"},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "256Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "128Mi"},
								"confidence":  0.95,
							},
						},
					},
				},
			},
		}},
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printRecommendationsCSV(items)
	require.NoError(t, w.Close())
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "namespace,policy,workload,container,cpu_req,cpu_rec,mem_req,mem_rec,grade,confidence_or_status")
	assert.Contains(t, s, "ns,p,api,app,500m,250m,256Mi,128Mi,F,95.0%")
}

func TestPrintRecommendationsCSV_UnderProvisionedGradeU(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "p", "namespace": "ns"},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "0", "memoryRequest": "0"},
								"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "512Mi"},
								"confidence":  0.92,
							},
						},
					},
				},
			},
		}},
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printRecommendationsCSV(items)
	require.NoError(t, w.Close())
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "ns,p,api,app,0,250m,0,512Mi,U,92.0%")
	assert.NotContains(t, s, ",A,")
}

func TestPrintRecommendationsCSV_StaleGradeDash(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "p", "namespace": "ns"},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api",
						"stale":    true,
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "0", "memoryRequest": "0"},
								"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "512Mi"},
								"confidence":  0.92,
							},
						},
					},
				},
			},
		}},
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printRecommendationsCSV(items)
	require.NoError(t, w.Close())
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "ns,p,api,app,0,250m,0,512Mi,-,stale")
	assert.NotContains(t, s, ",U,")
	assert.NotContains(t, s, "92.0%")
}

func TestPrintRecommendationsCSV_EmptyRecsUseReadyReason(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "p", "namespace": "ns"},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{"type": "Ready", "status": "False", "reason": "InsufficientData"},
				},
			},
		}},
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printRecommendationsCSV(items)
	require.NoError(t, w.Close())
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "namespace,policy,workload,container,cpu_req,cpu_rec,mem_req,mem_rec,grade,confidence_or_status")
	assert.Contains(t, s, "ns,p,,,,,,,-,InsufficientData")
	assert.NotContains(t, s, ",confidence\n")
}

func TestPrintEffectivePolicySummary_GitOpsPR(t *testing.T) {
	en := true
	dry := true
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeRecommend,
				Export: &attunev1alpha1.ExportConfig{
					ConfigMap: true,
					PullRequest: &attunev1alpha1.GitOpsPullRequestConfig{
						Enabled:    &en,
						DryRun:     &dry,
						Provider:   "github",
						Repository: "org/app",
					},
				},
			},
		},
	}
	item := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"updateStrategy": map[string]interface{}{
				"export": map[string]interface{}{
					"configMap": true,
					"pullRequest": map[string]interface{}{
						"enabled": true,
					},
				},
			},
		},
		"status": map[string]interface{}{
			"gitopsPR": map[string]interface{}{
				"url":              "https://github.com/org/app/pull/9",
				"driftFingerprint": "deadbeef",
			},
		},
	}}
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	printEffectivePolicySummary(item, policy, selectedDefaults{})
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "GitOps PR")
	assert.Contains(t, s, "github org/app (dry-run)")
	assert.Contains(t, s, "https://github.com/org/app/pull/9")
	assert.Contains(t, s, "deadbeef")
}

// ---------- mergeDefaultsIntoPolicy parity with controller ----------

func TestMergeDefaultsIntoPolicy_AllFieldsInherited(t *testing.T) {
	allowDecrease := true
	burstSensitivity := "0.2"
	cv := "RequestsAndLimits"
	boostMultiplier := "3.0"
	boostDuration := metav1.Duration{Duration: 2 * time.Minute}

	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile:       90,
				Overhead:         "50",
				ControlledValues: &cv,
				BurstSensitivity: &burstSensitivity,
				AllowDecrease:    &allowDecrease,
				StartupBoost:     &attunev1alpha1.StartupBoost{Multiplier: boostMultiplier, Duration: boostDuration},
				MaxChangePercent: ptrInt32(80),
			},
			Memory: &attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "40",
				ControlledValues: &cv,
				MaxChangePercent: ptrInt32(40),
			},
			MetricsSource: &attunev1alpha1.MetricsSource{
				HistoryWindow:     &metav1.Duration{Duration: 336 * time.Hour},
				MinimumDataPoints: ptrInt32(96),
				QueryStep:         &metav1.Duration{Duration: 10 * time.Minute},
				RateWindow:        &metav1.Duration{Duration: 15 * time.Minute},
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                    attunev1alpha1.UpdateTypeAuto,
				Cooldown:                &metav1.Duration{Duration: 30 * time.Minute},
				AutoRevert:              ptrBool(false),
				ResizeMethod:            attunev1alpha1.ResizeMethodInPlaceOrRecreate,
				SafetyObservationPeriod: &metav1.Duration{Duration: 10 * time.Minute},
				MaxConcurrentResizes:    5,
				Schedule:                &attunev1alpha1.ResizeSchedule{Timezone: "UTC"},
				Export:                  &attunev1alpha1.ExportConfig{ConfigMap: true},
				Canary:                  &attunev1alpha1.CanaryConfig{Percentage: 10, ObservationPeriod: metav1.Duration{Duration: 5 * time.Minute}},
			},
		},
	}

	policy := &attunev1alpha1.AttunePolicy{}
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
	assert.Equal(t, attunev1alpha1.UpdateTypeAuto, policy.Spec.UpdateStrategy.Type)
	require.NotNil(t, policy.Spec.UpdateStrategy.Cooldown)
	assert.Equal(t, 30*time.Minute, policy.Spec.UpdateStrategy.Cooldown.Duration)
	require.NotNil(t, policy.Spec.UpdateStrategy.AutoRevert)
	assert.False(t, *policy.Spec.UpdateStrategy.AutoRevert)
	assert.Equal(t, attunev1alpha1.ResizeMethodInPlaceOrRecreate, policy.Spec.UpdateStrategy.ResizeMethod)
	require.NotNil(t, policy.Spec.CPU.MaxChangePercent)
	assert.Equal(t, int32(80), *policy.Spec.CPU.MaxChangePercent)
	require.NotNil(t, policy.Spec.Memory.MaxChangePercent)
	assert.Equal(t, int32(40), *policy.Spec.Memory.MaxChangePercent)
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
	defaults := &attunev1alpha1.AttuneDefaults{
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile:       50,
				Overhead:         "100",
				ControlledValues: &cv,
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                 attunev1alpha1.UpdateTypeAuto,
				MaxConcurrentResizes: 10,
			},
			MetricsSource: &attunev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 20 * time.Minute},
			},
		},
	}

	policyCV := "RequestsOnly"
	policy := &attunev1alpha1.AttunePolicy{
		Spec: attunev1alpha1.AttunePolicySpec{
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &policyCV,
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                 attunev1alpha1.UpdateTypeRecommend,
				MaxConcurrentResizes: 3,
			},
			MetricsSource: attunev1alpha1.MetricsSource{
				RateWindow: &metav1.Duration{Duration: 5 * time.Minute},
			},
		},
	}
	mergeDefaultsIntoPolicy(policy, defaults)

	// Policy fields should be preserved.
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	assert.Equal(t, "20", policy.Spec.CPU.Overhead)
	assert.Equal(t, "RequestsOnly", *policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.UpdateTypeRecommend, policy.Spec.UpdateStrategy.Type)
	assert.Equal(t, int32(3), policy.Spec.UpdateStrategy.MaxConcurrentResizes)
	assert.Equal(t, 5*time.Minute, policy.Spec.MetricsSource.RateWindow.Duration)
}

func TestApplyBuiltInDefaults_SetsControlledValues(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{}
	applyBuiltInDefaults(policy)

	require.NotNil(t, policy.Spec.CPU.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.CPU.ControlledValues)
	require.NotNil(t, policy.Spec.Memory.ControlledValues)
	assert.Equal(t, attunev1alpha1.DefaultControlledValues, *policy.Spec.Memory.ControlledValues)
}

func TestRun_ExportList_BadSubcommand(t *testing.T) {
	code := run([]string{"export", "foo"}, func(string, string) (dynamic.Interface, string, error) {
		return nil, "default", nil
	})
	assert.Equal(t, 1, code)
}

func TestRun_ExportList_HappyPath(t *testing.T) {
	scheme := runtime.NewScheme()

	cm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-team-my-service-recommendations",
				"namespace": "default",
				"labels": map[string]interface{}{
					"attune.io/policy":   "my-team",
					"attune.io/workload": "my-service",
				},
			},
			"data": map[string]interface{}{
				"workload":            "my-service",
				"kind":                "Deployment",
				"main.cpu-request":    "150m",
				"main.memory-request": "256Mi",
				"last-updated":        "2026-05-30T12:00:00Z",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
		cm,
	)

	// Exercise runExportList directly (the main dispatch logic for the export command).
	// This is much closer to the real command path than calling printExportList in isolation.
	// CI trigger commit.
	code := runExportList(context.Background(), dynClient, "default", false, []string{"list"})
	assert.Equal(t, 0, code)
}

func TestPrintExportList(t *testing.T) {
	scheme := runtime.NewScheme()

	cm1 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-app-my-deployment-recommendations",
				"namespace": "production",
				"labels": map[string]interface{}{
					"attune.io/policy":   "my-app",
					"attune.io/workload": "my-deployment",
				},
			},
			"data": map[string]interface{}{
				"workload":               "my-deployment",
				"kind":                   "Deployment",
				"main.cpu-request":       "250m",
				"main.memory-request":    "512Mi",
				"main.confidence":        "0.92",
				"sidecar.cpu-request":    "50m",
				"sidecar.memory-request": "64Mi",
				"last-updated":           "2026-05-30T12:00:00Z",
			},
		},
	}

	cm2 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-app-my-statefulset-recommendations",
				"namespace": "production",
				"labels": map[string]interface{}{
					"attune.io/policy":   "my-app",
					"attune.io/workload": "my-statefulset",
				},
			},
			"data": map[string]interface{}{
				"workload":              "my-statefulset",
				"kind":                  "StatefulSet",
				"worker.cpu-request":    "500m",
				"worker.memory-request": "1Gi",
				"last-updated":          "2026-05-30T12:05:00Z",
			},
		},
	}

	// Third ConfigMap exercising hyphenated policy + workload names (common in real clusters).
	// Uses proper labels (the preferred path after our robustness improvements).
	cm3 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-cool-app-my-fancy-deployment-recommendations",
				"namespace": "production",
				"labels": map[string]interface{}{
					"attune.io/policy":   "my-cool-app",
					"attune.io/workload": "my-fancy-deployment",
				},
			},
			"data": map[string]interface{}{
				"workload":           "my-fancy-deployment",
				"kind":               "Deployment",
				"app.cpu-request":    "100m",
				"app.memory-request": "256Mi",
				"last-updated":       "2026-05-30T12:10:00Z",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
		cm1, cm2, cm3,
	)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	err = printExportList(context.Background(), dynClient, "production", false)
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "my-app")
	assert.Contains(t, output, "my-cool-app")
	// CONTAINERS is a dedicated column. A bare Contains("2") matches the
	// year in last-updated (2026) and would pass even if counts were wrong.
	assert.Regexp(t, `my-deployment\s+Deployment\s+2\s+2026-05-30T12:00:00Z`, output)
	assert.Regexp(t, `my-statefulset\s+StatefulSet\s+1\s+2026-05-30T12:05:00Z`, output)
	assert.Regexp(t, `my-fancy-deployment\s+Deployment\s+1\s+2026-05-30T12:10:00Z`, output)

	// Sorted: my-app/my-deployment, my-app/my-statefulset, then my-cool-app.
	lines := strings.Split(strings.TrimSpace(output), "\n")
	dataLines := lines[1:]
	require.GreaterOrEqual(t, len(dataLines), 3)
	joined := strings.Join(dataLines, "\n")
	depIdx := strings.Index(joined, "my-deployment")
	stsIdx := strings.Index(joined, "my-statefulset")
	fancyIdx := strings.Index(joined, "my-fancy-deployment")
	assert.Greater(t, stsIdx, depIdx, "my-deployment should sort before my-statefulset")
	assert.Greater(t, fancyIdx, stsIdx, "my-app rows should sort before my-cool-app")
}

func TestPrintExportList_AllNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()

	cmNs1 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "team-a-app1-recommendations",
				"namespace": "ns-one",
				"labels": map[string]interface{}{
					"attune.io/policy":   "team-a",
					"attune.io/workload": "app1",
				},
			},
			"data": map[string]interface{}{
				"workload":         "app1",
				"kind":             "Deployment",
				"main.cpu-request": "100m",
			},
		},
	}

	cmNs2 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "team-b-app2-recommendations",
				"namespace": "ns-two",
				"labels": map[string]interface{}{
					"attune.io/policy":   "team-b",
					"attune.io/workload": "app2",
				},
			},
			"data": map[string]interface{}{
				"workload":         "app2",
				"kind":             "Deployment",
				"main.cpu-request": "200m",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
		cmNs1, cmNs2,
	)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	err = printExportList(context.Background(), dynClient, "", true) // all namespaces
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "ns-one")
	assert.Contains(t, output, "ns-two")
	assert.Contains(t, output, "team-a")
	assert.Contains(t, output, "team-b")
}

func TestPrintExportList_MixedGoodAndBad(t *testing.T) {
	scheme := runtime.NewScheme()

	goodCM := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "good-policy-good-workload-recommendations",
				"namespace": "default",
				"labels": map[string]interface{}{
					"attune.io/policy":   "good-policy",
					"attune.io/workload": "good-workload",
				},
			},
			"data": map[string]interface{}{
				"workload":         "good-workload",
				"kind":             "Deployment",
				"main.cpu-request": "50m",
			},
		},
	}

	badCM := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "totally-unrelated-configmap-name",
				"namespace": "default",
				"labels": map[string]interface{}{
					"attune.io/policy": "bad-policy",
				},
			},
			"data": map[string]interface{}{
				"kind":             "Deployment",
				"main.cpu-request": "10m",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
		goodCM, badCM,
	)

	oldOut := os.Stdout
	oldErr := os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	printExportListErr := printExportList(context.Background(), dynClient, "default", false)
	require.NoError(t, printExportListErr)

	wOut.Close()
	wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr

	var bufOut, bufErr bytes.Buffer
	_, err := bufOut.ReadFrom(rOut)
	require.NoError(t, err)
	_, err = bufErr.ReadFrom(rErr)
	require.NoError(t, err)

	output := bufOut.String()
	stderr := bufErr.String()

	// Good data should still appear
	assert.Contains(t, output, "good-policy")
	assert.Contains(t, output, "good-workload")

	// Bad row should show '-' for workload (derivation failed)
	assert.Contains(t, output, "bad-policy")
	assert.Contains(t, output, "-               Deployment") // workload column shows '-' for the bad row (tabwriter spacing)

	// Warning should have fired once
	assert.Contains(t, stderr, "Warning: could not determine workload name")
}

func TestPrintExportList_NoConfigMaps(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
	)

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	err = printExportList(context.Background(), dynClient, "production", false)
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "No exported recommendation ConfigMaps found")
}

func TestPrintExportList_DerivationWarning(t *testing.T) {
	scheme := runtime.NewScheme()

	// ConfigMap that is missing the attune.io/workload label and data["workload"].
	// The name also does not follow the expected convention relative to the policy label,
	// so name derivation will fail and the warning should be emitted.
	badCM := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "random-configmap-name-that-wont-parse",
				"namespace": "production",
				"labels": map[string]interface{}{
					"attune.io/policy": "bad-policy",
				},
			},
			"data": map[string]interface{}{
				"kind":             "Deployment",
				"main.cpu-request": "100m",
				"last-updated":     "2026-05-30T12:00:00Z",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:   "AttunePolicyList",
			cmGVR: "ConfigMapList",
		},
		badCM,
	)

	// Capture both stdout and stderr
	oldOut := os.Stdout
	oldErr := os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	printExportListErr := printExportList(context.Background(), dynClient, "production", false)
	require.NoError(t, printExportListErr)

	wOut.Close()
	wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr

	var bufOut, bufErr bytes.Buffer
	_, err := bufOut.ReadFrom(rOut)
	require.NoError(t, err)
	_, err = bufErr.ReadFrom(rErr)
	require.NoError(t, err)

	output := bufOut.String()
	stderr := bufErr.String()

	assert.Contains(t, output, "bad-policy")
	// Missing workload label renders as a dedicated "-" WORKLOAD column.
	assert.Regexp(t, `bad-policy\s+-\s+Deployment`, output)
	assert.Contains(t, stderr, "Warning: could not determine workload name")
	assert.Contains(t, stderr, "missing attune.io/workload label")
}

func TestWorkloadFromConfigMap(t *testing.T) {
	tests := []struct {
		name     string
		cmName   string
		labels   map[string]string
		data     map[string]string
		expected string
	}{
		{
			name:     "prefers data.workload",
			cmName:   "my-app-my-deployment-recommendations",
			labels:   map[string]string{"attune.io/policy": "my-app"},
			data:     map[string]string{"workload": "my-deployment"},
			expected: "my-deployment",
		},
		{
			name:     "prefers attune.io/workload label over name derivation",
			cmName:   "my-app-my-deployment-recommendations",
			labels:   map[string]string{"attune.io/policy": "my-app", "attune.io/workload": "my-deployment"},
			data:     nil,
			expected: "my-deployment",
		},
		{
			name:     "falls back to name derivation when no labels or data",
			cmName:   "my-app-my-deployment-recommendations",
			labels:   map[string]string{"attune.io/policy": "my-app"},
			data:     nil,
			expected: "my-deployment",
		},
		{
			name:     "handles policy and workload with hyphens",
			cmName:   "my-cool-app-my-fancy-deployment-recommendations",
			labels:   map[string]string{"attune.io/policy": "my-cool-app"},
			data:     nil,
			expected: "my-fancy-deployment",
		},
		{
			name:     "returns empty when nothing available",
			cmName:   "weird-name",
			labels:   nil,
			data:     nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workloadFromConfigMap(tt.cmName, tt.labels, tt.data)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestIsNoResourceMatch(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "server could not find the requested resource",
			err:      fmt.Errorf("the server could not find the requested resource"),
			expected: true,
		},
		{
			name:     "no matches for kind",
			err:      fmt.Errorf("no matches for kind \"AttunePolicy\" in version \"attune.io/v1alpha1\""),
			expected: true,
		},
		{
			name:     "wrapped error with resource not found",
			err:      fmt.Errorf("failed to list: %w", fmt.Errorf("the server could not find the requested resource")),
			expected: true,
		},
		{
			name:     "unrelated error",
			err:      fmt.Errorf("connection refused"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isNoResourceMatch(tt.err))
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }
func ptrBool(v bool) *bool    { return &v }
