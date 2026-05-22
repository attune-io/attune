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
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"
)

// GVRs used by the wizard for Kubernetes discovery.
var (
	namespacesGVR   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	deploymentsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	statefulsetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	daemonsetsGVR   = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	servicesGVR     = schema.GroupVersionResource{Version: "v1", Resource: "services"}
)

// prompter abstracts user interaction for testing.
type prompter interface {
	Select(label string, options []string) (int, error)
	Input(label string, defaultVal string) (string, error)
	Confirm(label string, defaultVal bool) (bool, error)
}

// interactivePrompter reads from stdin.
type interactivePrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func newInteractivePrompter() *interactivePrompter {
	return &interactivePrompter{reader: bufio.NewReader(os.Stdin), out: os.Stdout}
}

func (p *interactivePrompter) Select(label string, options []string) (int, error) {
	fmt.Fprintf(p.out, "? %s\n", label)
	for i, opt := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprintf(p.out, "Enter number [1]: ")
	line, err := p.reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return 0, fmt.Errorf("invalid selection %q; enter a number between 1 and %d", line, len(options))
	}
	return n - 1, nil
}

func (p *interactivePrompter) Input(label, defaultVal string) (string, error) {
	if defaultVal != "" {
		fmt.Fprintf(p.out, "? %s [%s]: ", label, defaultVal)
	} else {
		fmt.Fprintf(p.out, "? %s: ", label)
	}
	line, err := p.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal, nil
	}
	return line, nil
}

func (p *interactivePrompter) Confirm(label string, defaultVal bool) (bool, error) {
	hint := "[Y/n]"
	if !defaultVal {
		hint = "[y/N]"
	}
	fmt.Fprintf(p.out, "? %s %s: ", label, hint)
	line, err := p.reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defaultVal, nil
	}
	return line == "y" || line == "yes", nil
}

// runWizard dispatches wizard subcommands.
func runWizard(ctx context.Context, dynClient dynamic.Interface, namespace string, args []string, p prompter) int {
	subcmd := ""
	if len(args) > 0 {
		subcmd = args[0]
	}
	switch subcmd {
	case "":
		if err := wizardCreate(ctx, dynClient, namespace, p); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	case "promote":
		if err := wizardPromote(ctx, dynClient, namespace, p); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown wizard subcommand: %s\nUsage: kubectl rightsize wizard [promote]\n", subcmd)
		return 1
	}
	return 0
}

// resolveNamespace returns the provided namespace or prompts the user to select one.
func resolveNamespace(ctx context.Context, dynClient dynamic.Interface, namespace string, p prompter) (string, error) {
	if namespace != "" {
		return namespace, nil
	}
	return selectNamespace(ctx, dynClient, p)
}

// wizardCreate guides the user through creating a RightSizePolicy.
func wizardCreate(ctx context.Context, dynClient dynamic.Interface, namespace string, p prompter) error {
	fmt.Println("Welcome to kube-rightsize! Let's create a RightSizePolicy.")
	fmt.Println()

	// 1. Namespace selection.
	ns, err := resolveNamespace(ctx, dynClient, namespace, p)
	if err != nil {
		return err
	}
	if namespace != "" {
		fmt.Printf("Using namespace: %s\n\n", ns)
	}

	// 2. Workload kind.
	kinds := []string{"Deployment", "StatefulSet", "DaemonSet"}
	kindIdx, err := p.Select("What kind of workload?", kinds)
	if err != nil {
		return err
	}
	kind := kinds[kindIdx]

	// 3. Workload selection.
	workloadName, err := selectWorkload(ctx, dynClient, ns, kind, p)
	if err != nil {
		return err
	}

	// 4. Prometheus auto-detection.
	promAddr, err := detectOrPromptPrometheus(ctx, dynClient, p)
	if err != nil {
		return err
	}

	// 5. CPU percentile.
	cpuOptions := []string{"P95 (recommended for most workloads)", "P99 (conservative)", "P90 (aggressive)", "P50 (very aggressive)"}
	cpuPercentiles := []int32{95, 99, 90, 50}
	cpuIdx, err := p.Select("CPU recommendation percentile:", cpuOptions)
	if err != nil {
		return err
	}

	// 6. Memory percentile.
	memOptions := []string{"P99 (recommended, prevents OOM)", "P95 (moderate risk)"}
	memPercentiles := []int32{99, 95}
	memIdx, err := p.Select("Memory recommendation percentile:", memOptions)
	if err != nil {
		return err
	}

	// 7. Starting mode.
	modeOptions := []string{"Recommend (safe: observe only)", "Auto (resize all pods)", "Observe (metrics collection only)"}
	modeValues := []string{"Recommend", "Auto", "Observe"}
	modeIdx, err := p.Select("Starting mode:", modeOptions)
	if err != nil {
		return err
	}

	// 8. Build policy name.
	policyName := workloadName + "-rightsize"

	// 9. Generate YAML.
	policy := buildPolicyObject(ns, policyName, kind, workloadName, promAddr,
		cpuPercentiles[cpuIdx], memPercentiles[memIdx], modeValues[modeIdx])

	yamlBytes, err := marshalPolicyYAML(policy)
	if err != nil {
		return fmt.Errorf("generating YAML: %w", err)
	}

	fmt.Printf("\n%s\n", string(yamlBytes))

	// 10. Apply or save.
	actionOptions := []string{"Apply to cluster", "Save to file", "Cancel"}
	actionIdx, err := p.Select("What would you like to do?", actionOptions)
	if err != nil {
		return err
	}

	switch actionIdx {
	case 0:
		_, err := dynClient.Resource(gvr).Namespace(ns).Create(ctx, policy, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating policy: %w", err)
		}
		fmt.Printf("RightSizePolicy %q created in namespace %q.\n", policyName, ns)
		fmt.Println("\nTip: Run \"kubectl rightsize status -w\" to watch data collection progress.")
	case 1:
		filename, err := p.Input("Filename", policyName+".yaml")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filename, yamlBytes, 0o600); err != nil {
			return fmt.Errorf("writing file: %w", err)
		}
		fmt.Printf("Saved to %s\n", filename)
	default:
		fmt.Println("Cancelled.")
	}
	return nil
}

