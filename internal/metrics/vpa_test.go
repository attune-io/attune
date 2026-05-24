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

package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newVPAObject(name, namespace string, containerRecs []map[string]interface{}) *unstructured.Unstructured {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "autoscaling.k8s.io",
		Version: "v1",
		Kind:    "VerticalPodAutoscaler",
	})
	vpa.SetName(name)
	vpa.SetNamespace(namespace)

	if containerRecs != nil {
		recsSlice := make([]interface{}, len(containerRecs))
		for i, r := range containerRecs {
			recsSlice[i] = r
		}
		_ = unstructured.SetNestedSlice(vpa.Object, recsSlice, "status", "recommendation", "containerRecommendations")
	}
	return vpa
}

func TestReadVPARecommendations_Success(t *testing.T) {
	vpa := newVPAObject("my-vpa", "default", []map[string]interface{}{
		{
			"containerName": "app",
			"target": map[string]interface{}{
				"cpu":    "250m",
				"memory": "512Mi",
			},
		},
		{
			"containerName": "sidecar",
			"target": map[string]interface{}{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
	})

	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "my-vpa", "default")
	require.NoError(t, err)
	require.Len(t, recs, 2)

	assert.Equal(t, "app", recs[0].ContainerName)
	assert.Equal(t, resource.MustParse("250m"), recs[0].CPUTarget)
	assert.Equal(t, resource.MustParse("512Mi"), recs[0].MemoryTarget)

	assert.Equal(t, "sidecar", recs[1].ContainerName)
	assert.Equal(t, resource.MustParse("100m"), recs[1].CPUTarget)
	assert.Equal(t, resource.MustParse("128Mi"), recs[1].MemoryTarget)
}

func TestReadVPARecommendations_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "missing", "default")
	require.Error(t, err)
	assert.Nil(t, recs)
	assert.Contains(t, err.Error(), "fetching VPA")
}

func TestReadVPARecommendations_NoRecommendations(t *testing.T) {
	vpa := newVPAObject("empty-vpa", "default", nil)
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "empty-vpa", "default")
	require.NoError(t, err)
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_EmptyRecommendations(t *testing.T) {
	vpa := newVPAObject("empty-recs", "default", []map[string]interface{}{})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "empty-recs", "default")
	require.NoError(t, err)
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_MissingContainerName(t *testing.T) {
	vpa := newVPAObject("bad-vpa", "default", []map[string]interface{}{
		{
			"target": map[string]interface{}{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
	})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "bad-vpa", "default")
	require.NoError(t, err)
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_MissingTarget(t *testing.T) {
	vpa := newVPAObject("no-target", "default", []map[string]interface{}{
		{
			"containerName": "app",
		},
	})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "no-target", "default")
	require.NoError(t, err)
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_InvalidCPU(t *testing.T) {
	vpa := newVPAObject("bad-cpu", "default", []map[string]interface{}{
		{
			"containerName": "app",
			"target": map[string]interface{}{
				"cpu":    "invalid",
				"memory": "128Mi",
			},
		},
	})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "bad-cpu", "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing VPA CPU target")
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_InvalidMemory(t *testing.T) {
	vpa := newVPAObject("bad-mem", "default", []map[string]interface{}{
		{
			"containerName": "app",
			"target": map[string]interface{}{
				"cpu":    "100m",
				"memory": "not-a-quantity",
			},
		},
	})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "bad-mem", "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing VPA memory target")
	assert.Nil(t, recs)
}

func TestReadVPARecommendations_MultipleContainers(t *testing.T) {
	vpa := newVPAObject("multi-vpa", "prod", []map[string]interface{}{
		{
			"containerName": "web",
			"target": map[string]interface{}{
				"cpu":    "500m",
				"memory": "1Gi",
			},
		},
		{
			"containerName": "worker",
			"target": map[string]interface{}{
				"cpu":    "1",
				"memory": "2Gi",
			},
		},
		{
			"containerName": "proxy",
			"target": map[string]interface{}{
				"cpu":    "50m",
				"memory": "64Mi",
			},
		},
	})
	c := fake.NewClientBuilder().WithObjects(vpa).Build()

	recs, err := ReadVPARecommendations(context.Background(), c, "multi-vpa", "prod")
	require.NoError(t, err)
	require.Len(t, recs, 3)

	assert.Equal(t, "web", recs[0].ContainerName)
	assert.Equal(t, resource.MustParse("500m"), recs[0].CPUTarget)
	assert.Equal(t, resource.MustParse("1Gi"), recs[0].MemoryTarget)

	assert.Equal(t, "worker", recs[1].ContainerName)
	assert.Equal(t, resource.MustParse("1"), recs[1].CPUTarget)
	assert.Equal(t, resource.MustParse("2Gi"), recs[1].MemoryTarget)

	assert.Equal(t, "proxy", recs[2].ContainerName)
	assert.Equal(t, resource.MustParse("50m"), recs[2].CPUTarget)
	assert.Equal(t, resource.MustParse("64Mi"), recs[2].MemoryTarget)
}
