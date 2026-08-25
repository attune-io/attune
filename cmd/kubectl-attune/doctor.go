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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

type prometheusDoctorTarget struct {
	address string
	hasAuth bool
}

func prometheusObjectHasAuth(obj unstructured.Unstructured) bool {
	secret, found, err := unstructured.NestedMap(obj.Object, "spec", "metricsSource", "prometheus", "bearerTokenSecret")
	if err == nil && found && len(secret) > 0 {
		return true
	}
	headers, found, err := unstructured.NestedStringMap(obj.Object, "spec", "metricsSource", "prometheus", "headers")
	if err == nil && found && len(headers) > 0 {
		return true
	}
	return false
}

func collectPrometheusTargets(objects ...unstructured.Unstructured) []prometheusDoctorTarget {
	seen := map[string]int{}
	var out []prometheusDoctorTarget
	for _, obj := range objects {
		addr := strings.TrimSpace(getNestedString(obj, "spec", "metricsSource", "prometheus", "address"))
		if addr == "" {
			continue
		}
		hasAuth := prometheusObjectHasAuth(obj)
		if i, ok := seen[addr]; ok {
			if hasAuth {
				out[i].hasAuth = true
			}
			continue
		}
		seen[addr] = len(out)
		out = append(out, prometheusDoctorTarget{address: addr, hasAuth: hasAuth})
	}
	return out
}

type httpStatusError struct {
	status int
	url    string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d", e.url, e.status)
}

func pingAuthFailure(err error) bool {
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.status == http.StatusUnauthorized || he.status == http.StatusForbidden
	}
	return false
}

// clusterLocalPrometheusHost is true for Service DNS names that resolve
// only inside the cluster. Doctor runs on the kubectl host, so those
// addresses cannot be pinged the way the operator would.
func clusterLocalPrometheusHost(address string) bool {
	u, err := url.Parse(address)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
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
	client := &http.Client{
		Timeout: prometheusPingTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := validation.PrometheusAddress(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{status: resp.StatusCode, url: u.Redacted()}
	}
	return nil
}

func appendListedResources(ctx context.Context, dynClient dynamic.Interface, resource schema.GroupVersionResource, namespace, kind string, out []unstructured.Unstructured, errs []error) ([]unstructured.Unstructured, []error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace == "" {
		list, err = dynClient.Resource(resource).List(ctx, metav1.ListOptions{})
	} else {
		list, err = dynClient.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	}
	if err != nil && !apierrors.IsNotFound(err) && !isNoResourceMatch(err) {
		return out, append(errs, fmt.Errorf("list %s: %w", kind, err))
	}
	if list != nil {
		out = append(out, list.Items...)
	}
	return out, errs
}

func listDoctorObjects(ctx context.Context, dynClient dynamic.Interface, namespace string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	var errs []error
	out, errs = appendListedResources(ctx, dynClient, gvr, namespace, "AttunePolicies", out, errs)
	out, errs = appendListedResources(ctx, dynClient, defaultsGVR, "", "AttuneDefaults", out, errs)
	out, errs = appendListedResources(ctx, dynClient, namespaceDefaultsGVR, namespace, "AttuneNamespaceDefaults", out, errs)
	return out, errors.Join(errs...)
}

type doctorResult struct {
	name     string
	required bool
	ok       bool
	detail   string
}

func runDoctorChecks(ctx context.Context, disc doctorDiscovery, objects []unstructured.Unstructured, listErr error, ping prometheusPinger) []doctorResult {
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
		detail := "subresource not found; enable InPlacePodVerticalScaling (1.32 alpha) or use Kubernetes 1.33+"
		if err != nil {
			detail = err.Error() + "; " + detail
		}
		results = append(results, doctorResult{
			name: "pods/resize", required: true, detail: detail,
		})
	} else {
		results = append(results, doctorResult{
			name: "pods/resize", required: true, ok: true, detail: "discovered",
		})
	}

	targets := collectPrometheusTargets(objects...)
	if len(targets) == 0 {
		detail := "skipped (no address on policies or defaults)"
		if listErr != nil {
			detail = "skipped (could not list policies or defaults)"
		}
		results = append(results, doctorResult{
			name: "Prometheus", required: false, ok: true,
			detail: detail,
		})
		return results
	}
	var failed []string
	var reachable []string
	var skippedLocal []string
	var skippedAuth []string
	for _, tgt := range targets {
		addr := tgt.address
		if err := validation.PrometheusAddress(addr); err != nil {
			failed = append(failed, addr+": "+err.Error())
			continue
		}
		if clusterLocalPrometheusHost(addr) {
			skippedLocal = append(skippedLocal, addr)
			continue
		}
		if err := ping(ctx, addr); err != nil {
			if tgt.hasAuth && pingAuthFailure(err) {
				skippedAuth = append(skippedAuth, addr)
				continue
			}
			failed = append(failed, addr+": "+err.Error())
			continue
		}
		reachable = append(reachable, addr)
	}
	if len(failed) > 0 {
		results = append(results, doctorResult{
			name: "Prometheus", required: false, detail: strings.Join(failed, "; "),
		})
		return results
	}
	detail := strings.Join(reachable, ", ") + " " + prometheusHealthyPath
	switch {
	case len(reachable) == 0 && len(skippedLocal) > 0 && len(skippedAuth) > 0:
		detail = "skipped (in-cluster address; ping is from this host, not the operator pod; HTTP 401/403 on address that uses bearer token or headers)"
	case len(reachable) == 0 && len(skippedLocal) > 0:
		detail = "skipped (in-cluster address; ping is from this host, not the operator pod)"
	case len(reachable) == 0 && len(skippedAuth) > 0:
		detail = "skipped (HTTP 401/403; address uses bearer token or headers the operator would send)"
	default:
		if len(skippedLocal) > 0 {
			detail += "; skipped in-cluster " + strings.Join(skippedLocal, ", ")
		}
		if len(skippedAuth) > 0 {
			detail += "; skipped authenticated " + strings.Join(skippedAuth, ", ")
		}
	}
	results = append(results, doctorResult{
		name: "Prometheus", required: false, ok: true,
		detail: detail,
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
		} else if !r.required {
			status = "WARN"
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
		fmt.Fprintf(stderr, "Warning: %v\n", err)
	}
	results := runDoctorChecks(ctx, disc, objects, err, ping)
	printDoctorResults(stdout, results)
	if doctorFailed(results) {
		fmt.Fprintln(stderr, "doctor: one or more checks failed")
		return 1
	}
	return 0
}
