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
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

const clusterAnnotation = "kubectl-rightsize/cluster"

// resolveContexts returns the list of context names to query based on flags.
func resolveContexts(kubeconfigPath, contextsFlag string, allContexts bool) ([]string, error) {
	if contextsFlag != "" {
		var result []string
		for _, c := range strings.Split(contextsFlag, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				result = append(result, c)
			}
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("--contexts value is empty")
		}
		return result, nil
	}
	if allContexts {
		return listKubeContexts(kubeconfigPath)
	}
	return nil, nil
}

// listKubeContexts returns all context names from the kubeconfig, sorted alphabetically.
func listKubeContexts(kubeconfigPath string) ([]string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	var contexts []string
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}
	if len(contexts) == 0 {
		return nil, fmt.Errorf("no contexts found in kubeconfig")
	}
	sort.Strings(contexts)
	return contexts, nil
}

type clusterFetchResult struct {
	cluster string
	items   []unstructured.Unstructured
	err     error
}

// fetchMultiCluster fetches policies from multiple clusters in parallel.
// Each item is annotated with the cluster context name.
// Returns the merged item list and any per-cluster warnings.
func fetchMultiCluster(
	ctx context.Context,
	kubeconfigPath string,
	contexts []string,
	namespace string,
	allNamespaces bool,
	buildClient dynamicClientFactory,
) ([]unstructured.Unstructured, []string) {
	results := make([]clusterFetchResult, len(contexts))
	var wg sync.WaitGroup

	for i, ctxName := range contexts {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()
			dynClient, defaultNS, err := buildClient(kubeconfigPath, name)
			if err != nil {
				results[idx] = clusterFetchResult{cluster: name, err: err}
				return
			}
			ns := namespace
			if ns == "" && !allNamespaces {
				ns = defaultNS
			}
			list, err := dynClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				results[idx] = clusterFetchResult{cluster: name, err: err}
				return
			}
			tagItems(list.Items, name)
			results[idx] = clusterFetchResult{cluster: name, items: list.Items}
		}(i, ctxName)
	}
	wg.Wait()

	var items []unstructured.Unstructured
	var warnings []string
	for _, r := range results {
		if r.err != nil {
			warnings = append(warnings, fmt.Sprintf("context %q: %v", r.cluster, r.err))
			continue
		}
		items = append(items, r.items...)
	}
	return items, warnings
}

// tagItems annotates each item with the cluster context name.
func tagItems(items []unstructured.Unstructured, cluster string) {
	for i := range items {
		ann := items[i].GetAnnotations()
		if ann == nil {
			ann = make(map[string]string)
		}
		ann[clusterAnnotation] = cluster
		items[i].SetAnnotations(ann)
	}
}

// itemCluster returns the cluster context name from an item's annotation.
func itemCluster(item unstructured.Unstructured) string {
	ann := item.GetAnnotations()
	if ann == nil {
		return ""
	}
	return ann[clusterAnnotation]
}

// hasClusterAnnotation returns true if any item has a cluster annotation.
func hasClusterAnnotation(items []unstructured.Unstructured) bool {
	for _, item := range items {
		if itemCluster(item) != "" {
			return true
		}
	}
	return false
}