// wizardPromote guides mode promotion of an existing policy.
func wizardPromote(ctx context.Context, dynClient dynamic.Interface, namespace string, p prompter) error {
	ns, err := resolveNamespace(ctx, dynClient, namespace, p)
	if err != nil {
		return err
	}

	// List policies.
	list, err := dynClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing policies: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no RightSizePolicies found in namespace %q", ns)
	}

	// Select policy.
	policyOptions := make([]string, len(list.Items))
	for i, item := range list.Items {
		mode := getNestedString(item, "spec", "updateStrategy", "mode")
		ready := policyReadyReason(item)
		policyOptions[i] = fmt.Sprintf("%s (mode: %s, status: %s)", item.GetName(), mode, ready)
	}
	policyIdx, err := p.Select("Select a policy to promote:", policyOptions)
	if err != nil {
		return err
	}

	selected := &list.Items[policyIdx]
	currentMode := getNestedString(*selected, "spec", "updateStrategy", "mode")
	fmt.Printf("\nPolicy: %s/%s (current mode: %s)\n", ns, selected.GetName(), currentMode)

	// Show recommendation summary.
	recs, found, _ := unstructured.NestedSlice(selected.Object, "status", "recommendations")
	if found && len(recs) > 0 {
		fmt.Println("Recommendations:")
		for _, r := range recs {
			rec, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			workload, _ := rec["workload"].(string)
			containers, _ := rec["containers"].([]interface{})
			for _, c := range containers {
				cont, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := cont["name"].(string)
				cur, _ := cont["current"].(map[string]interface{})
				recVals, _ := cont["recommended"].(map[string]interface{})
				curCPU, _ := cur["cpuRequest"].(string)
				recCPU, _ := recVals["cpuRequest"].(string)
				curMem, _ := cur["memoryRequest"].(string)
				recMem, _ := recVals["memoryRequest"].(string)
				fmt.Printf("  %s/%s: CPU %s -> %s, Memory %s -> %s\n",
					workload, name, curCPU, recCPU, curMem, recMem)
			}
		}
	} else {
		fmt.Println("No recommendations available yet.")
	}

	// Select target mode.
	modeOptions := []string{"Recommend", "Canary", "Auto", "OneShot", "Observe"}
	modeIdx, err := p.Select(fmt.Sprintf("Promote to which mode? (current: %s)", currentMode), modeOptions)
	if err != nil {
		return err
	}
	newMode := modeOptions[modeIdx]

	if newMode == currentMode {
		fmt.Println("Already in that mode. No changes made.")
		return nil
	}

	ok, err := p.Confirm(fmt.Sprintf("Change mode from %s to %s?", currentMode, newMode), true)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Cancelled.")
		return nil
	}

	// Re-fetch for fresh resourceVersion.
	fresh, err := dynClient.Resource(gvr).Namespace(ns).Get(ctx, selected.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-fetching policy: %w", err)
	}
	if err := unstructured.SetNestedField(fresh.Object, newMode, "spec", "updateStrategy", "mode"); err != nil {
		return fmt.Errorf("setting mode: %w", err)
	}
	_, err = dynClient.Resource(gvr).Namespace(ns).Update(ctx, fresh, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating policy: %w", err)
	}
	fmt.Printf("Policy %q promoted to %s mode.\n", selected.GetName(), newMode)
	return nil
}

