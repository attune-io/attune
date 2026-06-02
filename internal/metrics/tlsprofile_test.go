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
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestTLSMinVersionForProfile(t *testing.T) {
	tests := []struct {
		profile string
		want    uint16
	}{
		{"Modern", tls.VersionTLS13},
		{"Intermediate", tls.VersionTLS12},
		{"Old", tls.VersionTLS10},
		{"Custom", 0},
		{"", tls.VersionTLS12},
		{"Unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			assert.Equal(t, tt.want, TLSMinVersionForProfile(tt.profile))
		})
	}
}

// fakeAPIServer builds an httptest.Server that simulates a Kubernetes API
// server with optional OpenShift config.openshift.io/v1 resources.
// If tlsProfileType is empty, the apiserver resource has no TLS profile set.
// If includeOpenShift is false, the discovery response omits the OpenShift API group.
func fakeAPIServer(t *testing.T, includeOpenShift bool, tlsProfileType string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Discovery: /api
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIVersions{
			Versions: []string{"v1"},
		})
	})

	// Discovery: /apis
	groups := []metav1.APIGroup{}
	if includeOpenShift {
		groups = append(groups, metav1.APIGroup{
			Name: "config.openshift.io",
			Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "config.openshift.io/v1", Version: "v1"},
			},
		})
	}
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIGroupList{Groups: groups})
	})

	// Discovery: /apis/config.openshift.io/v1
	if includeOpenShift {
		mux.HandleFunc("/apis/config.openshift.io/v1", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metav1.APIResourceList{
				GroupVersion: "config.openshift.io/v1",
				APIResources: []metav1.APIResource{
					{Name: "apiservers", Kind: "APIServer", Namespaced: false},
				},
			})
		})

		// APIServer resource
		mux.HandleFunc("/apis/config.openshift.io/v1/apiservers/cluster", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"apiVersion": "config.openshift.io/v1",
				"kind":       "APIServer",
				"metadata":   map[string]string{"name": "cluster"},
				"spec":       map[string]interface{}{},
			}
			if tlsProfileType != "" {
				resp["spec"] = map[string]interface{}{
					"tlsSecurityProfile": map[string]string{"type": tlsProfileType},
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
	}

	return httptest.NewServer(mux)
}

func clientsetForServer(t *testing.T, server *httptest.Server) *kubernetes.Clientset {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	return cs
}

func TestDetectOpenShiftTLSProfile_VanillaKubernetes(t *testing.T) {
	server := fakeAPIServer(t, false, "")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(0), result, "vanilla K8s should return 0 (Go defaults)")
}

func TestDetectOpenShiftTLSProfile_OpenShiftModern(t *testing.T) {
	server := fakeAPIServer(t, true, "Modern")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS13), result)
}

func TestDetectOpenShiftTLSProfile_OpenShiftIntermediate(t *testing.T) {
	server := fakeAPIServer(t, true, "Intermediate")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS12), result)
}

func TestDetectOpenShiftTLSProfile_OpenShiftOld(t *testing.T) {
	server := fakeAPIServer(t, true, "Old")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS10), result)
}

func TestDetectOpenShiftTLSProfile_NoTLSProfileSet(t *testing.T) {
	server := fakeAPIServer(t, true, "")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS12), result, "unset profile should default to Intermediate (TLS 1.2)")
}

