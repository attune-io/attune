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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
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
			namespacesGVR:        "NamespaceList",
			deploymentsGVR:       "DeploymentList",
			statefulsetsGVR:      "StatefulSetList",
			daemonsetsGVR:        "DaemonSetList",
			servicesGVR:          "ServiceList",
			gvr:                  "AttunePolicyList",
			defaultsGVR:          "AttuneDefaultsList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
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
	return unstructuredServicePorts(name, namespace, []interface{}{
		map[string]interface{}{"port": port},
	})
}

func unstructuredServicePorts(name, namespace string, ports []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"ports": ports,
			},
		},
	}
}

func unstructuredPolicy(name, namespace, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "attune.io/v1alpha1",
			"kind":       "AttunePolicy",
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
		context.Background(), "api-server-attune", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "api-server-attune", created.GetName())

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
		context.Background(), "web-attune", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "production", created.GetNamespace())
}

func TestWizardPromote(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("api-attune", "default", "Recommend"),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // policy: api-attune
			2, // target mode: Auto
		},
		confirmAnswers: []bool{true},
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	updated, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-attune", metav1.GetOptions{})
	require.NoError(t, err)
	mode := getNestedString(*updated, "spec", "updateStrategy", "type")
	assert.Equal(t, "Auto", mode)
}

func TestWizardPromote_Cancel(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("cache-attune", "default", "Recommend"),
	)

	p := &scriptedPrompter{
		selectAnswers:  []int{0, 2},   // policy, mode
		confirmAnswers: []bool{false}, // cancel
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Mode should be unchanged.
	item, _ := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "cache-attune", metav1.GetOptions{})
	assert.Equal(t, "Recommend", getNestedString(*item, "spec", "updateStrategy", "type"))
}

func TestWizardPromote_NoPolicies(t *testing.T) {
	dynClient := newFakeDynClient()
	p := &scriptedPrompter{}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no AttunePolicies found")
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
	assert.Contains(t, results, "http://prometheus-server.monitoring.svc:9090")
	assert.Contains(t, results, "http://thanos-query.monitoring.svc:10902")
}

func TestDetectPrometheus_PrefersHTTPPort(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredServicePorts("thanos-query", "monitoring", []interface{}{
			map[string]interface{}{"name": "grpc", "port": int64(10901)},
			map[string]interface{}{"name": "http", "port": int64(9090)},
		}),
	)

	results := detectPrometheus(context.Background(), dynClient)
	require.Len(t, results, 1)
	assert.Equal(t, "http://thanos-query.monitoring.svc:9090", results[0])
}

func TestDetectPrometheus_NoMatches(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredService("redis", "default", int64(6379)),
	)
	results := detectPrometheus(context.Background(), dynClient)
	assert.Empty(t, results)
}

func unstructuredClusterDefaultsDatadog(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&attunev1alpha1.AttuneDefaults{
		TypeMeta:   metav1.TypeMeta{APIVersion: "attune.io/v1alpha1", Kind: "AttuneDefaults"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site: "datadoghq.com",
					APIKeySecretRef: attunev1alpha1.SecretKeyRef{
						Name: "datadog",
						Key:  "api-key",
					},
				},
			},
		},
	})
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: obj}
}

func TestWizardCreate_InheritsDefaultsDatadog(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredDeployment("api-server", "default", 3),
		unstructuredService("prometheus-server", "monitoring", int64(9090)),
	)
	_, err := dynClient.Resource(defaultsGVR).Create(context.Background(),
		unstructuredClusterDefaultsDatadog(t, "cluster"), metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Datadog", inheritedMetricsProviderLabel(context.Background(), dynClient, "default"))

	p := &scriptedPrompter{
		selectAnswers: []int{
			0, // kind: Deployment
			0, // workload: api-server
			0, // inherit Datadog
			0, // CPU: P95
			0, // Memory: P99
			0, // mode: Recommend
			0, // action: Apply
		},
	}

	err = wizardCreate(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	created, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-server-attune", metav1.GetOptions{})
	require.NoError(t, err)

	_, found, err := unstructured.NestedMap(created.Object, "spec", "metricsSource")
	require.NoError(t, err)
	assert.False(t, found, "wizard must omit metricsSource so MergeDefaults keeps AttuneDefaults Datadog")
}

func TestBuildPolicyObject(t *testing.T) {
	obj := buildPolicyObject("prod", "api-attune", "Deployment", "api-server",
		"http://prom:9090", 95, 99, "Recommend", false)

	assert.Equal(t, "attune.io/v1alpha1", obj.GetAPIVersion())
	assert.Equal(t, "AttunePolicy", obj.GetKind())
	assert.Equal(t, "api-attune", obj.GetName())
	assert.Equal(t, "prod", obj.GetNamespace())

	mode := getNestedString(*obj, "spec", "updateStrategy", "type")
	assert.Equal(t, "Recommend", mode)
	addr := getNestedString(*obj, "spec", "metricsSource", "prometheus", "address")
	assert.Equal(t, "http://prom:9090", addr)
}

func TestBuildPolicyObject_OmitsMetricsWhenInherited(t *testing.T) {
	obj := buildPolicyObject("prod", "api-attune", "Deployment", "api-server",
		"", 95, 99, "Recommend", false)
	_, found, err := unstructured.NestedMap(obj.Object, "spec", "metricsSource")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCRD_MetricsSourceNotRequired(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "attune.io_attunepolicies.yaml"))
	require.NoError(t, err)
	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	versions, found, err := unstructured.NestedSlice(doc, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, versions)
	ver, ok := versions[0].(map[string]interface{})
	require.True(t, ok)
	required, found, err := unstructured.NestedStringSlice(ver,
		"schema", "openAPIV3Schema", "properties", "spec", "required")
	require.NoError(t, err)
	require.True(t, found)
	assert.NotContains(t, required, "metricsSource",
		"omit metricsSource must be schema-valid so wizard inherit is accepted")
	assert.Contains(t, required, "targetRef")
	assert.Contains(t, required, "cpu")
	assert.Contains(t, required, "memory")
}

