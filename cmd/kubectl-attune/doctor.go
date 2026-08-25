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
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/attune-io/attune/internal/validation"
)

const (
	minKubernetesMajor     = 1
	minKubernetesMinor     = 32
	prometheusHealthyPath  = "/-/healthy"
	prometheusPingTimeout  = 3 * time.Second
	podsResizeResourceName = "pods/resize"
)

// doctorDiscovery is the discovery subset used by kubectl attune doctor.
type doctorDiscovery interface {
	ServerVersion() (*k8sversion.Info, error)
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

type prometheusPinger func(ctx context.Context, address string) error

// buildDoctorDiscovery loads a typed clientset Discovery from kubeconfig.
// Tests replace this to inject a fake discovery client.
var buildDoctorDiscovery = defaultBuildDoctorDiscovery

func defaultBuildDoctorDiscovery(kubeconfigPath, contextOverride string) (doctorDiscovery, error) {
	cfg, err := loadRESTConfig(kubeconfigPath, contextOverride)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return cs.Discovery(), nil
}

func loadRESTConfig(kubeconfigPath, contextOverride string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextOverride != "" {
		overrides.CurrentContext = contextOverride
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	return kubeConfig.ClientConfig()
}

func parseVersionPart(s string) (int, error) {
	n := 0
	found := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			found = true
			n = n*10 + int(r-'0')
			continue
		}
		if found {
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return n, nil
}

// classifyKubernetesVersion reports whether the cluster is Attune's minimum
// (Kubernetes 1.32+). Minor may look like "32", "32+", or "32.4".
func classifyKubernetesVersion(info *k8sversion.Info) error {
	if info == nil {
		return fmt.Errorf("server version is empty")
	}
	major, err := parseVersionPart(info.Major)
	if err != nil {
		return fmt.Errorf("parse major %q: %w", info.Major, err)
	}
	minor, err := parseVersionPart(info.Minor)
	if err != nil {
		return fmt.Errorf("parse minor %q: %w", info.Minor, err)
	}
	if major > minKubernetesMajor || (major == minKubernetesMajor && minor >= minKubernetesMinor) {
		return nil
	}
	return fmt.Errorf("cluster version %d.%d is below Attune's minimum 1.32 (in-place pod resize)", major, minor)
}

// hasPodsResizeSubresource reports whether discovery lists pods/resize.
func hasPodsResizeSubresource(lists []*metav1.APIResourceList) bool {
	for _, list := range lists {
		if list == nil {
			continue
		}
		if list.GroupVersion != "v1" {
			continue
		}
		for _, res := range list.APIResources {
			if res.Name == podsResizeResourceName {
				return true
			}
		}
	}
	return false
}

func collectPrometheusAddresses(objects ...unstructured.Unstructured) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, obj := range objects {
		addr := strings.TrimSpace(getNestedString(obj, "spec", "metricsSource", "prometheus", "address"))
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	return out
}

func pingPrometheusHealthy(ctx context.Context, address string) error {
	if err := validation.PrometheusAddress(address); err != nil {
		return err
	}
	u, err := url.Parse(address)
	if err != nil {
		return err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + prometheusHealthyPath
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: prometheusPingTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u.Redacted(), resp.StatusCode)
	}
	return nil
}

func listDoctorObjects(ctx context.Context, dynClient dynamic.Interface, namespace string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	policies, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !isNoResourceMatch(err) {
		return nil, fmt.Errorf("list AttunePolicies: %w", err)
	}
	if policies != nil {
		out = append(out, policies.Items...)
	}
	defaults, err := dynClient.Resource(defaultsGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !isNoResourceMatch(err) {
		return nil, fmt.Errorf("list AttuneDefaults: %w", err)
	}
	if defaults != nil {
		out = append(out, defaults.Items...)
	}
	nsDefaults, err := dynClient.Resource(namespaceDefaultsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !isNoResourceMatch(err) {
		return nil, fmt.Errorf("list AttuneNamespaceDefaults: %w", err)
	}
	if nsDefaults != nil {
		out = append(out, nsDefaults.Items...)
	}
	return out, nil
}

type doctorResult struct {
	name     string
	required bool
	ok       bool
	detail   string
}

func runDoctorChecks(ctx context.Context, disc doctorDiscovery, objects []unstructured.Unstructured, ping prometheusPinger) []doctorResult {
	if ping == nil {
		ping = pingPrometheusHealthy
	}
	results := make([]doctorResult, 0, 3)

	ver, err := disc.ServerVersion()
	if err != nil {
		results = append(results, doctorResult{
			name: "Kubernetes version", required: true, detail: err.Error(),
		})
	} else if err := classifyKubernetesVersion(ver); err != nil {
		results = append(results, doctorResult{
			name: "Kubernetes version", required: true, detail: err.Error(),
		})
	} else {
		git := ver.GitVersion
		if git == "" {
			git = ver.Major + "." + ver.Minor
		}
		results = append(results, doctorResult{
			name: "Kubernetes version", required: true, ok: true, detail: git,
		})
	}

	_, lists, err := disc.ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		results = append(results, doctorResult{
			name: "pods/resize", required: true, detail: err.Error(),
		})
	} else if !hasPodsResizeSubresource(lists) {
		results = append(results, doctorResult{
			name:     "pods/resize",
			required: true,
			detail:   "subresource not found; enable InPlacePodVerticalScaling (1.32 alpha) or use Kubernetes 1.33+",
		})
	} else {
		results = append(results, doctorResult{
			name: "pods/resize", required: true, ok: true, detail: "discovered",
		})
	}

	addrs := collectPrometheusAddresses(objects...)
	if len(addrs) == 0 {
		results = append(results, doctorResult{
			name: "Prometheus", required: false, ok: true,
			detail: "skipped (no address on policies or defaults)",
		})
		return results
	}
	var failed []string
	for _, addr := range addrs {
		if err := validation.PrometheusAddress(addr); err != nil {
			failed = append(failed, addr+": "+err.Error())
			continue
		}
		if err := ping(ctx, addr); err != nil {
			failed = append(failed, addr+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		results = append(results, doctorResult{
			name: "Prometheus", required: false, detail: strings.Join(failed, "; "),
		})
		return results
	}
	results = append(results, doctorResult{
		name: "Prometheus", required: false, ok: true,
		detail: strings.Join(addrs, ", ") + " " + prometheusHealthyPath,
	})
	return results
}

func doctorFailed(results []doctorResult) bool {
	for _, r := range results {
		if r.required && !r.ok {
			return true
		}
	}
	return false
}

func printDoctorResults(w io.Writer, results []doctorResult) {
	for _, r := range results {
		status := "FAIL"
		if r.ok {
			status = "ok"
		}
		kind := "required"
		if !r.required {
			kind = "optional"
		}
		fmt.Fprintf(w, "%-22s %-4s [%s] %s\n", r.name, status, kind, r.detail)
	}
}

func runDoctor(ctx context.Context, stdout, stderr io.Writer, disc doctorDiscovery, dynClient dynamic.Interface, namespace string, ping prometheusPinger) int {
	objects, err := listDoctorObjects(ctx, dynClient, namespace)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	results := runDoctorChecks(ctx, disc, objects, ping)
	printDoctorResults(stdout, results)
	if doctorFailed(results) {
		fmt.Fprintln(stderr, "doctor: one or more checks failed")
		return 1
	}
	return 0
}
