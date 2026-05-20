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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	sigsyaml "sigs.k8s.io/yaml"
)

var version = "dev"

const structuredOutputUsage = "Output raw RightSizePolicy objects as json or yaml (not command-specific)"

var gvr = schema.GroupVersionResource{
	Group:    "rightsize.io",
	Version:  "v1alpha1",
	Resource: "rightsizepolicies",
}

func main() {
	fs := flag.NewFlagSet("kubectl-rightsize", flag.ExitOnError)
	namespace := fs.String("n", "", "Namespace (defaults to current context namespace)")
	fs.StringVar(namespace, "namespace", "", "Namespace (defaults to current context namespace)")
	allNamespaces := fs.Bool("A", false, "List across all namespaces")
	fs.BoolVar(allNamespaces, "all-namespaces", false, "List across all namespaces")
	kubeconfig := fs.String("kubeconfig", "", "Path to kubeconfig file")
	output := fs.String("o", "", structuredOutputUsage)
	fs.StringVar(output, "output", "", structuredOutputUsage)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kubectl rightsize <command> [flags]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  status            Show policy status, workload counts, and conditions")
		fmt.Fprintln(os.Stderr, "  savings           Show estimated CPU/memory savings per policy")
		fmt.Fprintln(os.Stderr, "  recommendations   Show per-container sizing recommendations")
		fmt.Fprintln(os.Stderr, "  explain           Show recommendation reasoning for one policy")
		fmt.Fprintln(os.Stderr, "  history           Show resize history (including eviction fallbacks)")
		fmt.Fprintln(os.Stderr, "  version           Print plugin version")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Structured output note:")
		fmt.Fprintln(os.Stderr, "  -o json|yaml always prints raw RightSizePolicy objects returned by the cluster.")
		fmt.Fprintln(os.Stderr, "  It is not command-specific output for status, savings, recommendations, or history.")
	}

	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	if cmd == "--help" || cmd == "-h" || cmd == "help" {
		fs.Usage()
		return
	}
	if cmd == "version" {
		fmt.Printf("kubectl-rightsize %s\n", version)
		return
	}

	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
	}
	if isZeroArgCommand(cmd) {
		if err := zeroArgCommandArgs(cmd, fs.Args()); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Build client from kubeconfig.
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *kubeconfig != "" {
		loadingRules.ExplicitPath = *kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	if *namespace == "" && !*allNamespaces {
		ns, _, err := kubeConfig.Namespace()
		if err != nil || ns == "" {
			ns = "default"
		}
		*namespace = ns
	}
	if *allNamespaces {
		*namespace = ""
	}

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// For structured output, fetch and print all policies as JSON/YAML.
	if *output == "json" || *output == "yaml" {
		printStructured(ctx, dynClient, *namespace, *output)
		return
	}

	switch cmd {
	case "status":
		printStatus(ctx, dynClient, *namespace)
	case "savings":
		printSavings(ctx, dynClient, *namespace)
	case "recommendations":
		printRecommendations(ctx, dynClient, *namespace)
	case "explain":
		policyName, err := explainPolicyName(fs.Args())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		printExplain(ctx, dynClient, *namespace, policyName)
	case "history":
		printHistory(ctx, dynClient, *namespace)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		fs.Usage()
		os.Exit(1)
	}
}

func isZeroArgCommand(cmd string) bool {
	switch cmd {
	case "status", "savings", "recommendations", "history":
		return true
	default:
		return false
	}
}

func zeroArgCommandArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s accepts no positional arguments. Remove %q", cmd, args[0])
}

func explainPolicyName(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", fmt.Errorf("explain requires a policy name")
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("explain accepts exactly one policy name. Put flags before the policy name, for example: kubectl rightsize explain -n production %s", args[0])
	}
}

func fetchPolicies(ctx context.Context, dynClient dynamic.Interface, namespace string) *unstructured.UnstructuredList {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing policies: %v\n", err)
		os.Exit(1)
	}
	return list
}

