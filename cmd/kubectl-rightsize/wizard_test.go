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
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// scriptedPrompter returns pre-programmed answers for testing.
type scriptedPrompter struct {
	selectAnswers  []int
	inputAnswers   []string
	confirmAnswers []bool
	selectIdx      int
	inputIdx       int
	confirmIdx     int
}

func (s *scriptedPrompter) Select(_ string, options []string) (int, error) {
	if s.selectIdx >= len(s.selectAnswers) {
		return 0, fmt.Errorf("no more select answers (asked %d times, have %d)", s.selectIdx+1, len(s.selectAnswers))
	}
	idx := s.selectAnswers[s.selectIdx]
	s.selectIdx++
	if idx < 0 || idx >= len(options) {
		return 0, fmt.Errorf("scripted index %d out of range [0, %d)", idx, len(options))
	}
	return idx, nil
}

func (s *scriptedPrompter) Input(_ string, defaultVal string) (string, error) {
	if s.inputIdx >= len(s.inputAnswers) {
		return defaultVal, nil
	}
	val := s.inputAnswers[s.inputIdx]
	s.inputIdx++
	if val == "" {
		return defaultVal, nil
	}
	return val, nil
}

func (s *scriptedPrompter) Confirm(_ string, defaultVal bool) (bool, error) {
	if s.confirmIdx >= len(s.confirmAnswers) {
		return defaultVal, nil
	}
	val := s.confirmAnswers[s.confirmIdx]
	s.confirmIdx++
	return val, nil
}

func newFakeDynClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			namespacesGVR:   "NamespaceList",
			deploymentsGVR:  "DeploymentList",
			statefulsetsGVR: "StatefulSetList",
			daemonsetsGVR:   "DaemonSetList",
			servicesGVR:     "ServiceList",
			gvr:             "RightSizePolicyList",
		},
		objects...,
	)
}

func unstructuredNamespace(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": name},
		},
	}
}

func unstructuredDeployment(name, namespace string, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"replicas": replicas,
			},
		},
	}
}

func unstructuredService(name, namespace string, port int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"ports": []interface{}{
					map[string]interface{}{"port": port},
				},
			},
		},
	}
}

func unstructuredPolicy(name, namespace, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"updateStrategy": map[string]interface{}{
					"type": mode,
				},
			},
		},
	}
}

func TestWizardCreate_ApplyToCluster(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredDeployment("api-server", "default", 3),
		unstructuredService("prometheus-server", "monitoring", int64(9090)),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // kind: Deployment
			0, // workload: api-server
			0, // prometheus: auto-detected
			0, // CPU: P95
			0, // Memory: P99
			0, // mode: Recommend
			0, // action: Apply
		},
	}

	err := wizardCreate(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Verify the policy was created.
	created, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-server-rightsize", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "api-server-rightsize", created.GetName())

	mode := getNestedString(*created, "spec", "updateStrategy", "type")
	assert.Equal(t, "Recommend", mode)

	kind := getNestedString(*created, "spec", "targetRef", "kind")
	assert.Equal(t, "Deployment", kind)
}

func TestWizardCreate_Cancel(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredDeployment("worker", "default", 2),
		unstructuredService("prometheus-kube-stack", "monitoring", int64(9090)),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // kind: Deployment
			0, // workload: worker
			0, // prometheus
			0, // CPU
			0, // Memory
			0, // mode
			2, // action: Cancel
		},
	}

	err := wizardCreate(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Verify no policy was created.
	list, err := dynClient.Resource(gvr).Namespace("default").List(
		context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

func TestWizardCreate_NamespaceSelection(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredNamespace("default"),
		unstructuredNamespace("production"),
		unstructuredDeployment("web", "production", 5),
		unstructuredService("prometheus-server", "monitoring", int64(9090)),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			1, // namespace: production (default is idx 0)
			0, // kind: Deployment
			0, // workload: web
			0, // prometheus
			0, // CPU
			0, // Memory
			0, // mode
			0, // action: Apply
		},
	}

	err := wizardCreate(context.Background(), dynClient, "", p)
	require.NoError(t, err)

	created, err := dynClient.Resource(gvr).Namespace("production").Get(
		context.Background(), "web-rightsize", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "production", created.GetNamespace())
}

func TestWizardPromote(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("api-rightsize", "default", "Recommend"),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // policy: api-rightsize
			2, // target mode: Auto
		},
		confirmAnswers: []bool{true},
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	updated, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-rightsize", metav1.GetOptions{})
	require.NoError(t, err)
	mode := getNestedString(*updated, "spec", "updateStrategy", "type")
	assert.Equal(t, "Auto", mode)
}