func TestKindToGVR(t *testing.T) {
	assert.Equal(t, deploymentsGVR, kindToGVR("Deployment"))
	assert.Equal(t, statefulsetsGVR, kindToGVR("StatefulSet"))
	assert.Equal(t, daemonsetsGVR, kindToGVR("DaemonSet"))
}

func TestWizardPromote_SameMode(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("api-attune", "default", "Auto"),
	)

	p := &scriptedPrompter{
		selectAnswers: []int{0, 2}, // policy: api-attune, mode: Auto (same)
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	// Mode should still be Auto (no update attempted).
	item, _ := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-attune", metav1.GetOptions{})
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
		context.Background(), "worker-attune", metav1.GetOptions{})
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

func TestInitialSizingNextSteps(t *testing.T) {
	got := initialSizingNextSteps("prod")
	assert.Contains(t, got, "initialSizing.enabled")
	assert.Contains(t, got, "attune.io/initial-sizing=enabled")
	assert.Contains(t, got, "prod")
}

func TestBuildPolicyObject_WithInitialSizing(t *testing.T) {
	obj := buildPolicyObject("prod", "api-attune", "Deployment", "api-server",
		"http://prom:9090", 95, 99, "Auto", true)

	mode := getNestedString(*obj, "spec", "updateStrategy", "type")
	assert.Equal(t, "Auto", mode)

	is, found, _ := unstructured.NestedBool(obj.Object, "spec", "updateStrategy", "initialSizing")
	assert.True(t, found, "initialSizing should be present")
	assert.True(t, is, "initialSizing should be true")
}

func TestBuildPolicyObject_WithoutInitialSizing(t *testing.T) {
	obj := buildPolicyObject("prod", "api-attune", "Deployment", "api-server",
		"http://prom:9090", 95, 99, "Auto", false)

	// initialSizing should not be present when false.
	_, found, _ := unstructured.NestedBool(obj.Object, "spec", "updateStrategy", "initialSizing")
	assert.False(t, found, "initialSizing should not be set when false")
}

func TestWizardCreate_AutoModeWithInitialSizing(t *testing.T) {
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
			1, // mode: Auto
			0, // action: Apply
		},
		confirmAnswers: []bool{true}, // initial sizing: yes
	}

	err := wizardCreate(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	created, err := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-server-attune", metav1.GetOptions{})
	require.NoError(t, err)

	mode := getNestedString(*created, "spec", "updateStrategy", "type")
	assert.Equal(t, "Auto", mode)

	is, found, _ := unstructured.NestedBool(created.Object, "spec", "updateStrategy", "initialSizing")
	assert.True(t, found, "initialSizing should be present")
	assert.True(t, is, "initialSizing should be true")
}

func TestWizardPromote_EnablesInitialSizing(t *testing.T) {
	dynClient := newFakeDynClient(
		unstructuredPolicy("api-attune", "default", "Recommend"),
	)

	p := &scriptedPrompter{
		selectAnswers:  []int{0, 2},        // policy: api-attune, mode: Auto
		confirmAnswers: []bool{true, true}, // initial sizing: yes, confirm promote: yes
	}

	err := wizardPromote(context.Background(), dynClient, "default", p)
	require.NoError(t, err)

	item, _ := dynClient.Resource(gvr).Namespace("default").Get(
		context.Background(), "api-attune", metav1.GetOptions{})
	assert.Equal(t, "Auto", getNestedString(*item, "spec", "updateStrategy", "type"))

	is, found, _ := unstructured.NestedBool(item.Object, "spec", "updateStrategy", "initialSizing")
	assert.True(t, found, "initialSizing should be set")
	assert.True(t, is, "initialSizing should be true")
}

func TestMarshalPolicyYAML(t *testing.T) {
	obj := buildPolicyObject("default", "test", "Deployment", "app",
		"http://prom:9090", 95, 99, "Recommend", false)
	data, err := marshalPolicyYAML(obj)
	require.NoError(t, err)
	assert.Contains(t, string(data), "apiVersion: attune.io/v1alpha1")
	assert.Contains(t, string(data), "kind: AttunePolicy")
}
