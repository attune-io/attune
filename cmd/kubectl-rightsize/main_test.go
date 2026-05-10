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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func TestGetReadyStatus(t *testing.T) {
	tests := []struct {
		name string
		obj  unstructured.Unstructured
		want string
	}{
		{
			name: "ready true",
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
			want: "True",
		},
		{
			name: "ready false",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
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
			},
			want: "False",
		},
		{
			name: "no conditions",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{},
				},
			},
			want: "Unknown",
		},
		{
			name: "no status",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{},
			},
			want: "Unknown",
		},
		{
			name: "multiple conditions, ready in second",
			obj: unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Progressing",
								"status": "True",
							},
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
							},
						},
					},
				},
			},
			want: "True",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getReadyStatus(tt.obj)
			assert.Equal(t, tt.want, got)
		})
	}
}
