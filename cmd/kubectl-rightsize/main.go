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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	sigsyaml "sigs.k8s.io/yaml"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

var version = "dev"

const (
	structuredOutputUsage = "Output raw RightSizePolicy objects as json or yaml (status only)"
	sourcePolicy          = "policy"
	sourceNamespace       = "namespace default"
	sourceCluster         = "cluster default"
	sourceBuiltIn         = "built-in default"
	unsetValue            = "<unset>"
	defaultQueryStep      = 5 * time.Minute
)

var gvr = schema.GroupVersionResource{
	Group:    "rightsize.io",
	Version:  "v1alpha1",
	Resource: "rightsizepolicies",
}

var defaultsGVR = schema.GroupVersionResource{
	Group:    "rightsize.io",
	Version:  "v1alpha1",
	Resource: "rightsizedefaults",
}

var namespaceDefaultsGVR = schema.GroupVersionResource{
	Group:    "rightsize.io",
	Version:  "v1alpha1",
	Resource: "rightsizenamespacedefaults",
}

type selectedDefaults struct {
	defaults *rightsizev1alpha1.RightSizeDefaults
	source   string
}

type dynamicClientFactory func(kubeconfigPath string) (dynamic.Interface, string, error)

func main() {
	os.Exit(run(os.Args[1:], buildDynamicClient))
}