func printStatus(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list := fetchPolicies(ctx, dynClient, namespace)

	if len(list.Items) == 0 {
		fmt.Println("No RightSizePolicies found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tMODE\tWORKLOADS\tPENDING\tRESIZED\tREADY\tRESIZING\tDEGRADED\tAGE")

	for _, item := range list.Items {
		ns := item.GetNamespace()
		name := item.GetName()
		mode := getNestedString(item, "spec", "updateStrategy", "mode")
		workloads := getNestedInt64(item, "status", "workloads", "discovered")
		pending := getNestedInt64(item, "status", "workloads", "pending")
		resized := getNestedInt64(item, "status", "workloads", "resized")
		ready := policyReadyReason(item)
		resizing := getConditionReason(item, "Resizing")
		degraded := getConditionReason(item, "Degraded")
		age := formatAge(item.GetCreationTimestamp().Time)

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			ns, name, mode, workloads, pending, resized, ready, resizing, degraded, age)
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
}

func printSavings(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list := fetchPolicies(ctx, dynClient, namespace)

	if len(list.Items) == 0 {
		fmt.Println("No RightSizePolicies found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tCPU SAVED\tMEMORY SAVED\t% SAVED\tEST. MONTHLY")

	for _, item := range list.Items {
		ns := item.GetNamespace()
		name := item.GetName()
		cpuSaved := getNestedString(item, "status", "savings", "cpuRequestReduction")
		cpuTotal := getNestedString(item, "status", "savings", "cpuRequestTotal")
		memSaved := getNestedString(item, "status", "savings", "memoryRequestReduction")
		estMonthly := getNestedString(item, "status", "savings", "estimatedMonthlySavings")

		if cpuSaved == "" {
			cpuSaved = "-"
		}
		memSaved = formatMemory(memSaved)
		if memSaved == "" {
			memSaved = "-"
		}
		if estMonthly == "" {
			estMonthly = "-"
		}
		pctSaved := savingsPercent(cpuSaved, cpuTotal)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ns, name, cpuSaved, memSaved, pctSaved, estMonthly)
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
}

func getNestedString(obj unstructured.Unstructured, fields ...string) string {
	val, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return val
}

func getNestedInt64(obj unstructured.Unstructured, fields ...string) int64 {
	val, found, err := unstructured.NestedInt64(obj.Object, fields...)
	if err != nil || !found {
		return 0
	}
	return val
}

// getConditionReason returns "Status/Reason" for the named condition, or "-".
func getConditionReason(obj unstructured.Unstructured, conditionType string) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "-"
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		ct, _ := cond["type"].(string)
		if ct == conditionType {
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)
			if reason != "" {
				return reason
			}
			return status
		}
	}

	return "-"
}

func printRecommendations(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list := fetchPolicies(ctx, dynClient, namespace)

	if len(list.Items) == 0 {
		fmt.Println("No RightSizePolicies found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tPOLICY\tWORKLOAD\tCONTAINER\tCPU REQ\tCPU REC\tMEM REQ\tMEM REC\tCONFIDENCE / STATUS")

	var collecting int
	for _, item := range list.Items {
		ns := item.GetNamespace()
		policyName := item.GetName()
		recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
		if !found || len(recs) == 0 {
			collecting++
			fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t-\t-\t%s\n",
				ns, policyName, policyReadyReason(item))
			continue
		}

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
				confidence, _ := cont["confidence"].(float64)

				current, _ := cont["current"].(map[string]interface{})
				recommended, _ := cont["recommended"].(map[string]interface{})

				curCPU, _ := current["cpuRequest"].(string)
				recCPU, _ := recommended["cpuRequest"].(string)
				curMem, _ := current["memoryRequest"].(string)
				recMem, _ := recommended["memoryRequest"].(string)

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\n",
					ns, policyName, workload, name, curCPU, recCPU, curMem, recMem, confidence*100)
			}
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
	if collecting > 0 {
		noun := "policies"
		if collecting == 1 {
			noun = "policy"
		}
		fmt.Fprintf(os.Stderr, "\n%d %s collecting data. Run 'kubectl rightsize status' for details.\n",
			collecting, noun)
	}
}