func TestWizardPromote_Cancel(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("cache-rightsize", "default", "Recommend"),
	)

	p := &scriptedPrompter{
		selectAnswers:  []int{0, 2},   // policy, mode
		confirmAnswers: []bool{false}, // cancel
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Mode should be unchanged.
	item, _ := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "cache-rightsize", metav1.GetOptions{})
	assert.Equal(t, "Recommend", getNestedString(*item, "spec", "updateStrategy", "type"))
}

func TestWizardPromote_NoPolicies(t *testing.T) {
	dynClient := newFakeDynClient()
	p := &scriptedPrompter{}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no RightSizePolicies found")
}

func TestRunWizard_UnknownSubcommand(t *testing.T) {
	dynClient := newFakeDynClient()
	p := &scriptedPrompter{}
	code := runWizard(context.Background(), dynClient, "default", []string{"unknown"}, p)
	assert.Equal(t, 1, code)
}

func TestDetectPrometheus(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredService("prometheus-server", "monitoring", int64(9090)),
		unstructuredService("thanos-query", "monitoring", int64(10902)),
		unstructuredService("redis", "default", int64(6379)),
	)

	results := detectPrometheus(context.Background(), dynClient)
	assert.Len(t, results, 2)
	assert.Contains(t, results, "http://prometheus-server.monitoring:9090")
	assert.Contains(t, results, "http://thanos-query.monitoring:10902")
}

func TestDetectPrometheus_NoMatches(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredService("redis", "default", int64(6379)),
	)
	results := detectPrometheus(context.Background(), dynClient)
	assert.Empty(t, results)
}

func TestBuildPolicyObject(t *testing.T) {
	obj := buildPolicyObject("prod", "api-rightsize", "Deployment", "api-server",
		"http://prom:9090", 95, 99, "Recommend")

	assert.Equal(t, "rightsize.io/v1alpha1", obj.GetAPIVersion())
	assert.Equal(t, "RightSizePolicy", obj.GetKind())
	assert.Equal(t, "api-rightsize", obj.GetName())
	assert.Equal(t, "prod", obj.GetNamespace())

	mode := getNestedString(*obj, "spec", "updateStrategy", "type")
	assert.Equal(t, "Recommend", mode)
}

func TestKindToGVR(t *testing.T) {
	assert.Equal(t, deploymentsGVR, kindToGVR("Deployment"))
	assert.Equal(t, statefulsetsGVR, kindToGVR("StatefulSet"))
	assert.Equal(t, daemonsetsGVR, kindToGVR("DaemonSet"))
}

func TestWizardPromote_SameMode(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("api-rightsize", "default", "Auto"),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{0, 2}, // policy: api-rightsize, mode: Auto (same)
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Mode should still be Auto (no update attempted).
	item, _ := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-rightsize", metav1.GetOptions{})
	assert.Equal(t, "Auto", getNestedString(*item, "spec", "updateStrategy", "type"))
}

func TestWizardCreate_ManualPrometheus(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredDeployment("worker", "default", 3),
		// No prometheus service; forces manual input.
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // kind: Deployment
			0, // workload: worker
			0, // CPU: P95
			0, // Memory: P99
			0, // mode: Recommend
			0, // action: Apply
		},
		inputAnswers: []string{"http://custom-prom:9090"},
	}

	err := wizardCreate(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	created, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "worker-rightsize", metav1.GetOptions{})
	require.NoError(t, err)

	addr := getNestedString(*created, "spec", "metricsSource", "prometheus", "address")
	assert.Equal(t, "http://custom-prom:9090", addr)
}

func TestWizardCreate_NoWorkloads(t *testing.T) {
	dynClient := newFakeDynClient()
	p := &scriptedPrompter{
		selectAnswers: []int{0}, // kind: Deployment
	}
	err := wizardCreate(context.Background(), dynClient, "default", p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no Deployments found")
}

func TestMarshalPolicyYAML(t *testing.T) {
	obj := buildPolicyObject("default", "test", "Deployment", "app",
		"http://prom:9090", 95, 99, "Recommend")
	data, err := marshalPolicyYAML(obj)
	require.NoError(t, err)
	assert.Contains(t, string(data), "apiVersion: rightsize.io/v1alpha1")
	assert.Contains(t, string(data), "kind: RightSizePolicy")
}
