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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/attune-io/attune/internal/cluster"
)

// pingTestURL rewrites httptest's 127.0.0.1 listener to localhost so
// validation.PrometheusAddress allows the request. Literal loopback
// IPs are rejected as SSRF.
func pingTestURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Host = "localhost:" + u.Port()
	return u.String()
}

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

func doctorNamed(results []doctorResult, name string) doctorResult {
	for _, r := range results {
		if r.name == name {
			return r
		}
	}
	return doctorResult{name: name, detail: "missing row"}
}

func nfdCgroupNodeLister(cgroupV2 bool) cluster.NodeLister {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "n1",
			Labels: map[string]string{},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	if cgroupV2 {
		node.Labels[nfdCgroupV2Label] = "true"
	}
	return kubefake.NewSimpleClientset(node).CoreV1().Nodes()
}

func TestCollectPrometheusTargets(t *testing.T) {
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
	got := collectPrometheusTargets(policy, defaults, other, unstructured.Unstructured{})
	require.Len(t, got, 2)
	assert.Equal(t, "http://prom.ns.svc:9090", got[0].address)
	assert.False(t, got[0].hasAuth)
	assert.Equal(t, "https://thanos.example:9090", got[1].address)
	assert.False(t, got[1].hasAuth)
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
			Name: "pods/resize", Kind: "Pod", Namespaced: true,
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
		results := runDoctorChecks(ctx, resizeDiscovery("1", "32", true), nil, nil, nil, nil)
		require.Len(t, results, 5)
		assert.True(t, doctorNamed(results, "Kubernetes version").ok, doctorNamed(results, "Kubernetes version").detail)
		assert.True(t, doctorNamed(results, "pods/resize").ok, doctorNamed(results, "pods/resize").detail)
		prom := doctorNamed(results, "Prometheus")
		assert.False(t, prom.ok, "no Prometheus ping is not ok")
		assert.Contains(t, prom.detail, "skipped (no address on policies or defaults)")
		policies := doctorNamed(results, "AttunePolicies")
		assert.False(t, policies.ok)
		assert.Contains(t, policies.detail, "no AttunePolicies in scope")
		assert.False(t, doctorFailed(results))
	})

	t.Run("fail 1.31", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, resizeDiscovery("1", "31", true), nil, nil, nil, nil)
		ver := doctorNamed(results, "Kubernetes version")
		require.False(t, ver.ok)
		assert.Contains(t, ver.detail, "1.32")
		assert.True(t, doctorFailed(results))
	})

	t.Run("fail missing resize", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, resizeDiscovery("1", "32", false), nil, nil, nil, nil)
		resize := doctorNamed(results, "pods/resize")
		require.False(t, resize.ok)
		assert.Contains(t, resize.detail, "InPlacePodVerticalScaling")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return nil
		})
		prom := doctorNamed(results, "Prometheus")
		require.True(t, prom.ok, prom.detail)
		assert.Contains(t, prom.detail, "http://prometheus.example:9090")
		assert.False(t, doctorFailed(results))
	})

	t.Run("unreachable does not fail required exit", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return fmt.Errorf("connection refused")
		})
		prom := doctorNamed(results, "Prometheus")
		require.False(t, prom.ok)
		assert.Contains(t, prom.detail, "connection refused")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{local}, nil, func(context.Context, string) error {
			pinged = true
			return fmt.Errorf("should not ping")
		})
		assert.False(t, pinged)
		require.False(t, doctorNamed(results, "Prometheus").ok, "skip-only in-cluster is WARN, not ok: %s", doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "in-cluster")
		assert.False(t, doctorFailed(results))
	})

	t.Run("list error without address is not claimed as no address", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, nil, fmt.Errorf("list AttuneDefaults: forbidden"), nil)
		require.False(t, doctorNamed(results, "Prometheus").ok, "no ping is not ok")
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "could not list")
		assert.NotContains(t, doctorNamed(results, "Prometheus").detail, "no address")
	})

	t.Run("list error with a collected address still pings", func(t *testing.T) {
		t.Parallel()
		pinged := false
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{obj}, fmt.Errorf("list AttuneDefaults: forbidden"), func(context.Context, string) error {
			pinged = true
			return nil
		})
		assert.True(t, pinged)
		require.True(t, doctorNamed(results, "Prometheus").ok, doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "http://prometheus.example:9090")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{bad}, nil, func(context.Context, string) error {
			pinged = true
			return nil
		})
		assert.False(t, pinged)
		assert.False(t, doctorNamed(results, "Prometheus").ok)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "loopback")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.False(t, doctorNamed(results, "Prometheus").ok, "401 with configured auth is skip/WARN, not ok: %s", doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "401")
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "bearer")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 403, url: "http://mimir.example:8080/-/healthy"}
		})
		require.False(t, doctorNamed(results, "Prometheus").ok, "403 with configured auth is skip/WARN, not ok: %s", doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "403")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{authed}, nil, func(context.Context, string) error {
			return fmt.Errorf("connection refused")
		})
		require.False(t, doctorNamed(results, "Prometheus").ok, doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "connection refused")
		assert.False(t, doctorFailed(results), "Prometheus is optional")
	})

	t.Run("401 without auth config is still warn", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.False(t, doctorNamed(results, "Prometheus").ok, doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "HTTP 401")
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{plain, authed}, nil, func(context.Context, string) error {
			return &httpStatusError{status: 401, url: "http://prometheus.example:9090/-/healthy"}
		})
		require.False(t, doctorNamed(results, "Prometheus").ok, "skip-only 401 is WARN, not ok: %s", doctorNamed(results, "Prometheus").detail)
		assert.Contains(t, doctorNamed(results, "Prometheus").detail, "401")
	})
}

