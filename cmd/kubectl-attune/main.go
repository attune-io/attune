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
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	sigsyaml "sigs.k8s.io/yaml"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

var version = "dev"

const (
	structuredOutputUsage = "Output raw AttunePolicy objects as json or yaml (status only)"
	sourcePolicy          = "policy"
	sourceNamespace       = "namespace default"
	sourceCluster         = "cluster default"
	sourceBuiltIn         = "built-in default"
	unsetValue            = "<unset>"
)

var gvr = schema.GroupVersionResource{
	Group:    "attune.io",
	Version:  "v1alpha1",
	Resource: "attunepolicies",
}

var defaultsGVR = schema.GroupVersionResource{
	Group:    "attune.io",
	Version:  "v1alpha1",
	Resource: "attunedefaults",
}

var namespaceDefaultsGVR = schema.GroupVersionResource{
	Group:    "attune.io",
	Version:  "v1alpha1",
	Resource: "attunenamespacedefaults",
}

type selectedDefaults struct {
	defaults *attunev1alpha1.AttuneDefaults
	source   string
}

type dynamicClientFactory func(kubeconfigPath, context string) (dynamic.Interface, string, error)

func main() {
	os.Exit(run(os.Args[1:], buildDynamicClient))
}

func run(args []string, buildClient dynamicClientFactory) int {
	fs := flag.NewFlagSet("kubectl-attune", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("n", "", "Namespace (defaults to current context namespace)")
	fs.StringVar(namespace, "namespace", "", "Namespace (defaults to current context namespace)")
	allNamespaces := fs.Bool("A", false, "List across all namespaces")
	fs.BoolVar(allNamespaces, "all-namespaces", false, "List across all namespaces")
	kubeconfig := fs.String("kubeconfig", "", "Path to kubeconfig file")
	output := fs.String("o", "", structuredOutputUsage)
	fs.StringVar(output, "output", "", structuredOutputUsage)
	watch := fs.Bool("w", false, "Watch mode: refresh status every 10 seconds (status command only)")
	fs.BoolVar(watch, "watch", false, "Watch mode: refresh status every 10 seconds (status command only)")
	sortBy := fs.String("sort-by", "", "Sort output: name, namespace, savings, age (status/savings commands)")
	filter := fs.String("filter", "", "Filter policies by Ready condition reason: degraded, pending, collecting, ready, noworkloads (status command)")
	contexts := fs.String("contexts", "", "Comma-separated kubeconfig contexts to query (multi-cluster)")
	allContexts := fs.Bool("all-contexts", false, "Query all contexts in kubeconfig (multi-cluster)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kubectl attune <command> [flags]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  status            Show policy status, workload counts, and conditions")
		fmt.Fprintln(os.Stderr, "  savings           Show estimated CPU/memory savings per policy")
		fmt.Fprintln(os.Stderr, "  recommendations   Show per-container sizing recommendations")
		fmt.Fprintln(os.Stderr, "  explain           Show recommendation reasoning for one policy")
		fmt.Fprintln(os.Stderr, "  preview           Preview per-pod resource changes before promoting type")
		fmt.Fprintln(os.Stderr, "  history           Show resize history (including eviction fallbacks)")
		fmt.Fprintln(os.Stderr, "  diff              Show resource change diffs from recommendations")
		fmt.Fprintln(os.Stderr, "  wizard            Interactive policy creation and type promotion")
		fmt.Fprintln(os.Stderr, "  version           Print plugin version")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Structured output note:")
		fmt.Fprintln(os.Stderr, "  -o json|yaml is supported with status. -o yaml is supported with diff.")
		fmt.Fprintln(os.Stderr, "  For raw AttunePolicy objects with other commands, use kubectl get attunepolicy -o json|yaml.")
	}

	if len(args) == 0 {
		fs.Usage()
		return 1
	}

	remainingArgs := args[1:]
	cmd := args[0]
	if cmd == "--help" || cmd == "-h" || cmd == "help" {
		fs.Usage()
		return 0
	}
	if cmd == "version" {
		fmt.Printf("kubectl-attune %s\n", version)
		return 0
	}
	if !isKnownCommand(cmd) {
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		fs.Usage()
		return 1
	}

	if err := fs.Parse(remainingArgs); err != nil {
		return 2
	}
	parsedArgs := fs.Args()
	if isZeroArgCommand(cmd) {
		if err := zeroArgCommandArgs(cmd, parsedArgs); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	}
	if err := structuredOutputCommandError(cmd, *output); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if *watch && cmd != "status" {
		fmt.Fprintf(os.Stderr, "Error: --watch is supported only with the status command\n")
		return 1
	}
	if *filter != "" && cmd != "status" {
		fmt.Fprintf(os.Stderr, "Error: --filter is supported only with the status command\n")
		return 1
	}
	if *sortBy != "" && cmd != "status" && cmd != "savings" {
		fmt.Fprintf(os.Stderr, "Error: --sort-by is supported only with the status and savings commands\n")
		return 1
	}

	isMultiCtx := *allContexts || *contexts != ""
	if isMultiCtx {
		if cmd == "explain" || cmd == "preview" || cmd == "wizard" {
			fmt.Fprintf(os.Stderr, "Error: %s requires a single cluster context; remove --contexts/--all-contexts\n", cmd)
			return 1
		}
		if *output != "" {
			fmt.Fprintf(os.Stderr, "Error: -o %s is not supported with --contexts or --all-contexts\n", *output)
			return 1
		}
		if *watch {
			fmt.Fprintf(os.Stderr, "Error: --watch is not supported with --contexts or --all-contexts\n")
			return 1
		}
	}

	policyName := ""
	if cmd == "explain" || cmd == "preview" {
		name, err := policyNameArg(cmd, parsedArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		policyName = name
	}

	ctx := context.Background()

	// Multi-cluster mode: fetch from all requested contexts and render.
	if isMultiCtx {
		ctxList, err := resolveContexts(*kubeconfig, *contexts, *allContexts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		items, warnings := fetchMultiCluster(ctx, *kubeconfig, ctxList, *namespace, *allNamespaces, buildClient)
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
		}
		switch cmd {
		case "status":
			printStatusItems(items, *sortBy, *filter)
		case "savings":
			printSavingsItems(items, *sortBy)
		case "recommendations":
			printRecommendationsItems(items)
		case "history":
			printHistoryItems(items)
		case "diff":
			printDiffItems(items, "")
		}
		return 0
	}

	dynClient, currentNamespace, err := buildClient(*kubeconfig, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if *namespace == "" && !*allNamespaces {
		if currentNamespace == "" {
			currentNamespace = "default"
		}
		*namespace = currentNamespace
	}
	if *allNamespaces {
		*namespace = ""
	}
	if (*output == "json" || *output == "yaml") && cmd != "diff" {
		printStructured(ctx, dynClient, *namespace, *output)
		return 0
	}

	switch cmd {
	case "status":
		if *watch {
			watchStatus(ctx, dynClient, *namespace, *sortBy, *filter)
		} else {
			printStatus(ctx, dynClient, *namespace, *sortBy, *filter)
		}
	case "savings":
		printSavings(ctx, dynClient, *namespace, *sortBy)
	case "recommendations":
		printRecommendations(ctx, dynClient, *namespace)
	case "explain":
		printExplain(ctx, dynClient, *namespace, policyName)
	case "history":
		printHistory(ctx, dynClient, *namespace)
	case "preview":
		printPreview(ctx, dynClient, *namespace, policyName)
	case "diff":
		printDiff(ctx, dynClient, *namespace, *output)
	case "wizard":
		return runWizard(ctx, dynClient, *namespace, parsedArgs, newInteractivePrompter())
	}
	return 0
}

func buildDynamicClient(kubeconfigPath, contextOverride string) (dynamic.Interface, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextOverride != "" {
		overrides.CurrentContext = contextOverride
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	currentNamespace, _, err := kubeConfig.Namespace()
	if err != nil || currentNamespace == "" {
		currentNamespace = "default"
	}
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, "", err
	}
	return dynClient, currentNamespace, nil
}

func isKnownCommand(cmd string) bool {
	switch cmd {
	case "status", "savings", "recommendations", "explain", "history", "preview", "version", "wizard", "diff":
		return true
	default:
		return false
	}
}

func isZeroArgCommand(cmd string) bool {
	switch cmd {
	case "status", "savings", "recommendations", "history", "diff":
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
	return policyNameArg("explain", args)
}

func policyNameArg(cmd string, args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", fmt.Errorf("%s requires a policy name", cmd)
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("%s accepts exactly one policy name. Put flags before the policy name, for example: kubectl attune %s -n production %s", cmd, cmd, args[0])
	}
}

func structuredOutputCommandError(cmd, output string) error {
	if output == "" {
		return nil
	}
	if cmd == "diff" {
		if output == "yaml" {
			return nil
		}
		return fmt.Errorf("-o %s is not supported with diff; use -o yaml for patch manifests", output)
	}
	if output != "json" && output != "yaml" {
		return fmt.Errorf("unsupported output format %q, use json or yaml", output)
	}
	if cmd == "status" {
		return nil
	}
	return fmt.Errorf("-o %s is supported only with the status command. For raw AttunePolicy objects, use kubectl get attunepolicy -o %s", output, output)
}

func fetchPolicies(ctx context.Context, dynClient dynamic.Interface, namespace string) *unstructured.UnstructuredList {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || isNoResourceMatch(err) {
			fmt.Fprintln(os.Stderr, "Error: Attune CRDs are not installed in this cluster.")
			fmt.Fprintln(os.Stderr, "Install the operator first:")
			fmt.Fprintln(os.Stderr, "  helm install attune oci://ghcr.io/attune-io/charts/attune")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error listing policies: %v\n", err)
		os.Exit(1)
	}
	return list
}

// isNoResourceMatch checks if the error indicates the CRD/resource type
// does not exist on the API server (common when the operator isn't installed).
func isNoResourceMatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "the server could not find the requested resource")
}

