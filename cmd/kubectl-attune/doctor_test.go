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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestClassifyKubernetesVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		info    *k8sversion.Info
		wantErr string
	}{
		{name: "nil", wantErr: "empty"},
		{name: "1.31", info: &k8sversion.Info{Major: "1", Minor: "31"}, wantErr: "below Attune's minimum 1.32"},
		{name: "1.32", info: &k8sversion.Info{Major: "1", Minor: "32"}},
		{name: "1.32 plus suffix", info: &k8sversion.Info{Major: "1", Minor: "32+"}},
		{name: "1.32.4 patch in minor", info: &k8sversion.Info{Major: "1", Minor: "32.4"}},
		{name: "1.35", info: &k8sversion.Info{Major: "1", Minor: "35"}},
		{name: "2.0", info: &k8sversion.Info{Major: "2", Minor: "0"}},
		{name: "bad major", info: &k8sversion.Info{Major: "x", Minor: "32"}, wantErr: "parse major"},
		{name: "bad minor", info: &k8sversion.Info{Major: "1", Minor: "x"}, wantErr: "parse minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := classifyKubernetesVersion(tt.info)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestHasPodsResizeSubresource(t *testing.T) {
	t.Parallel()
	assert.False(t, hasPodsResizeSubresource(nil))
	assert.False(t, hasPodsResizeSubresource([]*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods"}}},
	}))
	assert.True(t, hasPodsResizeSubresource([]*metav1.APIResourceList{
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "pods/resize"}}},
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods/resize", Namespaced: true}}},
	}))
	assert.False(t, hasPodsResizeSubresource([]*metav1.APIResourceList{
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "pods/resize"}}},
	}))
}

func TestCollectPrometheusAddresses(t *testing.T) {
	t.Parallel()
	policy := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "http://prom.ns.svc:9090"},
			},
		},
	}}
	defaults := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "http://prom.ns.svc:9090"},
			},
		},
	}}
	other := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "https://thanos.example:9090"},
			},
		},
	}}
	got := collectPrometheusAddresses(policy, defaults, other, unstructured.Unstructured{})
	assert.Equal(t, []string{"http://prom.ns.svc:9090", "https://thanos.example:9090"}, got)
}

func resizeDiscovery(major, minor string, withResize bool) *fakediscovery.FakeDiscovery {
	fd := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	fd.FakedServerVersion = &k8sversion.Info{Major: major, Minor: minor, GitVersion: "v" + major + "." + minor + ".0"}
	core := metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod", Namespaced: true}},
	}
	if withResize {
		core.APIResources = append(core.APIResources, metav1.APIResource{
			Name: podsResizeResourceName, Kind: "Pod", Namespaced: true,
		})
	}
	fd.Resources = []*metav1.APIResourceList{&core}
	return fd
}

func TestRunDoctorChecks_VersionAndDiscovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("pass 1.32 with resize", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, resizeDiscovery("1", "32", true), nil, nil, nil)
		require.Len(t, results, 3)
		assert.True(t, results[0].ok, results[0].detail)
		assert.True(t, results[1].ok, results[1].detail)
		assert.True(t, results[2].ok)
		assert.Contains(t, results[2].detail, "skipped")
		assert.False(t, doctorFailed(results))
	})

	t.Run("fail 1.31", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, resizeDiscovery("1", "31", true), nil, nil, nil)
		require.False(t, results[0].ok)
		assert.Contains(t, results[0].detail, "1.32")
		assert.True(t, doctorFailed(results))
	})

	t.Run("fail missing resize", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, resizeDiscovery("1", "32", false), nil, nil, nil)
		require.False(t, results[1].ok)
		assert.Contains(t, results[1].detail, "InPlacePodVerticalScaling")
		assert.True(t, doctorFailed(results))
	})
}