func printExplain(ctx context.Context, dynClient dynamic.Interface, namespace, policyName string) {
	if namespace == "" {
		fmt.Fprintln(os.Stderr, "Error: explain requires a single namespace. Use -n or --namespace.")
		os.Exit(1)
	}

	item, err := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching policy %s/%s: %v\n", namespace, policyName, err)
		os.Exit(1)
	}

	recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
	if !found || len(recs) == 0 {
		fmt.Printf("%s/%s has no recommendations yet (%s).\n", namespace, policyName, policyReadyReason(*item))
		return
	}

	fmt.Printf("Policy: %s/%s\n", namespace, policyName)
	for _, r := range recs {
		rec, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		workload, _ := rec["workload"].(string)
		fmt.Printf("\nWorkload: %s\n", workload)
		containers, _ := rec["containers"].([]interface{})
		for _, c := range containers {
			cont, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := cont["name"].(string)
			confidence, _ := cont["confidence"].(float64)
			current, _ := cont["current"].(map[string]interface{})
			recommended, _ := cont["recommended"].(map[string]interface{})
			explanation, _ := cont["explanation"].(map[string]interface{})

			fmt.Printf("  Container: %s\n", name)
			fmt.Printf("    Confidence: %.1f%%\n", confidence*100)
			printResourceExplanation("CPU", current, recommended, explanation)
			printResourceExplanation("Memory", current, recommended, explanation)
		}
	}
}

func printResourceExplanation(resourceName string, current, recommended, explanation map[string]interface{}) {
	key := "cpu"
	currentField := "cpuRequest"
	recommendedField := "cpuRequest"
	if resourceName == "Memory" {
		key = "memory"
		currentField = "memoryRequest"
		recommendedField = "memoryRequest"
	}
	resourceExplanation, _ := explanation[key].(map[string]interface{})
	if len(resourceExplanation) == 0 {
		return
	}
	currentValue, _ := current[currentField].(string)
	finalValue, _ := recommended[recommendedField].(string)

	fmt.Printf("    %s:\n", resourceName)
	fmt.Printf("      Raw percentile:              %s\n", nestedString(resourceExplanation, "rawPercentile"))
	fmt.Printf("      x Safety margin (%s):      %s\n",
		formatFloat(nestedFloat(resourceExplanation, "safetyMargin")),
		nestedString(resourceExplanation, "afterSafetyMargin"))
	fmt.Printf("      x Confidence factor (%s, confidence %.2f): %s\n",
		formatFloat(nestedFloat(resourceExplanation, "confidenceFactor")),
		nestedFloat(resourceExplanation, "confidence"),
		nestedString(resourceExplanation, "afterConfidence"))
	fmt.Printf("      Bounds [%s, %s]:         %s%s\n",
		nestedStringMap(resourceExplanation, "bounds", "min"),
		nestedStringMap(resourceExplanation, "bounds", "max"),
		nestedString(resourceExplanation, "afterBounds"),
		formatAppliedSuffix(nestedString(resourceExplanation, "boundsApplied")))
	fmt.Printf("      Change filter [%s%%, %s%%]: %s%s\n",
		formatFloat(nestedFloat(resourceExplanation, "minChangePercent")),
		formatFloat(nestedFloat(resourceExplanation, "maxChangePercent")),
		nestedString(resourceExplanation, "afterChangeFilter"),
		formatAppliedSuffix(nestedString(resourceExplanation, "changeFilterApplied")))
	fmt.Printf("      Final recommendation:       %s (vs current %s%s)\n",
		finalValue, currentValue, formatDeltaSuffix(currentValue, finalValue))
	if adjustment := nestedString(resourceExplanation, "finalAdjustment"); adjustment != "" {
		fmt.Printf("      Final adjustment:           %s\n", adjustment)
	}
}

// getConditionMessage returns the message for the named condition, or "".
func getConditionMessage(obj unstructured.Unstructured, conditionType string) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == conditionType {
			msg, _ := cond["message"].(string)
			return msg
		}
	}
	return ""
}

// savingsPercent computes the CPU savings percentage from reduction and total strings.
func savingsPercent(saved, total string) string {
	if saved == "-" || saved == "" || total == "" {
		return "-"
	}
	s, err := resource.ParseQuantity(saved)
	if err != nil {
		return "-"
	}
	t, err := resource.ParseQuantity(total)
	if err != nil || t.MilliValue() == 0 {
		return "-"
	}
	pct := float64(s.MilliValue()) * 100.0 / float64(t.MilliValue())
	return fmt.Sprintf("%.0f%%", pct)
}