func printStatus(ctx context.Context, dynClient dynamic.Interface, namespace, sortByFlag, filterFlag string) {
	list := fetchPolicies(ctx, dynClient, namespace)
	printStatusItems(list.Items, sortByFlag, filterFlag)
}

func printStatusItems(allItems []unstructured.Unstructured, sortByFlag, filterFlag string) {
	if len(allItems) == 0 {
		fmt.Println("No AttunePolicies found.")
		return
	}

	items := filterPolicies(allItems, filterFlag)
	if len(items) == 0 {
		fmt.Println("No AttunePolicies match the filter.")
		return
	}
	sortPolicies(items, sortByFlag)

	showCluster := hasClusterAnnotation(items)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	if showCluster {
		fmt.Fprintln(w, "CLUSTER\tNAMESPACE\tNAME\tTYPE\tWORKLOADS\tPENDING\tRESIZED\tREADY\tRESIZING\tDEGRADED\tSCHEDULE\tCANARY\tAGE")
	} else {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tTYPE\tWORKLOADS\tPENDING\tRESIZED\tREADY\tRESIZING\tDEGRADED\tSCHEDULE\tCANARY\tAGE")
	}

	for _, item := range items {
		ns := item.GetNamespace()
		name := item.GetName()
		mode := getNestedString(item, "spec", "updateStrategy", "type")
		workloads := getNestedInt64(item, "status", "workloads", "discovered")
		pending := getNestedInt64(item, "status", "workloads", "pending")
		resized := getNestedInt64(item, "status", "workloads", "resized")
		ready := policyReadyReason(item)
		resizing := getConditionReason(item, "Resizing")
		degraded := getConditionReason(item, "Degraded")
		schedule := getConditionReason(item, "ScheduleBlocked")
		canary := formatCanaryStatus(item)
		age := formatAge(item.GetCreationTimestamp().Time)

		if showCluster {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				itemCluster(item), ns, name, mode, workloads, pending, resized, ready, resizing, degraded, schedule, canary, age)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				ns, name, mode, workloads, pending, resized, ready, resizing, degraded, schedule, canary, age)
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}

	// Show per-workload errors when present.
	for _, item := range allItems {
		errors, found, _ := unstructured.NestedSlice(item.Object, "status", "workloadErrors")
		if !found || len(errors) == 0 {
			continue
		}
		prefix := ""
		if showCluster {
			prefix = fmt.Sprintf("[%s] ", itemCluster(item))
		}
		fmt.Fprintf(os.Stderr, "\n%sWorkload errors for %s/%s:\n", prefix, item.GetNamespace(), item.GetName())
		for _, e := range errors {
			entry, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			wl, _ := entry["workload"].(string)
			msg, _ := entry["error"].(string)
			fmt.Fprintf(os.Stderr, "  %s: %s\n", wl, msg)
		}
	}
}

