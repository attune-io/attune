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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/attune-io/attune/internal/cluster"
	"github.com/attune-io/attune/internal/validation"
)

const (
	minKubernetesMajor    = 1
	minKubernetesMinor    = 32
	prometheusHealthyPath = "/-/healthy"
	prometheusPingTimeout = 3 * time.Second
	doctorCgroupName      = "cgroup v2"
	// nfdCgroupV2Label is Node Feature Discovery kernel compile-time.
	// It is never enough to PASS the doctor cgroup row.
	nfdCgroupV2Label = "feature.node.kubernetes.io/kernel.config.CGROUP_V2"
)

type prometheusPinger func(ctx context.Context, address string) error

// buildDoctorDiscovery loads Discovery and a NodeLister from kubeconfig.
var buildDoctorDiscovery = defaultBuildDoctorDiscovery

func defaultBuildDoctorDiscovery(kubeconfigPath, contextOverride string) (discovery.DiscoveryInterface, cluster.NodeLister, error) {
	cfg, err := loadRESTConfig(kubeconfigPath, contextOverride)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cs.Discovery(), cs.CoreV1().Nodes(), nil
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
	// Own transport when DefaultTransport is a *http.Transport. Sharing it
	// lets parallel httptest.Server.Close() abort an in-flight ping with
	// "http: CloseIdleConnections called". Tests may replace DefaultTransport
	// with a stub RoundTripper; keep that path.
	transport := http.DefaultTransport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := base.Clone()
		cloned.DisableKeepAlives = true
		transport = cloned
	}
	client := &http.Client{
		Timeout:   prometheusPingTimeout,
		Transport: transport,
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

func runDoctorChecks(ctx context.Context, disc discovery.DiscoveryInterface, nodes cluster.NodeLister, objects []unstructured.Unstructured, listErr error, ping prometheusPinger) []doctorResult {
	return runDoctorChecksFull(ctx, disc, nodes, objects, listErr, ping, false)
}

func runDoctorChecksFull(ctx context.Context, disc discovery.DiscoveryInterface, nodes cluster.NodeLister, objects []unstructured.Unstructured, listErr error, ping prometheusPinger, operatorAuth bool) []doctorResult {
	if ping == nil {
		ping = pingPrometheusHealthy
	}
	results := make([]doctorResult, 0, 5)

	caps, discErr := cluster.Discover(ctx, disc, nodes)
	results = append(results, doctorVersionResult(caps, discErr))
	results = append(results, doctorPodsResizeResult(caps, discErr))
	results = append(results, doctorCgroupResult(ctx, nodes, caps))

	targets := collectPrometheusTargets(objects...)
	if len(targets) == 0 {
		detail := "skipped (no address on policies or defaults)"
		if listErr != nil {
			detail = "skipped (could not list policies or defaults)"
		}
		results = append(results, doctorResult{
			name: "Prometheus", required: false, ok: false,
			detail: detail,
		})
		results = append(results, attunePolicyDoctorResult(objects, listErr))
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
			if (tgt.hasAuth || (operatorAuth && operatorAuthAddress(objects, addr))) && pingAuthFailure(err) {
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
		results = append(results, attunePolicyDoctorResult(objects, listErr))
		return results
	}
	detail := strings.Join(reachable, ", ") + " " + prometheusHealthyPath
	switch {
	case len(reachable) == 0 && len(skippedLocal) > 0 && len(skippedAuth) > 0:
		detail = "skipped (in-cluster address; ping is from this host, not the operator pod; HTTP 401/403 on address that uses bearer token or headers)"
	case len(reachable) == 0 && len(skippedLocal) > 0:
		detail = "skipped (in-cluster address; ping is from this host, not the operator pod)"
	case len(reachable) == 0 && len(skippedAuth) > 0:
		detail = "skipped (HTTP 401/403; address uses bearer token, headers, or operator Prometheus auth the operator would send)"
	default:
		if len(skippedLocal) > 0 {
			detail += "; skipped in-cluster " + strings.Join(skippedLocal, ", ")
		}
		if len(skippedAuth) > 0 {
			detail += "; skipped authenticated " + strings.Join(skippedAuth, ", ")
		}
	}
	results = append(results, doctorResult{
		name: "Prometheus", required: false, ok: len(reachable) > 0,
		detail: detail,
	})
	results = append(results, attunePolicyDoctorResult(objects, listErr))
	return results
}

func doctorVersionResult(caps *cluster.Capabilities, discErr error) doctorResult {
	if discErr != nil {
		return doctorResult{
			name: "Kubernetes version", required: true, detail: discErr.Error(),
		}
	}
	if caps == nil {
		return doctorResult{
			name: "Kubernetes version", required: true, detail: "server version is empty",
		}
	}
	info := &k8sversion.Info{
		GitVersion: caps.GitVersion,
		Major:      fmt.Sprintf("%d", caps.Major),
		Minor:      fmt.Sprintf("%d", caps.Minor),
	}
	if err := classifyKubernetesVersion(info); err != nil {
		return doctorResult{
			name: "Kubernetes version", required: true, detail: err.Error(),
		}
	}
	git := caps.GitVersion
	if git == "" {
		git = fmt.Sprintf("%d.%d", caps.Major, caps.Minor)
	}
	return doctorResult{
		name: "Kubernetes version", required: true, ok: true, detail: git,
	}
}

func doctorPodsResizeResult(caps *cluster.Capabilities, discErr error) doctorResult {
	if caps != nil && caps.PodsResize {
		return doctorResult{
			name: "pods/resize", required: true, ok: true, detail: "discovered",
		}
	}
	detail := "subresource not found; enable InPlacePodVerticalScaling (1.32 alpha) or use Kubernetes 1.33+"
	if discErr != nil {
		detail = discErr.Error() + "; " + detail
	}
	return doctorResult{
		name: "pods/resize", required: true, detail: detail,
	}
}

func doctorCgroupResult(ctx context.Context, nodes cluster.NodeLister, caps *cluster.Capabilities) doctorResult {
	detail := "could not determine runtime cgroup version (expected on k3s, kind, and most managed clusters)"
	if caps != nil && (caps.Major > 1 || (caps.Major == 1 && caps.Minor >= 37)) {
		detail += "; Kubernetes 1.37+ kubelet failCgroupV1 defaults to rejecting cgroup v1"
	}
	if nfdKernelCgroupV2(ctx, nodes) {
		detail += "; NFD kernel.config.CGROUP_V2=true is kernel compile-time only, not a runtime cgroup version"
	}
	return doctorResult{
		name:     doctorCgroupName,
		required: false,
		ok:       false,
		detail:   detail,
	}
}

func nfdKernelCgroupV2(ctx context.Context, nodes cluster.NodeLister) bool {
	if nodes == nil {
		return false
	}
	list, err := nodes.List(ctx, metav1.ListOptions{})
	if err != nil || list == nil {
		return false
	}
	for i := range list.Items {
		if list.Items[i].Labels[nfdCgroupV2Label] == "true" {
			return true
		}
	}
	return false
}

func attunePolicyDoctorResult(objects []unstructured.Unstructured, listErr error) doctorResult {
	var total, readyTrue, notReady, unknown int
	var reasons []string
	seen := map[string]struct{}{}
	for _, obj := range objects {
		if obj.GetKind() != "AttunePolicy" {
			continue
		}
		total++
		status, reason, _ := readyConditionFields(obj)
		switch status {
		case "True":
			readyTrue++
		case "False":
			notReady++
			if reason == "" {
				reason = "Unknown"
			}
			if _, ok := seen[reason]; ok {
				continue
			}
			seen[reason] = struct{}{}
			reasons = append(reasons, reason)
		default:
			unknown++
		}
	}
	if total == 0 {
		if listErr != nil {
			return doctorResult{
				name: "AttunePolicies", required: false,
				detail: fmt.Sprintf("could not list AttunePolicies: %v", listErr),
			}
		}
		return doctorResult{
			name: "AttunePolicies", required: false,
			detail: "no AttunePolicies in scope",
		}
	}
	incomplete := ""
	if listErr != nil {
		incomplete = "; list incomplete: " + listErr.Error()
	}
	if notReady == 0 && unknown == 0 {
		return doctorResult{
			name: "AttunePolicies", required: false, ok: true,
			detail: fmt.Sprintf("%d policies Ready", total) + incomplete,
		}
	}
	if unknown > 0 {
		return doctorResult{
			name: "AttunePolicies", required: false,
			detail: fmt.Sprintf("%d policies, %d Ready=True, %d without Ready=True",
				total, readyTrue, notReady+unknown) + incomplete,
		}
	}
	return doctorResult{
		name: "AttunePolicies", required: false,
		detail: fmt.Sprintf("%d policies, %d Ready=False (reasons: %s)",
			total, notReady, strings.Join(reasons, ", ")) + incomplete,
	}
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

func detectManagerPrometheusOperatorAuth(ctx context.Context, kubeconfigPath string) bool {
	cfg, err := loadRESTConfig(kubeconfigPath, "")
	if err != nil {
		return false
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return false
	}
	list, err := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=attune",
	})
	if err != nil {
		return false
	}
	for i := range list.Items {
		for _, c := range list.Items[i].Spec.Template.Spec.Containers {
			if argsHavePrometheusOperatorAuth(c.Args) {
				return true
			}
		}
	}
	return false
}

func argsHavePrometheusOperatorAuth(args []string) bool {
	for i := 0; i < len(args); i++ {
		if enabled, next, ok := serviceAccountTokenArg(args, i); ok {
			if enabled {
				return true
			}
			i = next
			continue
		}
		if value, next, ok := stringFlagValue(args, i, "--prometheus-bearer-token-secret"); ok {
			if strings.TrimSpace(value) != "" {
				return true
			}
			i = next
			continue
		}
		if value, next, ok := stringFlagValue(args, i, "--prometheus-query-service-account"); ok {
			if strings.TrimSpace(value) != "" {
				return true
			}
			i = next
		}
	}
	return false
}

func serviceAccountTokenArg(args []string, i int) (enabled bool, next int, ok bool) {
	const name = "--prometheus-use-service-account-token"
	a := args[i]
	if a == name {
		if i+1 < len(args) {
			if v, err := strconv.ParseBool(args[i+1]); err == nil {
				return v, i + 1, true
			}
		}
		return true, i, true
	}
	if !strings.HasPrefix(a, name+"=") {
		return false, i, false
	}
	v, err := strconv.ParseBool(strings.TrimPrefix(a, name+"="))
	if err != nil {
		return false, i, true
	}
	return v, i, true
}

func stringFlagValue(args []string, i int, name string) (string, int, bool) {
	a := args[i]
	if strings.HasPrefix(a, name+"=") {
		return strings.TrimPrefix(a, name+"="), i, true
	}
	if a != name {
		return "", i, false
	}
	if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
		return args[i+1], i + 1, true
	}
	return "", i, true
}