func TestDetectOpenShiftTLSProfile_PartialDiscoveryFailure(t *testing.T) {
	// Simulate a cluster where OpenShift's API group succeeds but another
	// group fails discovery (e.g., a broken third-party CRD controller).
	// ServerGroupsAndResources returns partial results alongside an error;
	// we must still detect the OpenShift TLS profile.
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIVersions{Versions: []string{"v1"}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIGroupList{Groups: []metav1.APIGroup{
			{
				Name: "config.openshift.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "config.openshift.io/v1", Version: "v1"},
				},
			},
			{
				Name: "broken.example.com",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "broken.example.com/v1", Version: "v1"},
				},
			},
		}})
	})
	mux.HandleFunc("/apis/config.openshift.io/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIResourceList{
			GroupVersion: "config.openshift.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "apiservers", Kind: "APIServer", Namespaced: false},
			},
		})
	})
	// Broken group returns 500
	mux.HandleFunc("/apis/broken.example.com/v1", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	})
	mux.HandleFunc("/apis/config.openshift.io/v1/apiservers/cluster", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "APIServer",
			"metadata":   map[string]string{"name": "cluster"},
			"spec": map[string]interface{}{
				"tlsSecurityProfile": map[string]string{"type": "Modern"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS13), result, "should detect TLS profile despite partial discovery failure")
}

func TestDetectOpenShiftTLSProfile_UnreachableAPI(t *testing.T) {
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	result := DetectOpenShiftTLSProfile(cs, logr.Discard())
	assert.Equal(t, uint16(0), result, "unreachable API should return 0 (Go defaults)")
}

// fakeAPIServerCustomTLS builds an httptest.Server that simulates an OpenShift
// cluster with a Custom TLS security profile containing an explicit minTLSVersion.
func fakeAPIServerCustomTLS(t *testing.T, minTLSVersion string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIVersions{Versions: []string{"v1"}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIGroupList{Groups: []metav1.APIGroup{{
			Name: "config.openshift.io",
			Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "config.openshift.io/v1", Version: "v1"},
			},
		}}})
	})
	mux.HandleFunc("/apis/config.openshift.io/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metav1.APIResourceList{
			GroupVersion: "config.openshift.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "apiservers", Kind: "APIServer", Namespaced: false},
			},
		})
	})
	mux.HandleFunc("/apis/config.openshift.io/v1/apiservers/cluster", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		spec := map[string]interface{}{
			"tlsSecurityProfile": map[string]interface{}{
				"type": "Custom",
				"custom": map[string]interface{}{
					"minTLSVersion": minTLSVersion,
				},
			},
		}
		resp := map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "APIServer",
			"metadata":   map[string]string{"name": "cluster"},
			"spec":       spec,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func TestDetectOpenShiftTLSProfile_CustomTLS13(t *testing.T) {
	server := fakeAPIServerCustomTLS(t, "VersionTLS13")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS13), result, "Custom profile with VersionTLS13 should return TLS 1.3")
}

func TestDetectOpenShiftTLSProfile_CustomTLS12(t *testing.T) {
	server := fakeAPIServerCustomTLS(t, "VersionTLS12")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS12), result, "Custom profile with VersionTLS12 should return TLS 1.2")
}

func TestDetectOpenShiftTLSProfile_CustomUnrecognizedVersion(t *testing.T) {
	server := fakeAPIServerCustomTLS(t, "VersionTLS99")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS12), result, "Custom profile with unrecognized version should fall back to TLS 1.2")
}

func TestDetectOpenShiftTLSProfile_CustomNoMinVersion(t *testing.T) {
	// Custom profile without the minTLSVersion field.
	server := fakeAPIServer(t, true, "Custom")
	defer server.Close()

	result := DetectOpenShiftTLSProfile(clientsetForServer(t, server), logr.Discard())
	assert.Equal(t, uint16(tls.VersionTLS12), result, "Custom profile without minTLSVersion should fall back to TLS 1.2")
}

func TestParseOpenShiftTLSVersion(t *testing.T) {
	tests := []struct {
		version string
		want    uint16
	}{
		{"VersionTLS10", tls.VersionTLS10},
		{"VersionTLS11", tls.VersionTLS11},
		{"VersionTLS12", tls.VersionTLS12},
		{"VersionTLS13", tls.VersionTLS13},
		{"VersionTLS99", 0},
		{"", 0},
		{"TLS13", 0},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			assert.Equal(t, tt.want, parseOpenShiftTLSVersion(tt.version))
		})
	}
}