func watchStatus(ctx context.Context, dynClient dynamic.Interface, namespace, sortByFlag, filterFlag string) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	for {
		// Clear screen and move cursor to top-left.
		fmt.Print("\033[2J\033[H")
		printStatus(ctx, dynClient, namespace, sortByFlag, filterFlag)
		fmt.Printf("\nLast refresh: %s  (Ctrl+C to stop)\n", time.Now().Format("15:04:05"))

		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func printSavings(ctx context.Context, dynClient dynamic.Interface, namespace, sortByFlag string) {
	list := fetchPolicies(ctx, dynClient, namespace)
	printSavingsItems(list.Items, sortByFlag)
}

func printSavingsItems(items []unstructured.Unstructured, sortByFlag string) {
	if len(items) == 0 {
		fmt.Println("No AttunePolicies found.")
		return
	}

	sortPolicies(items, sortByFlag)
	showCluster := hasClusterAnnotation(items)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	if showCluster {
		fmt.Fprintln(w, "CLUSTER\tNAMESPACE\tNAME\tCPU SAVED\tMEMORY SAVED\t% SAVED\tEST. MONTHLY")
	} else {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tCPU SAVED\tMEMORY SAVED\t% SAVED\tEST. MONTHLY")
	}

	var totalCPUMillis, totalCPUTotalMillis, totalMemBytes int64
	var totalMonthlyCents int64
	hasTotals := false

	for _, item := range items {
		ns := item.GetNamespace()
		name := item.GetName()
		cpuSaved := getNestedString(item, "status", "savings", "cpuRequestReduction")
		cpuTotal := getNestedString(item, "status", "savings", "cpuRequestTotal")
		memSaved := getNestedString(item, "status", "savings", "memoryRequestReduction")
		estMonthly := getNestedString(item, "status", "savings", "estimatedMonthlySavings")

		// Accumulate totals from raw values before formatting.
		if q, err := resource.ParseQuantity(cpuSaved); err == nil {
			totalCPUMillis += q.MilliValue()
			hasTotals = true
		}
		if q, err := resource.ParseQuantity(cpuTotal); err == nil {
			totalCPUTotalMillis += q.MilliValue()
		}
		if q, err := resource.ParseQuantity(memSaved); err == nil {
			totalMemBytes += q.Value()
		}
		totalMonthlyCents += parseDollarCents(estMonthly)

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

		if showCluster {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				itemCluster(item), ns, name, cpuSaved, memSaved, pctSaved, estMonthly)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				ns, name, cpuSaved, memSaved, pctSaved, estMonthly)
		}
	}

	// Print totals row when at least one policy has savings data.
	if hasTotals {
		totalCPU := resource.NewMilliQuantity(totalCPUMillis, resource.DecimalSI).String()
		totalMem := formatMemory(fmt.Sprintf("%d", totalMemBytes))
		if totalMem == "" {
			totalMem = "-"
		}
		totalPct := "-"
		if totalCPUTotalMillis > 0 {
			totalPct = fmt.Sprintf("%.0f%%", float64(totalCPUMillis)*100.0/float64(totalCPUTotalMillis))
		}
		totalMonthly := "-"
		if totalMonthlyCents > 0 {
			totalMonthly = fmt.Sprintf("$%.2f", float64(totalMonthlyCents)/100.0)
		}
		ec := ""
		if showCluster {
			ec = "\t"
		}
		fmt.Fprintln(w, ec+"\t\t\t\t\t")
		fmt.Fprintf(w, "%s\tTOTAL\t%s\t%s\t%s\t%s\n",
			ec, totalCPU, totalMem, totalPct, totalMonthly)
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
	printRecommendationsItems(list.Items)
}

