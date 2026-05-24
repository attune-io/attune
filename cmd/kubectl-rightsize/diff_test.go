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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestPrintDiff_UnifiedOutput(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "api-server-rightsize",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api-server",
						"kind":     "Deployment",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "512Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "280m", "memoryRequest": "384Mi"},
								"confidence":  0.92,
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

	printDiff(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "--- a/default/Deployment/api-server")
	assert.Contains(t, output, "+++ b/default/Deployment/api-server (recommended by api-server-rightsize)")
	assert.Contains(t, output, "@@ container: app @@")
	assert.Contains(t, output, "   resources:")
	assert.Contains(t, output, "     requests:")
	assert.Contains(t, output, "-      cpu: \"500m\"")
	assert.Contains(t, output, "+      cpu: \"280m\"")
	assert.Contains(t, output, "-      memory: \"512Mi\"")
	assert.Contains(t, output, "+      memory: \"384Mi\"")
}

func TestPrintDiff_NoChanges(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "stable-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "stable-deploy",
						"kind":     "Deployment",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "256Mi"},
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

	printDiff(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	// Header is still printed for the workload, but no container diff lines.
	assert.Contains(t, output, "--- a/default/Deployment/stable-deploy")
	assert.NotContains(t, output, "@@ container:")
}

func TestPrintDiff_MultipleContainers(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "multi-container-policy",
				"namespace": "production",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "web-app",
						"kind":     "Deployment",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "frontend",
								"current":     map[string]interface{}{"cpuRequest": "1000m", "memoryRequest": "1Gi"},
								"recommended": map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "768Mi"},
								"confidence":  0.88,
							},
							map[string]interface{}{
								"name":        "sidecar",
								"current":     map[string]interface{}{"cpuRequest": "100m", "memoryRequest": "64Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "50m", "memoryRequest": "64Mi"},
								"confidence":  0.75,
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

	printDiff(context.Background(), dynClient, "production", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "@@ container: frontend @@")
	assert.Contains(t, output, "-      cpu: \"1000m\"")
	assert.Contains(t, output, "+      cpu: \"500m\"")
	assert.Contains(t, output, "@@ container: sidecar @@")
	assert.Contains(t, output, "-      cpu: \"100m\"")
	assert.Contains(t, output, "+      cpu: \"50m\"")
	// sidecar memory unchanged, should show as context line.
	assert.Contains(t, output, "       memory: \"64Mi\"")
}

func TestPrintDiff_WithLimits(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "limits-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "guaranteed-deploy",
						"kind":     "Deployment",
						"containers": []interface{}{
							map[string]interface{}{
								"name": "app",
								"current": map[string]interface{}{
									"cpuRequest":    "500m",
									"memoryRequest": "512Mi",
									"cpuLimit":      "1000m",
									"memoryLimit":   "1Gi",
								},
								"recommended": map[string]interface{}{
									"cpuRequest":    "280m",
									"memoryRequest": "384Mi",
									"cpuLimit":      "560m",
									"memoryLimit":   "768Mi",
								},
								"confidence": 0.9,
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

	printDiff(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "     requests:")
	assert.Contains(t, output, "-      cpu: \"500m\"")
	assert.Contains(t, output, "+      cpu: \"280m\"")
	assert.Contains(t, output, "     limits:")
	assert.Contains(t, output, "-      cpu: \"1000m\"")
	assert.Contains(t, output, "+      cpu: \"560m\"")
	assert.Contains(t, output, "-      memory: \"1Gi\"")
	assert.Contains(t, output, "+      memory: \"768Mi\"")
}

func TestPrintDiff_YAMLOutput(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "api-policy",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "api-server",
						"kind":     "Deployment",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "app",
								"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "512Mi"},
								"recommended": map[string]interface{}{"cpuRequest": "280m", "memoryRequest": "384Mi"},
								"confidence":  0.92,
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

	printDiff(context.Background(), dynClient, "default", "yaml")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "---")
	assert.Contains(t, output, "apiVersion: apps/v1")
	assert.Contains(t, output, "kind: Deployment")
	assert.Contains(t, output, "name: api-server")
	assert.Contains(t, output, "namespace: default")
	assert.Contains(t, output, "name: app")
	assert.Contains(t, output, "cpu: 280m")
	assert.Contains(t, output, "memory: 384Mi")
	// Should not contain diff markers.
	assert.NotContains(t, output, "---  a/")
	assert.NotContains(t, output, "+++")
}

func TestPrintDiff_YAMLOutput_CronJob(t *testing.T) {
	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      "cronjob-policy",
				"namespace": "batch",
			},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "daily-report",
						"kind":     "CronJob",
						"containers": []interface{}{
							map[string]interface{}{
								"name":        "reporter",
								"current":     map[string]interface{}{"cpuRequest": "1000m", "memoryRequest": "2Gi"},
								"recommended": map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "1Gi"},
								"confidence":  0.85,
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

	printDiff(context.Background(), dynClient, "batch", "yaml")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "apiVersion: batch/v1")
	assert.Contains(t, output, "kind: CronJob")
	assert.Contains(t, output, "jobTemplate:")
}

func TestPrintDiff_NoPolicies(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "RightSizePolicyList"})

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printDiff(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "No RightSizePolicies found.")
}