func run(args []string, buildClient dynamicClientFactory) int {
	fs := flag.NewFlagSet("kubectl-rightsize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
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
		fmt.Fprintln(os.Stderr, "  -o json|yaml is supported only with the status command.")
		fmt.Fprintln(os.Stderr, "  For raw RightSizePolicy objects with other commands, use kubectl get rightsizepolicy -o json|yaml.")
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
		fmt.Printf("kubectl-rightsize %s\n", version)
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

	policyName := ""
	if cmd == "explain" {
		name, err := explainPolicyName(parsedArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		policyName = name
	}

	dynClient, currentNamespace, err := buildClient(*kubeconfig)
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

	ctx := context.Background()
	if *output == "json" || *output == "yaml" {
		printStructured(ctx, dynClient, *namespace, *output)
		return 0
	}

	switch cmd {
	case "status":
		printStatus(ctx, dynClient, *namespace)
	case "savings":
		printSavings(ctx, dynClient, *namespace)
	case "recommendations":
		printRecommendations(ctx, dynClient, *namespace)
	case "explain":
		printExplain(ctx, dynClient, *namespace, policyName)
	case "history":
		printHistory(ctx, dynClient, *namespace)
	}
	return 0
}

func buildDynamicClient(kubeconfigPath string) (dynamic.Interface, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
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
	case "status", "savings", "recommendations", "explain", "history", "version":
		return true
	default:
		return false
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

func structuredOutputCommandError(cmd, output string) error {
	if output == "" {
		return nil
	}
	if output != "json" && output != "yaml" {
		return fmt.Errorf("unsupported output format %q, use json or yaml", output)
	}
	if cmd == "status" {
		return nil
	}
	return fmt.Errorf("-o %s is supported only with the status command. For raw RightSizePolicy objects, use kubectl get rightsizepolicy -o %s", output, output)
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
	fmt.Fprintln(w, "NAMESPACE\tNAME\tMODE\tWORKLOADS\tPENDING\tRESIZED\tREADY\tRESIZING\tDEGRADED\tSCHEDULE\tAGE")

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
		schedule := getConditionReason(item, "ScheduleBlocked")
		age := formatAge(item.GetCreationTimestamp().Time)

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			ns, name, mode, workloads, pending, resized, ready, resizing, degraded, schedule, age)
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

func resolveEffectivePolicy(item *unstructured.Unstructured, selected selectedDefaults) (*rightsizev1alpha1.RightSizePolicy, error) {
	effective := &rightsizev1alpha1.RightSizePolicy{}
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
		return selectedDefaults{}, fmt.Errorf("listing RightSizeNamespaceDefaults in %s: %w", namespace, err)
	}
	if namespaceDefaults != nil {
		return selectedDefaults{defaults: namespaceDefaults, source: sourceNamespace}, nil
	}

	clusterDefaults, err := fetchSingleDefaults(ctx, dynClient, defaultsGVR, "")
	if err != nil {
		return selectedDefaults{}, fmt.Errorf("listing RightSizeDefaults: %w", err)
	}
	if clusterDefaults != nil {
		return selectedDefaults{defaults: clusterDefaults, source: sourceCluster}, nil
	}
	return selectedDefaults{source: sourceBuiltIn}, nil
}

func fetchSingleDefaults(ctx context.Context, dynClient dynamic.Interface, resource schema.GroupVersionResource, namespace string) (*rightsizev1alpha1.RightSizeDefaults, error) {
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

	defaults := &rightsizev1alpha1.RightSizeDefaults{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selected.Object, defaults); err != nil {
		return nil, err
	}
	return defaults, nil
}

func printEffectivePolicySummary(item unstructured.Unstructured, effective *rightsizev1alpha1.RightSizePolicy, selected selectedDefaults) {
	if effective == nil {
		return
	}

	var metricsDefaults *rightsizev1alpha1.MetricsSource
	var updateDefaults *rightsizev1alpha1.UpdateStrategy
	if selected.defaults != nil {
		metricsDefaults = selected.defaults.Spec.MetricsSource
		updateDefaults = selected.defaults.Spec.UpdateStrategy
	}

	fmt.Println("Effective values:")
	printEffectiveField("Mode", getNestedString(item, "spec", "updateStrategy", "mode"), string(effective.Spec.UpdateStrategy.Mode), selected, updateDefaults != nil && updateDefaults.Mode != "")
	printEffectiveField("Cooldown", getNestedString(item, "spec", "updateStrategy", "cooldown"), formatDurationPtr(effective.Spec.UpdateStrategy.Cooldown), selected, updateDefaults != nil && updateDefaults.Cooldown != nil)
	printEffectiveField("Query step", getNestedString(item, "spec", "metricsSource", "queryStep"), formatDurationPtr(effective.Spec.MetricsSource.QueryStep), selected, metricsDefaults != nil && metricsDefaults.QueryStep != nil)
	printEffectiveField("Minimum data points", formatInt64Ptr(rawInt64Field(item, "spec", "metricsSource", "minimumDataPoints")), formatInt32Ptr(effective.Spec.MetricsSource.MinimumDataPoints), selected, metricsDefaults != nil && metricsDefaults.MinimumDataPoints != nil)
	printEffectiveField("Resize method", getNestedString(item, "spec", "updateStrategy", "resizeMethod"), string(effective.Spec.UpdateStrategy.ResizeMethod), selected, updateDefaults != nil && updateDefaults.ResizeMethod != "")
	printEffectiveField("Max CPU change", formatPercentInt64Ptr(rawInt64Field(item, "spec", "updateStrategy", "maxCpuChangePercent")), formatPercentPtr(effective.Spec.UpdateStrategy.MaxCPUChangePercent), selected, updateDefaults != nil && updateDefaults.MaxCPUChangePercent != nil)
	printEffectiveField("Max memory change", formatPercentInt64Ptr(rawInt64Field(item, "spec", "updateStrategy", "maxMemoryChangePercent")), formatPercentPtr(effective.Spec.UpdateStrategy.MaxMemoryChangePercent), selected, updateDefaults != nil && updateDefaults.MaxMemoryChangePercent != nil)
	printEffectiveField("Observation period", getNestedString(item, "spec", "updateStrategy", "safetyObservationPeriod"), effectiveObservationPeriod(effective), selected, updateDefaults != nil && updateDefaults.SafetyObservationPeriod != nil)
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
func effectiveObservationPeriod(policy *rightsizev1alpha1.RightSizePolicy) string {
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

func formatPercentInt64Ptr(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d%%", *value)
}

func applyBuiltInDefaults(policy *rightsizev1alpha1.RightSizePolicy) {
	if policy.Spec.UpdateStrategy.Mode == "" {
		policy.Spec.UpdateStrategy.Mode = rightsizev1alpha1.DefaultUpdateMode
	}
	if policy.Spec.UpdateStrategy.MaxCPUChangePercent == nil {
		value := rightsizev1alpha1.DefaultMaxCPUChangePercent
		policy.Spec.UpdateStrategy.MaxCPUChangePercent = &value
	}
	if policy.Spec.UpdateStrategy.MaxMemoryChangePercent == nil {
		value := rightsizev1alpha1.DefaultMaxMemoryChangePercent
		policy.Spec.UpdateStrategy.MaxMemoryChangePercent = &value
	}
	if policy.Spec.UpdateStrategy.Cooldown == nil {
		duration, _ := time.ParseDuration(rightsizev1alpha1.DefaultCooldown)
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: duration}
	}
	if policy.Spec.UpdateStrategy.AutoRevert == nil {
		value := rightsizev1alpha1.DefaultAutoRevert
		policy.Spec.UpdateStrategy.AutoRevert = &value
	}
	if policy.Spec.UpdateStrategy.ResizeMethod == "" {
		policy.Spec.UpdateStrategy.ResizeMethod = rightsizev1alpha1.DefaultResizeMethod
	}
	if policy.Spec.MetricsSource.MinimumDataPoints == nil {
		value := rightsizev1alpha1.DefaultMinimumDataPoints
		policy.Spec.MetricsSource.MinimumDataPoints = &value
	}
	if policy.Spec.MetricsSource.HistoryWindow == nil {
		duration, _ := time.ParseDuration(rightsizev1alpha1.DefaultHistoryWindow)
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: duration}
	}
	if policy.Spec.MetricsSource.QueryStep == nil {
		policy.Spec.MetricsSource.QueryStep = &metav1.Duration{Duration: defaultQueryStep}
	}
}

func mergeDefaultsIntoPolicy(policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) {
	if defaults == nil {
		return
	}
	mergeMetricsSource(&policy.Spec.MetricsSource, defaults.Spec.MetricsSource)
	mergeUpdateStrategy(&policy.Spec.UpdateStrategy, defaults.Spec.UpdateStrategy)
}

func mergeMetricsSource(policy *rightsizev1alpha1.MetricsSource, defaults *rightsizev1alpha1.MetricsSource) {
	if defaults == nil {
		return
	}
	if policy.HistoryWindow == nil && defaults.HistoryWindow != nil {
		policy.HistoryWindow = defaults.HistoryWindow.DeepCopy()
	}
	if policy.MinimumDataPoints == nil && defaults.MinimumDataPoints != nil {
		value := *defaults.MinimumDataPoints
		policy.MinimumDataPoints = &value
	}
	if policy.QueryStep == nil && defaults.QueryStep != nil {
		policy.QueryStep = defaults.QueryStep.DeepCopy()
	}
}

func mergeUpdateStrategy(policy *rightsizev1alpha1.UpdateStrategy, defaults *rightsizev1alpha1.UpdateStrategy) {
	if defaults == nil {
		return
	}
	if policy.Mode == "" {
		policy.Mode = defaults.Mode
	}
	if policy.Cooldown == nil && defaults.Cooldown != nil {
		policy.Cooldown = defaults.Cooldown.DeepCopy()
	}
	if policy.AutoRevert == nil && defaults.AutoRevert != nil {
		value := *defaults.AutoRevert
		policy.AutoRevert = &value
	}
	if policy.ResizeMethod == "" && defaults.ResizeMethod != "" {
		policy.ResizeMethod = defaults.ResizeMethod
	}
	if policy.MaxCPUChangePercent == nil && defaults.MaxCPUChangePercent != nil {
		value := *defaults.MaxCPUChangePercent
		policy.MaxCPUChangePercent = &value
	}
	if policy.MaxMemoryChangePercent == nil && defaults.MaxMemoryChangePercent != nil {
		value := *defaults.MaxMemoryChangePercent
		policy.MaxMemoryChangePercent = &value
	}
	if policy.SafetyObservationPeriod == nil && defaults.SafetyObservationPeriod != nil {
		policy.SafetyObservationPeriod = defaults.SafetyObservationPeriod.DeepCopy()
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