func printRecommendationsItems(items []unstructured.Unstructured) {
	if len(items) == 0 {
		fmt.Println("No AttunePolicies found.")
		return
	}

	showCluster := hasClusterAnnotation(items)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	if showCluster {
		fmt.Fprintln(w, "CLUSTER\tNAMESPACE\tPOLICY\tWORKLOAD\tCONTAINER\tCPU REQ\tCPU REC\tMEM REQ\tMEM REC\tCONFIDENCE / STATUS")
	} else {
		fmt.Fprintln(w, "NAMESPACE\tPOLICY\tWORKLOAD\tCONTAINER\tCPU REQ\tCPU REC\tMEM REQ\tMEM REC\tCONFIDENCE / STATUS")
	}

	var collecting int
	for _, item := range items {
		ns := item.GetNamespace()
		policyName := item.GetName()
		cluster := itemCluster(item)
		recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
		if !found || len(recs) == 0 {
			collecting++
			if showCluster {
				fmt.Fprintf(w, "%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t%s\n",
					cluster, ns, policyName, policyReadyReason(item))
			} else {
				fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t-\t-\t%s\n",
					ns, policyName, policyReadyReason(item))
			}
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

				if showCluster {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\n",
						cluster, ns, policyName, workload, name, curCPU, recCPU, curMem, recMem, confidence*100)
				} else {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\n",
						ns, policyName, workload, name, curCPU, recCPU, curMem, recMem, confidence*100)
				}
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
		fmt.Fprintf(os.Stderr, "\n%d %s collecting data. Run 'kubectl attune status' for details.\n",
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

	selected, err := fetchSelectedDefaults(ctx, dynClient, namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving effective values for %s/%s: %v\n", namespace, policyName, err)
		os.Exit(1)
	}
	effective, err := resolveEffectivePolicy(item, selected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving effective values for %s/%s: %v\n", namespace, policyName, err)
		os.Exit(1)
	}

	recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
	if !found || len(recs) == 0 {
		fmt.Printf("%s/%s has no recommendations yet (%s).\n", namespace, policyName, policyReadyReason(*item))
		printEffectivePolicySummary(*item, effective, selected)
		return
	}

	fmt.Printf("Policy: %s/%s\n", namespace, policyName)
	printEffectivePolicySummary(*item, effective, selected)
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

func resolveEffectivePolicy(item *unstructured.Unstructured, selected selectedDefaults) (*attunev1alpha1.AttunePolicy, error) {
	effective := &attunev1alpha1.AttunePolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, effective); err != nil {
		return nil, err
	}
	mergeDefaultsIntoPolicy(effective, selected.defaults)
	applyBuiltInDefaults(effective)
	return effective, nil
}