// operatorAuthAddress reports whether the operator would attach its own
// Prometheus credentials to addr. The pick matches fetchDefaults: the
// lexicographically smallest cluster AttuneDefaults, and the smallest
// AttuneNamespaceDefaults in each namespace. An address on a policy or on
// that selected namespace object does not get operator auth.
func operatorAuthAddress(objects []unstructured.Unstructured, addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	type namedAddr struct {
		name string
		addr string
	}
	policyAddrs := map[string]struct{}{}
	nsPick := map[string]namedAddr{}
	var cluster *namedAddr
	for i := range objects {
		obj := objects[i]
		objAddr := strings.TrimSpace(getNestedString(obj, "spec", "metricsSource", "prometheus", "address"))
		switch obj.GetKind() {
		case "AttunePolicy":
			if objAddr != "" {
				policyAddrs[objAddr] = struct{}{}
			}
		case "AttuneNamespaceDefaults":
			cur, ok := nsPick[obj.GetNamespace()]
			if !ok || obj.GetName() < cur.name {
				nsPick[obj.GetNamespace()] = namedAddr{name: obj.GetName(), addr: objAddr}
			}
		case "AttuneDefaults":
			if cluster == nil || obj.GetName() < cluster.name {
				picked := namedAddr{name: obj.GetName(), addr: objAddr}
				cluster = &picked
			}
		}
	}
	if cluster == nil || cluster.addr != addr {
		return false
	}
	if _, blocked := policyAddrs[addr]; blocked {
		return false
	}
	for _, picked := range nsPick {
		if picked.addr == addr {
			return false
		}
	}
	return true
}

func runDoctor(ctx context.Context, stdout, stderr io.Writer, disc discovery.DiscoveryInterface, nodes cluster.NodeLister, dynClient dynamic.Interface, namespace string, ping prometheusPinger, operatorAuth bool) int {
	objects, err := listDoctorObjects(ctx, dynClient, namespace)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: %v\n", err)
	}
	results := runDoctorChecksFull(ctx, disc, nodes, objects, err, ping, operatorAuth)
	printDoctorResults(stdout, results)
	fmt.Fprintln(stdout, "Namespace freeze: annotate the namespace attune.io/freeze=true to skip apply. Pending safety revert still runs.")
	if doctorFailed(results) {
		fmt.Fprintln(stderr, "doctor: one or more checks failed")
		return 1
	}
	return 0
}
