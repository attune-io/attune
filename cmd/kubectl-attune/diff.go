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
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"
)

// printDiff lists all AttunePolicies and shows resource change diffs for
// each workload with recommendations.
func printDiff(ctx context.Context, dynClient dynamic.Interface, namespace, output string) {
	list := fetchPolicies(ctx, dynClient, namespace)
	printDiffItems(list.Items, output)
}

// printDiffItems renders diff output for the given policy items.
func printDiffItems(items []unstructured.Unstructured, output string) {
	if len(items) == 0 {
		fmt.Println("No AttunePolicies found.")
		return
	}

	hasOutput := false
	for _, item := range items {
		recs, found, _ := unstructured.NestedSlice(item.Object, "status", "recommendations")
		if !found || len(recs) == 0 {
			continue
		}

		policyName := item.GetName()
		ns := item.GetNamespace()

		for _, r := range recs {
			rec, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			workload, _ := rec["workload"].(string)
			kind, _ := rec["kind"].(string)
			if kind == "" {
				kind = "Deployment"
			}
			containers, _ := rec["containers"].([]interface{})
			if len(containers) == 0 {
				continue
			}

			if output == "yaml" {
				printDiffYAML(ns, kind, workload, containers)
			} else {
				printDiffUnified(ns, kind, workload, policyName, containers)
			}
			hasOutput = true
		}
	}

	if !hasOutput {
		fmt.Println("No recommendations available. Run 'kubectl attune status' to check policy status.")
	}
}

// printDiffUnified outputs a unified-diff style view of resource changes.
func printDiffUnified(namespace, kind, workload, policyName string, containers []interface{}) {
	fmt.Printf("--- a/%s/%s/%s\n", namespace, kind, workload)
	fmt.Printf("+++ b/%s/%s/%s (recommended by %s)\n", namespace, kind, workload, policyName)

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
		curCPULimit, _ := current["cpuLimit"].(string)
		recCPULimit, _ := recommended["cpuLimit"].(string)
		curMemLimit, _ := current["memoryLimit"].(string)
		recMemLimit, _ := recommended["memoryLimit"].(string)

		hasRequestChanges := curCPU != recCPU || curMem != recMem
		hasLimitChanges := curCPULimit != recCPULimit || curMemLimit != recMemLimit

		if !hasRequestChanges && !hasLimitChanges {
			continue
		}

		fmt.Printf("@@ container: %s @@\n", name)
		fmt.Println("   resources:")

		if hasRequestChanges {
			fmt.Println("     requests:")
			printDiffResourceLine("cpu", curCPU, recCPU)
			printDiffResourceLine("memory", curMem, recMem)
		}

		if hasLimitChanges {
			fmt.Println("     limits:")
			printDiffResourceLine("cpu", curCPULimit, recCPULimit)
			printDiffResourceLine("memory", curMemLimit, recMemLimit)
		}
	}
}

// printDiffResourceLine prints a single resource line in diff format.
func printDiffResourceLine(resourceName, current, recommended string) {
	if current == recommended {
		if current != "" && current != "0" {
			fmt.Printf("       %s: \"%s\"\n", resourceName, current)
		}
		return
	}
	if current != "" && current != "0" {
		fmt.Printf("-      %s: \"%s\"\n", resourceName, current)
	}
	if recommended != "" && recommended != "0" {
		fmt.Printf("+      %s: \"%s\"\n", resourceName, recommended)
	}
}

// printDiffYAML outputs a YAML patch manifest with recommended resources.
func printDiffYAML(namespace, kind, workload string, containers []interface{}) {
	patchContainers := buildPatchContainers(containers)
	if len(patchContainers) == 0 {
		return
	}

	spec := buildPatchSpec(kind, patchContainers)
	patch := map[string]interface{}{
		"apiVersion": apiVersionForKind(kind),
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":      workload,
			"namespace": namespace,
		},
		"spec": spec,
	}

	data, err := json.Marshal(patch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling patch: %v\n", err)
		return
	}
	yamlData, err := sigsyaml.JSONToYAML(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error converting to YAML: %v\n", err)
		return
	}
	fmt.Printf("---\n%s", string(yamlData))
}

// buildPatchContainers builds the containers portion of a YAML patch
// from recommendations.
func buildPatchContainers(containers []interface{}) []interface{} {
	var result []interface{}
	for _, c := range containers {
		cont, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := cont["name"].(string)
		recommended, _ := cont["recommended"].(map[string]interface{})

		recCPU, _ := recommended["cpuRequest"].(string)
		recMem, _ := recommended["memoryRequest"].(string)
		recCPULimit, _ := recommended["cpuLimit"].(string)
		recMemLimit, _ := recommended["memoryLimit"].(string)

		resources := map[string]interface{}{}
		requests := map[string]interface{}{}
		limits := map[string]interface{}{}

		if recCPU != "" && recCPU != "0" {
			requests["cpu"] = recCPU
		}
		if recMem != "" && recMem != "0" {
			requests["memory"] = recMem
		}
		if recCPULimit != "" && recCPULimit != "0" {
			limits["cpu"] = recCPULimit
		}
		if recMemLimit != "" && recMemLimit != "0" {
			limits["memory"] = recMemLimit
		}

		if len(requests) > 0 {
			resources["requests"] = requests
		}
		if len(limits) > 0 {
			resources["limits"] = limits
		}

		entry := map[string]interface{}{
			"name":      name,
			"resources": resources,
		}
		result = append(result, entry)
	}
	return result
}

// buildPatchSpec builds the appropriate spec structure based on the
// workload kind.
func buildPatchSpec(kind string, containers []interface{}) map[string]interface{} {
	podSpec := map[string]interface{}{
		"containers": containers,
	}
	template := map[string]interface{}{
		"spec": podSpec,
	}

	if kind == "CronJob" {
		return map[string]interface{}{
			"jobTemplate": map[string]interface{}{
				"spec": map[string]interface{}{
					"template": template,
				},
			},
		}
	}

	return map[string]interface{}{
		"template": template,
	}
}

// apiVersionForKind returns the API version for a given workload kind.
func apiVersionForKind(kind string) string {
	switch kind {
	case "CronJob", "Job":
		return "batch/v1"
	default:
		return "apps/v1"
	}
}
