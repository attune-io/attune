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

// Package conflict detects conflicts with VPA, HPA, and other
// RightSizePolicies that could interfere with resize operations.
package conflict

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConflictType identifies the kind of resource that conflicts with a
// RightSizePolicy.
type ConflictType string

const (
	// ConflictVPA indicates a VerticalPodAutoscaler targets the same workload.
	ConflictVPA ConflictType = "VPA"
	// ConflictHPA indicates a HorizontalPodAutoscaler targets the same workload.
	ConflictHPA ConflictType = "HPA"
	// ConflictPolicy indicates another RightSizePolicy targets the same workload.
	ConflictPolicy ConflictType = "RightSizePolicy"
)

// Conflict describes a single detected conflict.
type Conflict struct {
	Type    ConflictType
	Name    string
	Message string
}

// Detector checks for conflicts on target workloads.
type Detector struct {
	logger logr.Logger
}

// NewDetector creates a Detector with the given logger.
func NewDetector(logger logr.Logger) *Detector {
	return &Detector{
		logger: logger,
	}
}

// CheckAnnotationOptOut returns true if the object carries the annotation
// "rightsize.io/skip" set to "true", indicating that the workload has opted
// out of automatic right-sizing.
func (d *Detector) CheckAnnotationOptOut(obj metav1.ObjectMeta) bool {
	if obj.Annotations == nil {
		return false
	}
	return obj.Annotations["rightsize.io/skip"] == "true"
}

// CheckActiveRollout returns true if the deployment has an active rollout in
// progress. A rollout is considered active when UpdatedReplicas does not
// match Replicas, meaning not all pods have been updated to the latest
// revision yet.
func (d *Detector) CheckActiveRollout(deployment *appsv1.Deployment) bool {
	return deployment.Status.UpdatedReplicas != deployment.Status.Replicas
}

// vpaGVK is the GroupVersionKind for VerticalPodAutoscaler resources.
var vpaGVK = schema.GroupVersionKind{
	Group:   "autoscaling.k8s.io",
	Version: "v1",
	Kind:    "VerticalPodAutoscalerList",
}

// CheckVPAConflict lists VerticalPodAutoscaler resources in the namespace and
// returns a Conflict if any VPA targets the same workload. Returns nil if the
// VPA CRD is not installed (no error) or no conflicts are found.
func (d *Detector) CheckVPAConflict(ctx context.Context, c client.Client, namespace, workloadName, workloadKind string) *Conflict {
	vpaList := &unstructured.UnstructuredList{}
	vpaList.SetGroupVersionKind(vpaGVK)

	if err := c.List(ctx, vpaList, client.InNamespace(namespace)); err != nil {
		// VPA CRD not installed or RBAC issue; not an error for our purposes.
		d.logger.V(1).Info("Could not list VPAs (CRD may not be installed)", "error", err)
		return nil
	}

	for _, vpa := range vpaList.Items {
		targetRef, found, _ := unstructured.NestedMap(vpa.Object, "spec", "targetRef")
		if !found {
			continue
		}
		refKind, _ := targetRef["kind"].(string)
		refName, _ := targetRef["name"].(string)
		if refKind == workloadKind && refName == workloadName {
			return &Conflict{
				Type:    ConflictVPA,
				Name:    vpa.GetName(),
				Message: fmt.Sprintf("VPA %s targets the same %s/%s; consider disabling VPA to avoid conflicting resource adjustments", vpa.GetName(), workloadKind, workloadName),
			}
		}
	}
	return nil
}

// CheckPolicyConflict checks if another RightSizePolicy with higher weight
// targets the same workload. Returns a Conflict if a higher-weight policy
// exists (the current policy should defer). Returns nil if the current policy
// has the highest weight or no overlap exists.
func (d *Detector) CheckPolicyConflict(ctx context.Context, c client.Client, namespace, workloadName, workloadKind, currentPolicyName string, currentWeight int32) *Conflict {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "rightsize.io",
		Version: "v1alpha1",
		Kind:    "RightSizePolicyList",
	})

	if err := c.List(ctx, policyList, client.InNamespace(namespace)); err != nil {
		d.logger.V(1).Info("Could not list RightSizePolicies for conflict check", "error", err)
		return nil
	}

	for _, policy := range policyList.Items {
		if policy.GetName() == currentPolicyName {
			continue
		}

		// Check if this policy targets the same workload by name.
		targetKind, _, _ := unstructured.NestedString(policy.Object, "spec", "targetRef", "kind")
		targetName, _, _ := unstructured.NestedString(policy.Object, "spec", "targetRef", "name")
		if targetKind != workloadKind || targetName != workloadName {
			continue
		}

		otherWeight, _, _ := unstructured.NestedInt64(policy.Object, "spec", "weight")
		if otherWeight > int64(currentWeight) {
			return &Conflict{
				Type: ConflictPolicy,
				Name: policy.GetName(),
				Message: fmt.Sprintf("RightSizePolicy %s has higher weight (%d > %d) for %s/%s; deferring",
					policy.GetName(), otherWeight, currentWeight, workloadKind, workloadName),
			}
		}
	}
	return nil
}

