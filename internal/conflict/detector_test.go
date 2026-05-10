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

package conflict

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckAnnotationOptOut(t *testing.T) {
	detector := NewDetector(testr.New(t))

	tests := []struct {
		name string
		obj  metav1.ObjectMeta
		want bool
	}{
		{
			name: "annotation present with value true",
			obj: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rightsize.io/skip": "true",
				},
			},
			want: true,
		},
		{
			name: "annotation absent",
			obj:  metav1.ObjectMeta{},
			want: false,
		},
		{
			name: "annotation present with value false",
			obj: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rightsize.io/skip": "false",
				},
			},
			want: false,
		},
		{
			name: "different annotation key",
			obj: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rightsize.io/enabled": "true",
				},
			},
			want: false,
		},
		{
			name: "empty annotations map",
			obj: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detector.CheckAnnotationOptOut(tt.obj)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckActiveRollout(t *testing.T) {
	detector := NewDetector(testr.New(t))

	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		want       bool
	}{
		{
			name: "rollout in progress",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:        3,
					UpdatedReplicas: 1,
				},
			},
			want: true,
		},
		{
			name: "rollout complete",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:        3,
					UpdatedReplicas: 3,
				},
			},
			want: false,
		},
		{
			name: "zero replicas",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:        0,
					UpdatedReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "scaling up from zero",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:        2,
					UpdatedReplicas: 0,
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detector.CheckActiveRollout(tt.deployment)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckHPAConflict_Found(t *testing.T) {
	detector := NewDetector(testr.New(t))

	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "my-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-app",
				},
			},
		},
	}

	conflict := detector.CheckHPAConflict(hpas, "my-app", "Deployment")
	assert.NotNil(t, conflict)
	assert.Equal(t, ConflictHPA, conflict.Type)
	assert.Equal(t, "my-hpa", conflict.Name)
	assert.Contains(t, conflict.Message, "HPA my-hpa targets the same Deployment/my-app")
}

func TestCheckHPAConflict_NotFound(t *testing.T) {
	detector := NewDetector(testr.New(t))

	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "other-app",
				},
			},
		},
	}

	conflict := detector.CheckHPAConflict(hpas, "my-app", "Deployment")
	assert.Nil(t, conflict)
}

func TestCheckHPAConflict_EmptyList(t *testing.T) {
	detector := NewDetector(testr.New(t))

	conflict := detector.CheckHPAConflict([]autoscalingv2.HorizontalPodAutoscaler{}, "my-app", "Deployment")
	assert.Nil(t, conflict)
}

func TestCheckVPAConflict_Found(t *testing.T) {
	detector := NewDetector(testr.New(t))

	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(vpaGVK)
	vpa.SetName("my-vpa")
	vpa.SetNamespace("default")
	vpa.Object["apiVersion"] = "autoscaling.k8s.io/v1"
	vpa.Object["kind"] = "VerticalPodAutoscaler"
	_ = unstructured.SetNestedMap(vpa.Object, map[string]interface{}{
		"kind": "Deployment",
		"name": "my-app",
	}, "spec", "targetRef")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vpa).Build()

	result := detector.CheckVPAConflict(context.Background(), c, "default", "my-app", "Deployment")
	assert.NotNil(t, result)
	assert.Equal(t, ConflictVPA, result.Type)
	assert.Equal(t, "my-vpa", result.Name)
	assert.Contains(t, result.Message, "VPA my-vpa targets the same Deployment/my-app")
}

func TestCheckVPAConflict_NotFound(t *testing.T) {
	detector := NewDetector(testr.New(t))

	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(vpaGVK)
	vpa.SetName("other-vpa")
	vpa.SetNamespace("default")
	vpa.Object["apiVersion"] = "autoscaling.k8s.io/v1"
	vpa.Object["kind"] = "VerticalPodAutoscaler"
	_ = unstructured.SetNestedMap(vpa.Object, map[string]interface{}{
		"kind": "Deployment",
		"name": "other-app",
	}, "spec", "targetRef")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vpa).Build()

	result := detector.CheckVPAConflict(context.Background(), c, "default", "my-app", "Deployment")
	assert.Nil(t, result)
}

func TestCheckVPAConflict_NoCRD(t *testing.T) {
	detector := NewDetector(testr.New(t))

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	result := detector.CheckVPAConflict(context.Background(), c, "default", "my-app", "Deployment")
	assert.Nil(t, result)
}

func TestCheckHPAConflict_DifferentKind(t *testing.T) {
	detector := NewDetector(testr.New(t))

	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "my-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "StatefulSet",
					Name: "my-app",
				},
			},
		},
	}

	conflict := detector.CheckHPAConflict(hpas, "my-app", "Deployment")
	assert.Nil(t, conflict)
}