func TestArgsHavePrometheusOperatorAuth(t *testing.T) {
	t.Parallel()
	assert.True(t, argsHavePrometheusOperatorAuth([]string{"--leader-elect", "--prometheus-use-service-account-token"}))
	assert.True(t, argsHavePrometheusOperatorAuth([]string{"--prometheus-bearer-token-secret=attune-thanos-token"}))
	assert.False(t, argsHavePrometheusOperatorAuth([]string{"--leader-elect"}))
}

func TestRunDoctorChecks_OperatorAuth401Skip(t *testing.T) {
	t.Parallel()
	disc := resizeDiscovery("1", "32", true)
	obj := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"metricsSource": map[string]interface{}{
				"prometheus": map[string]interface{}{"address": "https://thanos.example:9091"},
			},
		},
	}}
	results := runDoctorChecksFull(context.Background(), disc, nil, []unstructured.Unstructured{obj}, nil, func(context.Context, string) error {
		return &httpStatusError{status: 401, url: "https://thanos.example:9091/-/healthy"}
	}, true)
	prom := doctorNamed(results, "Prometheus")
	require.False(t, prom.ok)
	assert.Contains(t, prom.detail, "401")
	assert.Contains(t, prom.detail, "operator Prometheus auth")
	assert.False(t, doctorFailed(results))
}

func TestPingAuthFailure(t *testing.T) {
	t.Parallel()
	assert.True(t, pingAuthFailure(&httpStatusError{status: 401, url: "http://x"}))
	assert.True(t, pingAuthFailure(&httpStatusError{status: 403, url: "http://x"}))
	assert.False(t, pingAuthFailure(&httpStatusError{status: 500, url: "http://x"}))
	assert.False(t, pingAuthFailure(fmt.Errorf("HTTP 401")))
	assert.False(t, pingAuthFailure(nil))
}

func TestPingPrometheusHealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ok hits /-/healthy", func(t *testing.T) {
		t.Parallel()
		var gotPath, gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		require.NoError(t, pingPrometheusHealthy(ctx, pingTestURL(srv.URL)+"/prom?foo=1"))
		assert.Equal(t, "/prom/-/healthy", gotPath)
		assert.Empty(t, gotQuery)
	})

	t.Run("non-200 is httpStatusError", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		err := pingPrometheusHealthy(ctx, pingTestURL(srv.URL))
		require.Error(t, err)
		var he *httpStatusError
		require.True(t, errors.As(err, &he), "got %T: %v", err, err)
		assert.Equal(t, http.StatusUnauthorized, he.status)
		assert.True(t, pingAuthFailure(err))
	})

	t.Run("loopback IP rejected before request", func(t *testing.T) {
		t.Parallel()
		err := pingPrometheusHealthy(ctx, "http://127.0.0.1:1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "loopback")
	})

	t.Run("redirect to loopback is rejected", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/-/healthy", http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		err := pingPrometheusHealthy(ctx, pingTestURL(srv.URL))
		require.Error(t, err)
		var uerr *url.Error
		require.ErrorAs(t, err, &uerr)
		assert.EqualError(t, uerr.Err, `address must not target loopback/metadata IP "127.0.0.1"`)
	})
}

