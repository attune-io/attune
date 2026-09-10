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

package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type goldenResources struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

type replaceGolden struct {
	Current goldenResources `json:"current"`
	Want    goldenResources `json:"want"`
	Got     goldenResources `json:"got"`
}

func parseGoldenResources(g goldenResources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	if len(g.Requests) > 0 {
		out.Requests = corev1.ResourceList{}
		for k, v := range g.Requests {
			out.Requests[corev1.ResourceName(k)] = resource.MustParse(v)
		}
	}
	if len(g.Limits) > 0 {
		out.Limits = corev1.ResourceList{}
		for k, v := range g.Limits {
			out.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
		}
	}
	return out
}

func TestReplaceCPUMemoryResources_Golden(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "goldens", "replace_extended.json"))
	require.NoError(t, err)
	var g replaceGolden
	require.NoError(t, json.Unmarshal(raw, &g))

	got := replaceCPUMemoryResources(parseGoldenResources(g.Current), parseGoldenResources(g.Want))
	if diff := cmp.Diff(g.Got.Requests, resourceListMap(got.Requests)); diff != "" {
		t.Fatalf("requests mismatch (-golden +got):\n%s", diff)
	}
	if diff := cmp.Diff(g.Got.Limits, resourceListMap(got.Limits)); diff != "" {
		t.Fatalf("limits mismatch (-golden +got):\n%s", diff)
	}
}
