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
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

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
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: kubectl rightsize <status|savings|recommendations> [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
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

	switch cmd {
	case "status":
		printStatus(ctx, dynClient, *namespace)
	case "savings":
		printSavings(ctx, dynClient, *namespace)
	case "recommendations":
		printRecommendations(ctx, dynClient, *namespace)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		fs.Usage()
		os.Exit(1)
	}
}

func printStatus(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing policies: %v\n", err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tMODE\tWORKLOADS\tRESIZED\tREADY\tAGE")

	for _, item := range list.Items {
		ns := item.GetNamespace()
		name := item.GetName()
		mode := getNestedString(item, "spec", "updateStrategy", "mode")
		workloads := getNestedInt64(item, "status", "workloads", "discovered")
		resized := getNestedInt64(item, "status", "workloads", "resized")
		ready := getReadyStatus(item)
		age := formatAge(item.GetCreationTimestamp().Time)

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			ns, name, mode, workloads, resized, ready, age)
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
}

func printSavings(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing policies: %v\n", err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tCPU SAVED\tMEMORY SAVED\tEST. MONTHLY")

	for _, item := range list.Items {
		ns := item.GetNamespace()
		name := item.GetName()
		cpuSaved := getNestedString(item, "status", "savings", "cpuRequestReduction")
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

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			ns, name, cpuSaved, memSaved, estMonthly)
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

func getReadyStatus(obj unstructured.Unstructured) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "Unknown"
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		if condType == "Ready" {
			status, _ := cond["status"].(string)
			return status
		}
	}

	return "Unknown"
}

func printRecommendations(ctx context.Context, dynClient dynamic.Interface, namespace string) {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing policies: %v\n", err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tCONTAINER\tCPU REQ\tCPU REC\tMEM REQ\tMEM REC\tCONFIDENCE")

	for _, item := range list.Items {
		recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
		if !found {
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

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\n",
					workload, name, curCPU, recCPU, curMem, recMem, confidence*100)
			}
		}
	}

	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing output: %v\n", err)
	}
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