func TestPrintDiff_NoRecommendations(t *testing.T) {
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
						"type":   "Ready",
						"status": "False",
						"reason": "InsufficientData",
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

	printDiff(context.Background(), dynClient, "default", "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "No recommendations available")
}

func TestPrintDiff_DefaultsKindToDeployment(t *testing.T) {
	items := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "my-policy", "namespace": "default"},
			"status": map[string]interface{}{
				"recommendations": []interface{}{
					map[string]interface{}{
						"workload": "web-deploy",
						// kind field missing.
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
		}},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	printDiffItems(items, "")

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "--- a/default/Deployment/web-deploy")
}

func TestApiVersionForKind(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{kind: "Deployment", want: "apps/v1"},
		{kind: "StatefulSet", want: "apps/v1"},
		{kind: "DaemonSet", want: "apps/v1"},
		{kind: "ReplicaSet", want: "apps/v1"},
		{kind: "CronJob", want: "batch/v1"},
		{kind: "Job", want: "batch/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			assert.Equal(t, tt.want, apiVersionForKind(tt.kind))
		})
	}
}

func TestRun_DiffWiring(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rightsize.io/v1alpha1",
		"kind":       "RightSizePolicy",
		"metadata": map[string]interface{}{
			"name":      "api-svc",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"recommendations": []interface{}{
				map[string]interface{}{
					"workload": "api-deploy",
					"kind":     "Deployment",
					"containers": []interface{}{
						map[string]interface{}{
							"name":        "app",
							"current":     map[string]interface{}{"cpuRequest": "500m", "memoryRequest": "512Mi"},
							"recommended": map[string]interface{}{"cpuRequest": "250m", "memoryRequest": "384Mi"},
							"confidence":  0.85,
						},
					},
				},
			},
		},
	}}

	tests := []struct {
		name         string
		args         []string
		wantExitCode int
		wantStdout   string
		wantStderr   string
	}{
		{
			name:         "diff shows unified output",
			args:         []string{"diff"},
			wantExitCode: 0,
			wantStdout:   "--- a/default/Deployment/api-deploy",
		},
		{
			name:         "diff with yaml output",
			args:         []string{"diff", "-o", "yaml"},
			wantExitCode: 0,
			wantStdout:   "apiVersion: apps/v1",
		},
		{
			name:         "diff rejects json output",
			args:         []string{"diff", "-o", "json"},
			wantExitCode: 1,
			wantStderr:   "-o json is not supported with diff",
		},
		{
			name:         "diff rejects positional args",
			args:         []string{"diff", "extra"},
			wantExitCode: 1,
			wantStderr:   "diff accepts no positional arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitCode, stdout, stderr := captureRun(t, tt.args, fakeDynamicClientFactory(t, policy))
			assert.Equal(t, tt.wantExitCode, exitCode)
			if tt.wantStdout != "" {
				assert.Contains(t, stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" {
				assert.Contains(t, stderr, tt.wantStderr)
			}
		})
	}
}

func TestStructuredOutputCommandError_Diff(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr string
	}{
		{
			name:   "diff allows yaml",
			output: "yaml",
		},
		{
			name:    "diff rejects json",
			output:  "json",
			wantErr: "-o json is not supported with diff",
		},
		{
			name:    "diff rejects table",
			output:  "table",
			wantErr: "-o table is not supported with diff",
		},
		{
			name:   "diff allows empty output",
			output: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := structuredOutputCommandError("diff", tt.output)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}
