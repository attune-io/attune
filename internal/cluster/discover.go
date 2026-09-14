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

package cluster

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

const podsResizeResourceName = "pods/resize"

// NodeLister is the nodes subset Discover needs.
// kubernetes.Interface satisfies it via CoreV1().Nodes().
type NodeLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error)
}

// Discover returns cluster capabilities. err is non-nil only when
// ServerVersion is unusable. OpenAPI and node-list failures use
// GitVersion fallbacks and are not returned as err.
func Discover(ctx context.Context, disco discovery.DiscoveryInterface, nodes NodeLister) (*Capabilities, error) {
	if disco == nil {
		return SafeDefaults(), fmt.Errorf("discovery client is nil")
	}
	log := logr.FromContextOrDiscard(ctx)

	sv, err := disco.ServerVersion()
	if err != nil {
		return SafeDefaults(), fmt.Errorf("server version: %w", err)
	}
	if sv == nil {
		return SafeDefaults(), fmt.Errorf("server version is empty")
	}

	caps := &Capabilities{GitVersion: sv.GitVersion}
	if major, minor, ok := ParseGitVersion(sv.GitVersion); ok {
		caps.Major, caps.Minor = major, minor
	}
	caps.AllowInPlaceMemoryLimitDecrease = AllowsInPlaceMemoryLimitDecrease(sv.GitVersion)
	caps.PodsResize = hasPodsResize(disco, log)
	caps.PodLevelResourcesField = podLevelResourcesField(disco, caps, log)
	caps.InPlacePodLevelResources = inPlacePodLevelResources(ctx, nodes, caps, log)
	caps.HPAScaleToZero = hpaScaleToZero(disco, caps, log)
	return caps, nil
}

func hasPodsResize(disco discovery.DiscoveryInterface, log logr.Logger) bool {
	_, lists, err := disco.ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		log.V(1).Info("API discovery failed; pods/resize unknown", "error", err)
		return false
	}
	if err != nil {
		log.V(1).Info("partial API discovery; using available groups", "error", err)
	}
	for _, list := range lists {
		if list == nil || list.GroupVersion != "v1" {
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

func podLevelResourcesField(disco discovery.DiscoveryInterface, caps *Capabilities, log logr.Logger) bool {
	ok, err := openAPIHasPodSpecResources(disco)
	if err != nil {
		log.V(1).Info("OpenAPI unavailable; PodSpec.resources from GitVersion", "error", err)
		return atLeast(caps.Major, caps.Minor, 1, 34)
	}
	return ok
}

func openAPIHasPodSpecResources(disco discovery.DiscoveryInterface) (bool, error) {
	doc, err := disco.OpenAPISchema()
	if err != nil {
		return false, err
	}
	if doc == nil || doc.GetDefinitions() == nil {
		return false, fmt.Errorf("empty openapi document")
	}
	foundSpec := false
	for _, item := range doc.GetDefinitions().GetAdditionalProperties() {
		if item.GetName() != "io.k8s.api.core.v1.PodSpec" {
			continue
		}
		foundSpec = true
		schema := item.GetValue()
		if schema == nil || schema.GetProperties() == nil {
			return false, nil
		}
		for _, prop := range schema.GetProperties().GetAdditionalProperties() {
			if prop.GetName() == "resources" {
				return true, nil
			}
		}
		return false, nil
	}
	if !foundSpec {
		return false, fmt.Errorf("podspec definition missing")
	}
	return false, nil
}

func hpaScaleToZero(disco discovery.DiscoveryInterface, caps *Capabilities, log logr.Logger) bool {
	ok, err := openAPIHasScaledToZero(disco)
	if err != nil {
		log.V(1).Info("OpenAPI unavailable; HPAScaleToZero from GitVersion", "error", err)
		return atLeast(caps.Major, caps.Minor, 1, 37)
	}
	return ok
}

func openAPIHasScaledToZero(disco discovery.DiscoveryInterface) (bool, error) {
	doc, err := disco.OpenAPISchema()
	if err != nil {
		return false, err
	}
	if doc == nil || doc.GetDefinitions() == nil {
		return false, fmt.Errorf("empty openapi document")
	}
	for _, item := range doc.GetDefinitions().GetAdditionalProperties() {
		name := item.GetName()
		if name != "io.k8s.api.autoscaling.v2.HorizontalPodAutoscalerCondition" &&
			name != "io.k8s.api.autoscaling.v2.HorizontalPodAutoscalerConditionType" {
			continue
		}
		schema := item.GetValue()
		if schema == nil {
			continue
		}
		for _, ev := range schema.GetEnum() {
			if ev != nil && ev.GetYaml() == "ScaledToZero" {
				return true, nil
			}
		}
		if props := schema.GetProperties(); props != nil {
			for _, prop := range props.GetAdditionalProperties() {
				if prop.GetName() != "type" || prop.GetValue() == nil {
					continue
				}
				for _, ev := range prop.GetValue().GetEnum() {
					if ev != nil && ev.GetYaml() == "ScaledToZero" {
						return true, nil
					}
				}
			}
		}
	}
	return false, fmt.Errorf("scaledToZero enum not found")
}

func inPlacePodLevelResources(ctx context.Context, nodes NodeLister, caps *Capabilities, log logr.Logger) bool {
	if nodes == nil {
		log.V(1).Info("no node lister; InPlacePodLevelResources from GitVersion")
		return atLeast(caps.Major, caps.Minor, 1, 36)
	}
	list, err := nodes.List(ctx, metav1.ListOptions{})
	if err != nil {
		log.V(1).Info("node list failed; InPlacePodLevelResources from GitVersion", "error", err)
		return atLeast(caps.Major, caps.Minor, 1, 36)
	}
	return inPlaceFromDeclaredFeatures(list, caps.Major, caps.Minor)
}

// inPlaceFromDeclaredFeatures is the three-way probe:
//  1. any Ready node lists the feature name => true
//  2. any Ready node has a non-empty list that omits the name => false
//  3. every Ready node has nil/empty declaredFeatures => GitVersion >= 1.36
//
// When the feature later GAs, kubelets stop listing the name. Empty
// plus GitVersion past that GA minor stays true via case 3. Do not
// freeze "name absent => false" forever.
func inPlaceFromDeclaredFeatures(list *corev1.NodeList, major, minor uint) bool {
	if list == nil {
		return atLeast(major, minor, 1, 36)
	}
	seenName := false
	seenNonEmptyOmit := false
	for i := range list.Items {
		node := &list.Items[i]
		if !nodeReady(node) {
			continue
		}
		feats := node.Status.DeclaredFeatures
		if len(feats) == 0 {
			continue
		}
		if containsString(feats, FeatureInPlacePodLevelResources) {
			seenName = true
			continue
		}
		seenNonEmptyOmit = true
	}
	if seenName {
		return true
	}
	if seenNonEmptyOmit {
		return false
	}
	return atLeast(major, minor, 1, 36)
}

func nodeReady(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func containsString(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}
