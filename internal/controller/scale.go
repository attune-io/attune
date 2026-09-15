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

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workloadScaleState is whether apply should run for a workload.
// HPA ScaledToZero is not a human turning the app off.
type workloadScaleState int

const (
	scaleActive workloadScaleState = iota
	scaleManualZero
	scaleHPAZero
)

func (s workloadScaleState) idle() bool {
	return s == scaleManualZero || s == scaleHPAZero
}

func (s workloadScaleState) String() string {
	switch s {
	case scaleManualZero:
		return "manualZero"
	case scaleHPAZero:
		return "hpaScaledToZero"
	default:
		return "active"
	}
}

// classifyWorkloadScale returns HPA zero, manual zero, or active.
// ScaledToZero True wins even when spec.replicas is nil or greater than 0.
// Manual zero uses spec.replicas only, never status.replicas.
func classifyWorkloadScale(specReplicas *int32, matchingHPA *autoscalingv2.HorizontalPodAutoscaler) workloadScaleState {
	if matchingHPA != nil {
		for _, c := range matchingHPA.Status.Conditions {
			if c.Type == autoscalingv2.ScaledToZero && c.Status == corev1.ConditionTrue {
				return scaleHPAZero
			}
		}
	}
	if specReplicas != nil && *specReplicas == 0 {
		return scaleManualZero
	}
	return scaleActive
}

func workloadSpecReplicas(obj client.Object) *int32 {
	switch w := obj.(type) {
	case *appsv1.Deployment:
		return w.Spec.Replicas
	case *appsv1.StatefulSet:
		return w.Spec.Replicas
	case *appsv1.ReplicaSet:
		return w.Spec.Replicas
	default:
		return nil
	}
}

func listNamespaceHPAs(ctx context.Context, c client.Client, ns string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	if c == nil {
		return nil, nil
	}
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func matchingHPA(hpas []autoscalingv2.HorizontalPodAutoscaler, name, kind string) *autoscalingv2.HorizontalPodAutoscaler {
	for i := range hpas {
		if hpas[i].Spec.ScaleTargetRef.Name == name && hpas[i].Spec.ScaleTargetRef.Kind == kind {
			return &hpas[i]
		}
	}
	return nil
}

func classifyObjectScale(obj client.Object, hpas []autoscalingv2.HorizontalPodAutoscaler) workloadScaleState {
	if obj == nil {
		return scaleActive
	}
	return classifyWorkloadScale(workloadSpecReplicas(obj), matchingHPA(hpas, obj.GetName(), workloadKindName(obj)))
}