// TestPingPrometheusHealthy_RedirectToCloudMetadata is not Parallel: it
// wraps DefaultTransport so a missing CheckRedirect cannot hide behind
// "lookup metadata.google.internal" (that string still contains "metadata").
func TestPingPrometheusHealthy_RedirectToCloudMetadata(t *testing.T) {
	var sawMetadata atomic.Bool
	orig := http.DefaultTransport
	http.DefaultTransport = pingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.EqualFold(r.URL.Hostname(), "metadata.google.internal") {
			sawMetadata.Store(true)
			return nil, errors.New("test: followed redirect to cloud metadata")
		}
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.google.internal/-/healthy", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	err := pingPrometheusHealthy(context.Background(), pingTestURL(srv.URL))
	require.Error(t, err)
	var uerr *url.Error
	require.ErrorAs(t, err, &uerr)
	assert.EqualError(t, uerr.Err, `address must not target cloud metadata endpoint "metadata.google.internal"`)
	assert.False(t, sawMetadata.Load(), "must not follow redirect to metadata.google.internal")
}

type pingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f pingRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
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
	code := runDoctor(context.Background(), &stdout, &stderr, resizeDiscovery("1", "32", true), nil, dyn, "default", nil, false)
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "pods/resize            ok   [required] discovered")
	assert.Contains(t, stdout.String(), "Kubernetes version     ok   [required]")
	assert.Contains(t, stdout.String(), "cgroup v2              WARN [optional] could not determine")
	assert.Contains(t, stdout.String(), "Prometheus             WARN [optional] skipped (no address on policies or defaults)")
	assert.Contains(t, stdout.String(), "AttunePolicies         WARN [optional] no AttunePolicies in scope")
	assert.Contains(t, stdout.String(), "Namespace freeze: annotate the namespace attune.io/freeze=true to skip apply. Pending safety revert still runs.")
	assert.NotContains(t, stdout.String(), "Prometheus             ok")
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	code = runDoctor(context.Background(), &stdout, &stderr, resizeDiscovery("1", "31", true), nil, dyn, "default", nil, false)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "one or more checks failed")
	assert.Contains(t, stdout.String(), "FAIL")
}