func TestRunDoctorChecks_PrometheusOptional(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	disc := resizeDiscovery("1", "35", true)
	obj := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "http://prometheus.example:9090"},
			},
		},
	}}

	t.Run("reachable", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return nil
		})
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "http://prometheus.example:9090")
		assert.False(t, doctorFailed(results))
	})

	t.Run("unreachable does not fail required exit", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return fmt.Errorf("connection refused")
		})
		require.False(t, results[2].ok)
		assert.Contains(t, results[2].detail, "connection refused")
		assert.False(t, doctorFailed(results), "Prometheus is optional")
	})

	t.Run("in-cluster address is not pinged from kubectl host", func(t *testing.T) {
		t.Parallel()
		local := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{"address": "http://prom.monitoring.svc:9090"},
				},
			},
		}}
		pinged := false
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{local}, nil, func(context.Context, string) error {
			pinged = true
			return fmt.Errorf("should not ping")
		})
		assert.False(t, pinged)
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "in-cluster")
		assert.False(t, doctorFailed(results))
	})

	t.Run("list error without address is not claimed as no address", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, fmt.Errorf("list AttuneDefaults: forbidden"), nil)
		require.True(t, results[2].ok)
		assert.Contains(t, results[2].detail, "could not list")
		assert.NotContains(t, results[2].detail, "no address")
	})

	t.Run("list error with a collected address still pings", func(t *testing.T) {
		t.Parallel()
		pinged := false
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{obj}, fmt.Errorf("list AttuneDefaults: forbidden"), func(context.Context, string) error {
			pinged = true
			return nil
		})
		assert.True(t, pinged)
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "http://prometheus.example:9090")
	})

	t.Run("ssrf rejected without ping", func(t *testing.T) {
		t.Parallel()
		bad := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{"address": "http://127.0.0.1:1"},
				},
			},
		}}
		pinged := false
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{bad}, nil, func(context.Context, string) error {
			pinged = true
			return nil
		})
		assert.False(t, pinged)
		assert.False(t, results[2].ok)
		assert.Contains(t, results[2].detail, "loopback")
	})

	t.Run("bearer token 401 is skip not warn", func(t *testing.T) {
		t.Parallel()
		authed := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{
						"address":           "http://prometheus.example:9090",
						"bearerTokenSecret": map[string]interface{}{"name": "prom-token", "key": "token"},
					},
				},
			},
		}}
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "401")
		assert.Contains(t, results[2].detail, "bearer")
		assert.False(t, doctorFailed(results))
	})

	t.Run("headers 403 is skip not warn", func(t *testing.T) {
		t.Parallel()
		authed := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{
						"address": "http://mimir.example:8080",
						"headers": map[string]interface{}{"X-Scope-OrgID": "team-a"},
					},
				},
			},
		}}
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 403, url: "http://mimir.example:8080/-/healthy"}
		})
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "403")
		assert.False(t, doctorFailed(results))
	})

	t.Run("bearer token connection error is still warn", func(t *testing.T) {
		t.Parallel()
		authed := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{
						"address":           "http://prometheus.example:9090",
						"bearerTokenSecret": map[string]interface{}{"name": "prom-token", "key": "token"},
					},
				},
			},
		}}
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return fmt.Errorf("connection refused")
		})
		require.False(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "connection refused")
		assert.False(t, doctorFailed(results), "Prometheus is optional")
	})

	t.Run("401 without auth config is still warn", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.False(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "HTTP 401")
	})

	t.Run("same address with and without auth merges auth", func(t *testing.T) {
		t.Parallel()
		plain := obj
		authed := unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{
						"address":           "http://prometheus.example:9090",
						"bearerTokenSecret": map[string]interface{}{"name": "prom-token", "key": "token"},
					},
				},
			},
		}}
		results := runDoctorChecks(ctx, disc, []unstructured.Unstructured{plain, authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.True(t, results[2].ok, results[2].detail)
		assert.Contains(t, results[2].detail, "401")
	})
}

func TestPingAuthFailure(t *testing.T) {
	t.Parallel()
	assert.True(t, pingAuthFailure(&httpStatusError{status: 401, url: "http://x"}))
	assert.True(t, pingAuthFailure(&httpStatusError{status: 403, url: "http://x"}))
	assert.False(t, pingAuthFailure(&httpStatusError{status: 500, url: "http://x"}))
	assert.False(t, pingAuthFailure(fmt.Errorf("HTTP 401")))
	assert.False(t, pingAuthFailure(nil))
}

func TestPrintDoctorResults_OptionalWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printDoctorResults(&buf, []doctorResult{
		{name: "Kubernetes version", required: true, ok: true, detail: "v1.32.0"},
		{name: "Prometheus", required: false, detail: "connection refused"},
	})
	out := buf.String()
	assert.Contains(t, out, "WARN")
	assert.NotContains(t, out, "FAIL")

	buf.Reset()
	printDoctorResults(&buf, []doctorResult{
		{name: "Kubernetes version", required: true, detail: "below 1.32"},
		{name: "Prometheus", required: false, detail: "connection refused"},
	})
	out = buf.String()
	assert.Contains(t, out, "FAIL")
	assert.Contains(t, out, "WARN")
}

func TestClusterLocalPrometheusHost(t *testing.T) {
	t.Parallel()
	assert.True(t, clusterLocalPrometheusHost("http://prom.ns.svc:9090"))
	assert.True(t, clusterLocalPrometheusHost("http://prom.ns.svc.cluster.local:9090"))
	assert.False(t, clusterLocalPrometheusHost("http://prometheus.example:9090"))
	assert.False(t, clusterLocalPrometheusHost("not a url"))
	// Chart and docs use service.namespace without .svc. That is still
	// pinged; do not treat every two-label host as in-cluster.
	assert.False(t, clusterLocalPrometheusHost("http://prometheus-server.monitoring:80"))
}

func TestListDoctorObjects_KeepsPartialOnError(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "AttunePolicyList",
			defaultsGVR:          "AttuneDefaultsList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
		})
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "attune.io/v1alpha1",
		"kind":       "AttunePolicy",
		"metadata":   map[string]interface{}{"name": "p", "namespace": "default"},
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "http://prometheus.example:9090"},
			},
		},
	}}
	_, err := dyn.Resource(gvr).Namespace("default").Create(context.Background(), policy, metav1.CreateOptions{})
	require.NoError(t, err)
	dyn.PrependReactor("list", "attunedefaults", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden")
	})
	got, err := listDoctorObjects(context.Background(), dyn, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AttuneDefaults")
	require.Len(t, got, 1)
	assert.Equal(t, "p", got[0].GetName())
}

func TestRunDoctor_ExitCodes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr:                  "AttunePolicyList",
			defaultsGVR:          "AttuneDefaultsList",
			namespaceDefaultsGVR: "AttuneNamespaceDefaultsList",
		})
	var stdout, stderr bytes.Buffer
	code := runDoctor(context.Background(), &stdout, &stderr, resizeDiscovery("1", "32", true), dyn, "default", nil)
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "pods/resize            ok   [required] discovered")
	assert.Contains(t, stdout.String(), "Kubernetes version     ok   [required]")
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	code = runDoctor(context.Background(), &stdout, &stderr, resizeDiscovery("1", "31", true), dyn, "default", nil)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "one or more checks failed")
	assert.Contains(t, stdout.String(), "FAIL")
}

func TestParseVersionPart(t *testing.T) {
	t.Parallel()
	n, err := parseVersionPart("32+")
	require.NoError(t, err)
	assert.Equal(t, 32, n)
	_, err = parseVersionPart("abc")
	assert.Error(t, err)
}