func fetchSelectedDefaults(ctx context.Context, dynClient dynamic.Interface, namespace string) (selectedDefaults, error) {
	namespaceDefaults, err := fetchSingleDefaults(ctx, dynClient, namespaceDefaultsGVR, namespace)
	if err != nil {
		return selectedDefaults{}, fmt.Errorf("listing AttuneNamespaceDefaults in %s: %w", namespace, err)
	}
	if namespaceDefaults != nil {
		return selectedDefaults{defaults: namespaceDefaults, source: sourceNamespace}, nil
	}

	clusterDefaults, err := fetchSingleDefaults(ctx, dynClient, defaultsGVR, "")
	if err != nil {
		return selectedDefaults{}, fmt.Errorf("listing AttuneDefaults: %w", err)
	}
	if clusterDefaults != nil {
		return selectedDefaults{defaults: clusterDefaults, source: sourceCluster}, nil
	}
	return selectedDefaults{source: sourceBuiltIn}, nil
}

func fetchSingleDefaults(ctx context.Context, dynClient dynamic.Interface, resource schema.GroupVersionResource, namespace string) (*attunev1alpha1.AttuneDefaults, error) {
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if namespace != "" {
		list, err = dynClient.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = dynClient.Resource(resource).List(ctx, metav1.ListOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil
	}

	selected := &list.Items[0]
	for i := 1; i < len(list.Items); i++ {
		if list.Items[i].GetName() < selected.GetName() {
			selected = &list.Items[i]
		}
	}

	defaults := &attunev1alpha1.AttuneDefaults{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selected.Object, defaults); err != nil {
		return nil, err
	}
	return defaults, nil
}

func printEffectivePolicySummary(item unstructured.Unstructured, effective *attunev1alpha1.AttunePolicy, selected selectedDefaults) {
	if effective == nil {
		return
	}

	var metricsDefaults *attunev1alpha1.MetricsSource
	var updateDefaults *attunev1alpha1.UpdateStrategy
	if selected.defaults != nil {
		metricsDefaults = selected.defaults.Spec.MetricsSource
		updateDefaults = selected.defaults.Spec.UpdateStrategy
	}

	fmt.Println("Effective values:")
	printEffectiveField("Type", getNestedString(item, "spec", "updateStrategy", "type"), string(effective.Spec.UpdateStrategy.Type), selected, updateDefaults != nil && updateDefaults.Type != "")
	printEffectiveField("Cooldown", getNestedString(item, "spec", "updateStrategy", "cooldown"), formatDurationPtr(effective.Spec.UpdateStrategy.Cooldown), selected, updateDefaults != nil && updateDefaults.Cooldown != nil)
	printEffectiveField("Query step", getNestedString(item, "spec", "metricsSource", "queryStep"), formatDurationPtr(effective.Spec.MetricsSource.QueryStep), selected, metricsDefaults != nil && metricsDefaults.QueryStep != nil)
	printEffectiveField("Minimum data points", formatInt64Ptr(rawInt64Field(item, "spec", "metricsSource", "minimumDataPoints")), formatInt32Ptr(effective.Spec.MetricsSource.MinimumDataPoints), selected, metricsDefaults != nil && metricsDefaults.MinimumDataPoints != nil)
	printEffectiveField("Paused", formatBoolField(item, "spec", "paused"), formatBoolPtr(effective.Spec.Paused), selected, false)
	printEffectiveField("Resize method", getNestedString(item, "spec", "updateStrategy", "resizeMethod"), string(effective.Spec.UpdateStrategy.ResizeMethod), selected, updateDefaults != nil && updateDefaults.ResizeMethod != "")
	obsConfigured := getNestedString(item, "spec", "updateStrategy", "safetyObservationPeriod")
	if obsConfigured == "" {
		obsConfigured = getNestedString(item, "spec", "updateStrategy", "canary", "observationPeriod")
	}
	printEffectiveField("Observation period", obsConfigured, effectiveObservationPeriod(effective), selected, updateDefaults != nil && updateDefaults.SafetyObservationPeriod != nil)
	printEffectiveField("Auto revert", formatBoolField(item, "spec", "updateStrategy", "autoRevert"), formatBoolPtr(effective.Spec.UpdateStrategy.AutoRevert), selected, updateDefaults != nil && updateDefaults.AutoRevert != nil)
	printEffectiveField("Initial sizing", formatBoolField(item, "spec", "updateStrategy", "initialSizing"), formatBoolPtr(effective.Spec.UpdateStrategy.InitialSizing), selected, updateDefaults != nil && updateDefaults.InitialSizing != nil)
	printEffectiveField("Max concurrent resizes", formatInt64Field(item, "spec", "updateStrategy", "maxConcurrentResizes"), formatInt32Val(effective.Spec.UpdateStrategy.MaxConcurrentResizes), selected, updateDefaults != nil && updateDefaults.MaxConcurrentResizes != 0)
	printEffectiveField("Rate window", getNestedString(item, "spec", "metricsSource", "rateWindow"), formatDurationPtr(effective.Spec.MetricsSource.RateWindow), selected, metricsDefaults != nil && metricsDefaults.RateWindow != nil)

	var cpuDefaults, memDefaults *attunev1alpha1.ResourceConfig
	if selected.defaults != nil {
		cpuDefaults = selected.defaults.Spec.CPU
		memDefaults = selected.defaults.Spec.Memory
	}

	fmt.Println("  CPU:")
	printEffectiveField("  Percentile", formatInt64Field(item, "spec", "cpu", "percentile"), formatInt32Val(effective.Spec.CPU.Percentile), selected, cpuDefaults != nil && cpuDefaults.Percentile != 0)
	printEffectiveField("  Overhead", getNestedString(item, "spec", "cpu", "overhead"), effective.Spec.CPU.Overhead, selected, cpuDefaults != nil && cpuDefaults.Overhead != "")
	printEffectiveField("  Controlled values", getNestedString(item, "spec", "cpu", "controlledValues"), formatStringPtr(effective.Spec.CPU.ControlledValues), selected, cpuDefaults != nil && cpuDefaults.ControlledValues != nil)
	printEffectiveField("  Max change", formatPercentInt64Ptr(rawInt64Field(item, "spec", "cpu", "maxChangePercent")), formatPercentPtr(effective.Spec.CPU.MaxChangePercent), selected, cpuDefaults != nil && cpuDefaults.MaxChangePercent != nil)

	fmt.Println("  Memory:")
	printEffectiveField("  Percentile", formatInt64Field(item, "spec", "memory", "percentile"), formatInt32Val(effective.Spec.Memory.Percentile), selected, memDefaults != nil && memDefaults.Percentile != 0)
	printEffectiveField("  Overhead", getNestedString(item, "spec", "memory", "overhead"), effective.Spec.Memory.Overhead, selected, memDefaults != nil && memDefaults.Overhead != "")
	printEffectiveField("  Controlled values", getNestedString(item, "spec", "memory", "controlledValues"), formatStringPtr(effective.Spec.Memory.ControlledValues), selected, memDefaults != nil && memDefaults.ControlledValues != nil)
	printEffectiveField("  Max change", formatPercentInt64Ptr(rawInt64Field(item, "spec", "memory", "maxChangePercent")), formatPercentPtr(effective.Spec.Memory.MaxChangePercent), selected, memDefaults != nil && memDefaults.MaxChangePercent != nil)
}

func printEffectiveField(label, configured, effective string, selected selectedDefaults, inherited bool) {
	if effective == "" {
		return
	}

	source := sourcePolicy
	if configured == "" {
		configured = unsetValue
		source = effectiveSource(selected, inherited)
	}

	fmt.Printf("  %s: %s (source: %s, configured: %s)\n", label, effective, source, configured)
}

func effectiveSource(selected selectedDefaults, inherited bool) string {
	if inherited {
		return selected.source
	}
	return sourceBuiltIn
}

// effectiveObservationPeriod computes the observation period using the
// precedence: safetyObservationPeriod > canary.observationPeriod > 5m default.
func effectiveObservationPeriod(policy *attunev1alpha1.AttunePolicy) string {
	if policy.Spec.UpdateStrategy.SafetyObservationPeriod != nil && policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.SafetyObservationPeriod.Duration.String()
	}
	if policy.Spec.UpdateStrategy.Canary != nil && policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration > 0 {
		return policy.Spec.UpdateStrategy.Canary.ObservationPeriod.Duration.String()
	}
	return (5 * time.Minute).String()
}