// selectNamespace lists cluster namespaces and prompts the user.
func selectNamespace(ctx context.Context, dynClient dynamic.Interface, p prompter) (string, error) {
	nsList, err := dynClient.Resource(namespacesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing namespaces: %w", err)
	}
	if len(nsList.Items) == 0 {
		return "", fmt.Errorf("no namespaces found")
	}

	// Put "default" first if present.
	names := make([]string, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		names = append(names, ns.GetName())
	}
	for i, n := range names {
		if n == "default" && i > 0 {
			names[0], names[i] = names[i], names[0]
			break
		}
	}

	idx, err := p.Select("Select a namespace:", names)
	if err != nil {
		return "", err
	}
	return names[idx], nil
}

// selectWorkload lists workloads of the given kind and prompts the user.
func selectWorkload(ctx context.Context, dynClient dynamic.Interface, namespace, kind string, p prompter) (string, error) {
	workloadGVR := kindToGVR(kind)
	list, err := dynClient.Resource(workloadGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing %ss: %w", kind, err)
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("no %ss found in namespace %q", kind, namespace)
	}

	options := make([]string, len(list.Items))
	for i, item := range list.Items {
		replicas := getNestedInt64(item, "spec", "replicas")
		options[i] = fmt.Sprintf("%s (%d replicas)", item.GetName(), replicas)
	}

	idx, err := p.Select(fmt.Sprintf("Select a %s:", kind), options)
	if err != nil {
		return "", err
	}
	return list.Items[idx].GetName(), nil
}

func kindToGVR(kind string) schema.GroupVersionResource {
	switch kind {
	case "StatefulSet":
		return statefulsetsGVR
	case "DaemonSet":
		return daemonsetsGVR
	default:
		return deploymentsGVR
	}
}

// detectOrPromptPrometheus auto-detects Prometheus services and falls back to manual input.
func detectOrPromptPrometheus(ctx context.Context, dynClient dynamic.Interface, p prompter) (string, error) {
	detected := detectPrometheus(ctx, dynClient)
	if len(detected) > 0 {
		options := append(detected, "Enter manually")
		idx, err := p.Select("Prometheus address (auto-detected):", options)
		if err != nil {
			return "", err
		}
		if idx < len(detected) {
			return detected[idx], nil
		}
	}
	addr, err := p.Input("Prometheus address", "")
	if err != nil {
		return "", err
	}
	if addr == "" {
		return "", fmt.Errorf("prometheus address is required")
	}
	return addr, nil
}

// detectPrometheus scans cluster services for Prometheus-like endpoints.
func detectPrometheus(ctx context.Context, dynClient dynamic.Interface) []string {
	svcList, err := dynClient.Resource(servicesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}

	promKeywords := []string{"prometheus", "thanos-query", "vmsingle", "vmselect", "mimir-query"}
	var results []string

	for _, svc := range svcList.Items {
		name := svc.GetName()
		ns := svc.GetNamespace()
		nameLower := strings.ToLower(name)

		match := false
		for _, kw := range promKeywords {
			if strings.Contains(nameLower, kw) {
				match = true
				break
			}
		}
		if !match {
			continue
		}

		ports, found, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
		if !found || len(ports) == 0 {
			continue
		}
		port, _ := ports[0].(map[string]interface{})
		portNum, _, _ := unstructured.NestedInt64(port, "port")
		if portNum == 0 {
			portNum = 80
		}
		results = append(results, fmt.Sprintf("http://%s.%s:%d", name, ns, portNum))
	}
	return results
}

// buildPolicyObject constructs an unstructured RightSizePolicy.
func buildPolicyObject(namespace, name, kind, workloadName, promAddr string, cpuPercentile, memPercentile int32, mode string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rightsize.io/v1alpha1",
			"kind":       "RightSizePolicy",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"targetRef": map[string]interface{}{
					"kind": kind,
					"name": workloadName,
				},
				"metricsSource": map[string]interface{}{
					"prometheus": map[string]interface{}{
						"address": promAddr,
					},
				},
				"cpu": map[string]interface{}{
					"percentile":   int64(cpuPercentile),
					"safetyMargin": "1.2",
				},
				"memory": map[string]interface{}{
					"percentile":   int64(memPercentile),
					"safetyMargin": "1.3",
				},
				"updateStrategy": map[string]interface{}{
					"mode": mode,
				},
			},
		},
	}
	return obj
}

func marshalPolicyYAML(obj *unstructured.Unstructured) ([]byte, error) {
	return sigsyaml.Marshal(obj.Object)
}