// policyReadyReason extracts the Ready condition reason from an unstructured policy.
func policyReadyReason(item unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == "Ready" {
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)
			msg, _ := cond["message"].(string)
			if status == "False" && msg != "" {
				return msg
			}
			if reason != "" {
				return reason
			}
			if status != "" {
				return status
			}
		}
	}
	return "Pending"
}

func formatMemory(s string) string {
	if s == "" || s == "-" {
		return s
	}
	bytes, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0fKi", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

func nestedString(obj map[string]interface{}, field string) string {
	value, _ := obj[field].(string)
	return value
}

func nestedFloat(obj map[string]interface{}, field string) float64 {
	value, _ := obj[field].(float64)
	return value
}

func nestedStringMap(obj map[string]interface{}, field, nested string) string {
	child, _ := obj[field].(map[string]interface{})
	if child == nil {
		return ""
	}
	value, _ := child[nested].(string)
	return value
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

func formatAppliedSuffix(value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf(" (%s)", value)
}

func formatDeltaSuffix(currentValue, finalValue string) string {
	currentQty, err := resource.ParseQuantity(currentValue)
	if err != nil {
		return ""
	}
	finalQty, err := resource.ParseQuantity(finalValue)
	if err != nil {
		return ""
	}
	currentMilli := currentQty.MilliValue()
	if currentMilli == 0 {
		return ""
	}
	deltaPct := (float64(finalQty.MilliValue()) - float64(currentMilli)) * 100 / float64(currentMilli)
	return fmt.Sprintf(", %+0.0f%%", deltaPct)
}

func printStructured(ctx context.Context, dynClient dynamic.Interface, namespace, format string) {
	list := fetchPolicies(ctx, dynClient, namespace)

	switch format {
	case "json":
		data, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	case "yaml":
		data, err := json.Marshal(list)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling: %v\n", err)
			os.Exit(1)
		}
		yamlData, err := sigsyaml.JSONToYAML(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error converting to YAML: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(yamlData))
	}
}

func formatAge(created time.Time) string {
	dur := time.Since(created)
	switch {
	case dur < time.Minute:
		return fmt.Sprintf("%ds", int(dur.Seconds()))
	case dur < time.Hour:
		return fmt.Sprintf("%dm", int(dur.Minutes()))
	case dur < 24*time.Hour:
		return fmt.Sprintf("%dh", int(dur.Hours()))
	default:
		return fmt.Sprintf("%dd", int(dur.Hours()/24))
	}
}

func printHistory(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list := fetchPolicies(ctx, dynClient, namespace)

	if len(list.Items) == 0 {
		fmt.Println("No RightSizePolicies found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tPOLICY\tTIMESTAMP\tWORKLOAD\tCONTAINER\tRESOURCE\tFROM\tTO\tMETHOD\tRESULT")

	var hasEntries bool
	for _, item := range list.Items {
		ns := item.GetNamespace()
		policyName := item.GetName()
		history, found, _ := unstructured.NestedSlice(item.Object, "status", "resizeHistory")
		if !found {
			continue
		}

		for _, h := range history {
			entry, ok := h.(map[string]interface{})
			if !ok {
				continue
			}
			ts, _ := entry["timestamp"].(string)
			workload, _ := entry["workload"].(string)
			container, _ := entry["container"].(string)
			resource, _ := entry["resource"].(string)
			from, _ := entry["from"].(string)
			to, _ := entry["to"].(string)
			method, _ := entry["method"].(string)
			result, _ := entry["result"].(string)
			if method == "" {
				if result == "Evicted" {
					method = "Eviction"
				} else {
					method = "InPlace"
				}
			}

			if t, parseErr := time.Parse(time.RFC3339, ts); parseErr == nil {
				ts = t.Local().Format("Jan 02 15:04")
			}

			hasEntries = true
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				ns, policyName, ts, workload, container, resource, from, to, method, result)
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
	if !hasEntries {
		fmt.Fprintf(os.Stderr, "\nNo resize history found. In-place resizes and eviction fallbacks are recorded in Canary, OneShot, and Auto modes.\n")
	}
}