func formatDurationPtr(value *metav1.Duration) string {
	if value == nil {
		return ""
	}
	return value.Duration.String()
}

func rawInt64Field(item unstructured.Unstructured, fields ...string) *int64 {
	value, found, err := unstructured.NestedInt64(item.Object, fields...)
	if err != nil || !found {
		return nil
	}
	return &value
}

func formatInt64Ptr(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func formatInt32Ptr(value *int32) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(int64(*value), 10)
}

func formatPercentPtr(value *int32) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d%%", *value)
}

func formatBoolPtr(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func formatBoolField(obj unstructured.Unstructured, fields ...string) string {
	val, found, err := unstructured.NestedBool(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return strconv.FormatBool(val)
}

func formatInt64Field(obj unstructured.Unstructured, fields ...string) string {
	val, found, err := unstructured.NestedInt64(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return strconv.FormatInt(val, 10)
}

func formatInt32Val(value int32) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

func formatStringPtr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func formatPercentInt64Ptr(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d%%", *value)
}

func applyBuiltInDefaults(policy *attunev1alpha1.AttunePolicy) {
	pkgdefaults.ApplyBuiltInDefaults(policy)
}

func mergeDefaultsIntoPolicy(policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) {
	pkgdefaults.MergeDefaults(policy, defaults)
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
	fmt.Printf("      + Overhead (%s%%):           %s\n",
		formatFloat(nestedFloat(resourceExplanation, "overhead")),
		nestedString(resourceExplanation, "afterOverhead"))
	if bf := nestedFloat(resourceExplanation, "burstFactor"); bf > 1.0 {
		fmt.Printf("      x Burst factor (%s):        %s\n",
			formatFloat(bf),
			nestedString(resourceExplanation, "afterBurst"))
	}
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
// parseDollarCents parses a dollar string like "$12.78" into cents (1278).
// Returns 0 for empty, dash, or unparseable values.
func parseDollarCents(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	s = strings.TrimPrefix(s, "$")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * 100)
}

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

// filterPolicies returns items matching the given filter keyword based on the
// Ready condition reason. Supported filters: degraded, pending, collecting,
// ready, noworkloads. Empty filter returns all items.
func filterPolicies(items []unstructured.Unstructured, filterFlag string) []unstructured.Unstructured {
	if filterFlag == "" {
		return items
	}
	f := strings.ToLower(filterFlag)
	var result []unstructured.Unstructured
	for _, item := range items {
		reason := strings.ToLower(policyReadyReason(item))
		degraded := strings.ToLower(getConditionReason(item, "Degraded"))
		var match bool
		switch f {
		case "degraded":
			match = degraded != "-" && degraded != ""
		case "pending":
			match = reason == "pending"
		case "collecting":
			match = strings.Contains(reason, "insufficientdata") || strings.Contains(reason, "collecting")
		case "ready":
			match = strings.Contains(reason, "monitoring") || strings.Contains(reason, "ready")
		case "noworkloads":
			match = strings.Contains(reason, "noworkloadsfound") || strings.Contains(reason, "no matching workloads")
		default:
			match = strings.Contains(reason, f)
		}
		if match {
			result = append(result, item)
		}
	}
	return result
}

// sortPolicies sorts items in place by the given key. Supported: name,
// namespace, savings, age. Empty or unrecognized values are no-ops (API order).
func sortPolicies(items []unstructured.Unstructured, sortByFlag string) {
	switch strings.ToLower(sortByFlag) {
	case "name":
		sort.Slice(items, func(i, j int) bool {
			return items[i].GetName() < items[j].GetName()
		})
	case "namespace":
		sort.Slice(items, func(i, j int) bool {
			if items[i].GetNamespace() == items[j].GetNamespace() {
				return items[i].GetName() < items[j].GetName()
			}
			return items[i].GetNamespace() < items[j].GetNamespace()
		})
	case "savings":
		sort.Slice(items, func(i, j int) bool {
			return parseDollarCents(getNestedString(items[i], "status", "savings", "estimatedMonthlySavings")) >
				parseDollarCents(getNestedString(items[j], "status", "savings", "estimatedMonthlySavings"))
		})
	case "age":
		sort.Slice(items, func(i, j int) bool {
			return items[i].GetCreationTimestamp().Time.Before(items[j].GetCreationTimestamp().Time)
		})
	}
}

// formatCanaryStatus returns a summary of the canary rollout state, or "-" if
// the policy is not in Canary mode or has no canary status.
func formatCanaryStatus(item unstructured.Unstructured) string {
	mode := getNestedString(item, "spec", "updateStrategy", "type")
	if mode != "Canary" {
		return "-"
	}
	phase := getNestedString(item, "status", "canary", "phase")
	if phase == "" {
		return "Pending"
	}
	pods, _, _ := unstructured.NestedStringSlice(item.Object, "status", "canary", "pods")
	if len(pods) > 0 {
		return fmt.Sprintf("%s (%d pods)", phase, len(pods))
	}
	return phase
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
	printHistoryItems(list.Items)
}

func printHistoryItems(items []unstructured.Unstructured) {
	if len(items) == 0 {
		fmt.Println("No AttunePolicies found.")
		return
	}

	showCluster := hasClusterAnnotation(items)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	if showCluster {
		fmt.Fprintln(w, "CLUSTER\tNAMESPACE\tPOLICY\tTIMESTAMP\tWORKLOAD\tCONTAINER\tRESOURCE\tFROM\tTO\tMETHOD\tRESULT\tREASON")
	} else {
		fmt.Fprintln(w, "NAMESPACE\tPOLICY\tTIMESTAMP\tWORKLOAD\tCONTAINER\tRESOURCE\tFROM\tTO\tMETHOD\tRESULT\tREASON")
	}

	var hasEntries bool
	for _, item := range items {
		ns := item.GetNamespace()
		policyName := item.GetName()
		cluster := itemCluster(item)
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
			histResource, _ := entry["resource"].(string)
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

			reason, _ := entry["reason"].(string)
			if reason == "" {
				reason = "-"
			}

			hasEntries = true
			if showCluster {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					cluster, ns, policyName, ts, workload, container, histResource, from, to, method, result, reason)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					ns, policyName, ts, workload, container, histResource, from, to, method, result, reason)
			}
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
	if !hasEntries {
		fmt.Fprintf(os.Stderr, "\nNo resize history found. In-place resizes and eviction fallbacks are recorded in Canary, OneShot, and Auto modes.\n")
	}
}

// printPreview shows per-workload, per-container resource changes that would
// occur if the policy's mode were promoted to Auto (or Canary).
func printPreview(ctx context.Context, dynClient dynamic.Interface, namespace, policyName string) {
	if namespace == "" {
		fmt.Fprintln(os.Stderr, "Error: preview requires a single namespace. Use -n or --namespace.")
		os.Exit(1)
	}

	item, err := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching policy %s/%s: %v\n", namespace, policyName, err)
		os.Exit(1)
	}

	mode := getNestedString(*item, "spec", "updateStrategy", "type")
	recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
	if !found || len(recs) == 0 {
		fmt.Printf("%s/%s has no recommendations yet (%s). Nothing to preview.\n",
			namespace, policyName, policyReadyReason(*item))
		return
	}

	fmt.Printf("Preview: %s/%s (current mode: %s)\n\n", namespace, policyName, mode)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tCONTAINER\tRESOURCE\tCURRENT\tRECOMMENDED\tCHANGE")

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
			current, _ := cont["current"].(map[string]interface{})
			recommended, _ := cont["recommended"].(map[string]interface{})

			curCPU, _ := current["cpuRequest"].(string)
			recCPU, _ := recommended["cpuRequest"].(string)
			curMem, _ := current["memoryRequest"].(string)
			recMem, _ := recommended["memoryRequest"].(string)

			cpuDelta := formatDeltaSuffix(curCPU, recCPU)
			memDelta := formatDeltaSuffix(curMem, recMem)

			fmt.Fprintf(w, "%s\t%s\tCPU\t%s\t%s\t%s\n",
				workload, name, curCPU, recCPU, strings.TrimPrefix(cpuDelta, ", "))
			fmt.Fprintf(w, "%s\t%s\tMemory\t%s\t%s\t%s\n",
				workload, name, curMem, recMem, strings.TrimPrefix(memDelta, ", "))
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}

	// Show canary pod count if policy is in Canary mode.
	canaryPct := getNestedString(*item, "spec", "updateStrategy", "canary", "percentage")
	if canaryPct != "" {
		fmt.Fprintf(os.Stderr, "\nCanary percentage: %s%% of eligible pods will be resized first.\n", canaryPct)
	}

	// Warn about workload errors.
	errors, found, _ := unstructured.NestedSlice(item.Object, "status", "workloadErrors")
	if found && len(errors) > 0 {
		fmt.Fprintf(os.Stderr, "\nWarning: %d workload(s) had errors during last reconcile. Run 'kubectl attune status' for details.\n", len(errors))
	}
}