// CheckHPAConflict checks if an HPA targets the same workload and returns a Conflict if so.
func (d *Detector) CheckHPAConflict(hpas []autoscalingv2.HorizontalPodAutoscaler, workloadName, workloadKind string) *Conflict {
	for _, hpa := range hpas {
		if hpa.Spec.ScaleTargetRef.Name == workloadName && hpa.Spec.ScaleTargetRef.Kind == workloadKind {
			return &Conflict{
				Type:    ConflictHPA,
				Name:    hpa.Name,
				Message: fmt.Sprintf("HPA %s targets the same %s/%s; kube-rightsize will adjust requests without interfering with HPA scaling", hpa.Name, workloadKind, workloadName),
			}
		}
	}
	return nil
}

// ListVPAs fetches all VPAs in the namespace once for efficient conflict checking.
// Returns nil if the VPA CRD is not installed.
func (d *Detector) ListVPAs(ctx context.Context, c client.Client, namespace string) *unstructured.UnstructuredList {
	vpaList := &unstructured.UnstructuredList{}
	vpaList.SetGroupVersionKind(vpaGVK)

	if err := c.List(ctx, vpaList, client.InNamespace(namespace)); err != nil {
		d.logger.V(1).Info("Could not list VPAs (CRD may not be installed)", "error", err)
		return nil
	}
	return vpaList
}

// CheckVPAConflictInMemory checks for VPA conflicts against a pre-fetched list.
// Use with ListVPAs to avoid repeated API calls when checking multiple workloads.
func (d *Detector) CheckVPAConflictInMemory(vpaList *unstructured.UnstructuredList, workloadName, workloadKind string) *Conflict {
	if vpaList == nil {
		return nil
	}

	for _, vpa := range vpaList.Items {
		targetRef, found, _ := unstructured.NestedMap(vpa.Object, "spec", "targetRef")
		if !found {
			continue
		}
		refKind, _ := targetRef["kind"].(string)
		refName, _ := targetRef["name"].(string)
		if refKind == workloadKind && refName == workloadName {
			return &Conflict{
				Type:    ConflictVPA,
				Name:    vpa.GetName(),
				Message: fmt.Sprintf("VPA %s targets the same %s/%s; consider disabling VPA to avoid conflicting resource adjustments", vpa.GetName(), workloadKind, workloadName),
			}
		}
	}
	return nil
}

// ListPolicies fetches all RightSizePolicies in the namespace once for efficient conflict checking.
func (d *Detector) ListPolicies(ctx context.Context, c client.Client, namespace string) *unstructured.UnstructuredList {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "rightsize.io",
		Version: "v1alpha1",
		Kind:    "RightSizePolicyList",
	})

	if err := c.List(ctx, policyList, client.InNamespace(namespace)); err != nil {
		d.logger.V(1).Info("Could not list RightSizePolicies for conflict check", "error", err)
		return nil
	}
	return policyList
}

// CheckPolicyConflictInMemory checks for policy conflicts against a pre-fetched list.
// Use with ListPolicies to avoid repeated API calls when checking multiple workloads.
func (d *Detector) CheckPolicyConflictInMemory(policyList *unstructured.UnstructuredList, workloadName, workloadKind, currentPolicyName string, currentWeight int32) *Conflict {
	if policyList == nil {
		return nil
	}

	for _, policy := range policyList.Items {
		if policy.GetName() == currentPolicyName {
			continue
		}

		// Check if this policy targets the same workload by name.
		targetKind, _, _ := unstructured.NestedString(policy.Object, "spec", "targetRef", "kind")
		targetName, _, _ := unstructured.NestedString(policy.Object, "spec", "targetRef", "name")
		if targetKind != workloadKind || targetName != workloadName {
			continue
		}

		otherWeight, _, _ := unstructured.NestedInt64(policy.Object, "spec", "weight")
		if otherWeight > int64(currentWeight) {
			return &Conflict{
				Type: ConflictPolicy,
				Name: policy.GetName(),
				Message: fmt.Sprintf("RightSizePolicy %s has higher weight (%d > %d) for %s/%s; deferring",
					policy.GetName(), otherWeight, currentWeight, workloadKind, workloadName),
			}
		}
	}
	return nil
}