func TestRunDoctorChecks_PolicyScopeWarn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	disc := resizeDiscovery("1", "32", true)

	t.Run("zero policies", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, nil, nil, nil)
		got := results[len(results)-1]
		assert.Equal(t, "AttunePolicies", got.name)
		assert.False(t, got.required)
		assert.False(t, got.ok)
		assert.Equal(t, "no AttunePolicies in scope", got.detail)
		assert.False(t, doctorFailed(results))
	})

	t.Run("list error with zero policies is not empty scope", func(t *testing.T) {
		t.Parallel()
		results := runDoctorChecks(ctx, disc, nil, nil, fmt.Errorf("list AttunePolicies: forbidden"), nil)
		got := results[len(results)-1]
		assert.Equal(t, "AttunePolicies", got.name)
		assert.False(t, got.required)
		assert.False(t, got.ok)
		assert.Contains(t, got.detail, "could not list")
		assert.NotContains(t, got.detail, "in scope")
		assert.False(t, doctorFailed(results))
	})

	t.Run("missing Ready is not claimed Ready", func(t *testing.T) {
		t.Parallel()
		policy := unstructured.Unstructured{Object: map[string]interface{}{
			"kind":     "AttunePolicy",
			"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		}}
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{policy}, nil, nil)
		got := results[len(results)-1]
		assert.Equal(t, "AttunePolicies", got.name)
		assert.False(t, got.required)
		assert.False(t, got.ok)
		assert.NotContains(t, got.detail, "1 policies Ready")
		assert.False(t, doctorFailed(results))
	})

	t.Run("Ready False ConflictCheckFailed", func(t *testing.T) {
		t.Parallel()
		policy := unstructured.Unstructured{Object: map[string]interface{}{
			"kind":     "AttunePolicy",
			"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":    "Ready",
						"status":  "False",
						"reason":  "ConflictCheckFailed",
						"message": "Failed to list AttunePolicies for conflict detection; recommendations not computed",
					},
				},
			},
		}}
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{policy}, nil, nil)
		got := results[len(results)-1]
		assert.False(t, got.ok)
		assert.False(t, got.required)
		assert.Equal(t, "1 policies, 1 Ready=False (reasons: ConflictCheckFailed)", got.detail)
		assert.False(t, doctorFailed(results))
	})

	t.Run("all Ready", func(t *testing.T) {
		t.Parallel()
		policy := unstructured.Unstructured{Object: map[string]interface{}{
			"kind":     "AttunePolicy",
			"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
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
		results := runDoctorChecks(ctx, disc, nil, []unstructured.Unstructured{policy}, nil, nil)
		got := results[len(results)-1]
		assert.True(t, got.ok)
		assert.Equal(t, "1 policies Ready", got.detail)
		assert.False(t, doctorFailed(results))
	})
}

func TestRunDoctorChecks_CgroupOptional(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name          string
		major         string
		minor         string
		withResize    bool
		nodes         cluster.NodeLister
		wantDetail    []string
		forbidDetail  []string
		wantFailed    bool
		wantCgroupOK  bool
		wantCgroupReq bool
	}{
		{
			name:         "inconclusive default",
			major:        "1",
			minor:        "36",
			withResize:   true,
			wantDetail:   []string{"could not determine"},
			forbidDetail: []string{"failCgroupV1"},
		},
		{
			name:         "NFD CGROUP_V2 is footnote not PASS",
			major:        "1",
			minor:        "36",
			withResize:   true,
			nodes:        nfdCgroupNodeLister(true),
			wantDetail:   []string{"could not determine", "kernel.config.CGROUP_V2", "compile-time"},
			forbidDetail: []string{"failCgroupV1"},
		},
		{
			name:       "1.37+ inconclusive mentions failCgroupV1",
			major:      "1",
			minor:      "37",
			withResize: true,
			wantDetail: []string{"could not determine", "failCgroupV1"},
		},
		{
			name:         "1.36 inconclusive does not mention failCgroupV1",
			major:        "1",
			minor:        "36",
			withResize:   true,
			nodes:        nfdCgroupNodeLister(false),
			wantDetail:   []string{"could not determine"},
			forbidDetail: []string{"failCgroupV1"},
		},
		{
			name:       "missing pods/resize still required FAIL",
			major:      "1",
			minor:      "32",
			withResize: false,
			wantDetail: []string{"could not determine"},
			wantFailed: true,
		},
		{
			name:       "optional cgroup never flips doctorFailed",
			major:      "1",
			minor:      "35",
			withResize: true,
			nodes:      nfdCgroupNodeLister(true),
			wantDetail: []string{"could not determine", "compile-time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results := runDoctorChecks(ctx, resizeDiscovery(tt.major, tt.minor, tt.withResize), tt.nodes, nil, nil, nil)
			cgroup := doctorNamed(results, doctorCgroupName)
			assert.Equal(t, tt.wantCgroupReq, cgroup.required, "cgroup row must stay optional")
			assert.Equal(t, tt.wantCgroupOK, cgroup.ok, "cgroup row must not PASS")
			for _, want := range tt.wantDetail {
				assert.Contains(t, cgroup.detail, want)
			}
			for _, forbid := range tt.forbidDetail {
				assert.NotContains(t, cgroup.detail, forbid)
			}
			resize := doctorNamed(results, "pods/resize")
			if tt.withResize {
				assert.True(t, resize.ok, resize.detail)
			} else {
				assert.False(t, resize.ok)
				assert.True(t, resize.required)
			}
			assert.Equal(t, tt.wantFailed, doctorFailed(results))
		})
	}
}

func TestParseVersionPart(t *testing.T) {
	t.Parallel()
	n, err := parseVersionPart("32+")
	require.NoError(t, err)
	assert.Equal(t, 32, n)
	_, err = parseVersionPart("abc")
	assert.Error(t, err)
}
