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
	"fmt"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
					AnnotationSkip: "true",
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
					AnnotationSkip: "false",
				},
			},
			want: false,
		},
		{
			name: "different annotation key",
			obj: metav1.ObjectMeta{
				Annotations: map[string]string{
					"attune.io/enabled": "true",
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

func TestListVPAs_ListError(t *testing.T) {
	detector := NewDetector(testr.New(t))

	// Interceptor forces List to return an error.
	errClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("simulated API error")
		},
	}).Build()

	result := detector.ListVPAs(context.Background(), errClient, "default")
	assert.Nil(t, result, "ListVPAs should return nil on List error")
}

func TestListPolicies_ListError(t *testing.T) {
	detector := NewDetector(testr.New(t))

	errClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return fmt.Errorf("simulated API error")
		},
	}).Build()

	result := detector.ListPolicies(context.Background(), errClient, "default")
	assert.Nil(t, result, "ListPolicies should return nil on List error")
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

// ---------- CheckPolicyConflict ----------

func TestCheckPolicyConflict_HigherWeightDefers(t *testing.T) {
	detector := NewDetector(testr.New(t))

	otherPolicy := &unstructured.Unstructured{}
	otherPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "attune.io",
		Version: "v1alpha1",
		Kind:    "AttunePolicy",
	})
	otherPolicy.SetName("high-priority")
	otherPolicy.SetNamespace("default")
	_ = unstructured.SetNestedField(otherPolicy.Object, "Deployment", "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(otherPolicy.Object, "my-app", "spec", "targetRef", "name")
	_ = unstructured.SetNestedField(otherPolicy.Object, int64(200), "spec", "weight")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(otherPolicy).Build()

	result := detector.CheckPolicyConflict(context.Background(), c, "default", "my-app", "Deployment", nil, "low-priority", 100)
	assert.NotNil(t, result)
	assert.Equal(t, ConflictPolicy, result.Type)
	assert.Equal(t, "high-priority", result.Name)
	assert.Contains(t, result.Message, "higher weight")
}

func TestCheckPolicyConflict_LowerWeightNoConflict(t *testing.T) {
	detector := NewDetector(testr.New(t))

	otherPolicy := &unstructured.Unstructured{}
	otherPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "attune.io",
		Version: "v1alpha1",
		Kind:    "AttunePolicy",
	})
	otherPolicy.SetName("low-priority")
	otherPolicy.SetNamespace("default")
	_ = unstructured.SetNestedField(otherPolicy.Object, "Deployment", "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(otherPolicy.Object, "my-app", "spec", "targetRef", "name")
	_ = unstructured.SetNestedField(otherPolicy.Object, int64(50), "spec", "weight")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(otherPolicy).Build()

	result := detector.CheckPolicyConflict(context.Background(), c, "default", "my-app", "Deployment", nil, "high-priority", 100)
	assert.Nil(t, result)
}

func TestCheckPolicyConflict_DifferentWorkload(t *testing.T) {
	detector := NewDetector(testr.New(t))

	otherPolicy := &unstructured.Unstructured{}
	otherPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "attune.io",
		Version: "v1alpha1",
		Kind:    "AttunePolicy",
	})
	otherPolicy.SetName("other")
	otherPolicy.SetNamespace("default")
	_ = unstructured.SetNestedField(otherPolicy.Object, "Deployment", "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(otherPolicy.Object, "other-app", "spec", "targetRef", "name")
	_ = unstructured.SetNestedField(otherPolicy.Object, int64(999), "spec", "weight")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(otherPolicy).Build()

	result := detector.CheckPolicyConflict(context.Background(), c, "default", "my-app", "Deployment", nil, "current", 100)
	assert.Nil(t, result)
}

func TestCheckPolicyConflict_SkipsSelf(t *testing.T) {
	detector := NewDetector(testr.New(t))

	self := &unstructured.Unstructured{}
	self.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "attune.io",
		Version: "v1alpha1",
		Kind:    "AttunePolicy",
	})
	self.SetName("current-policy")
	self.SetNamespace("default")
	_ = unstructured.SetNestedField(self.Object, "Deployment", "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(self.Object, "my-app", "spec", "targetRef", "name")
	_ = unstructured.SetNestedField(self.Object, int64(100), "spec", "weight")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(self).Build()

	result := detector.CheckPolicyConflict(context.Background(), c, "default", "my-app", "Deployment", nil, "current-policy", 100)
	assert.Nil(t, result, "should not conflict with itself")
}

// ---------- CheckVPAConflictInMemory ----------

func newVPA(name, targetKind, targetName string) unstructured.Unstructured {
	vpa := unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler",
	})
	vpa.SetName(name)
	vpa.SetNamespace("default")
	_ = unstructured.SetNestedMap(vpa.Object, map[string]interface{}{
		"kind": targetKind, "name": targetName,
	}, "spec", "targetRef")
	return vpa
}

func TestCheckVPAConflictInMemory_NilList(t *testing.T) {
	detector := NewDetector(testr.New(t))
	assert.Nil(t, detector.CheckVPAConflictInMemory(nil, "my-app", "Deployment"))
}

func TestCheckVPAConflictInMemory_EmptyList(t *testing.T) {
	detector := NewDetector(testr.New(t))
	list := &unstructured.UnstructuredList{}
	assert.Nil(t, detector.CheckVPAConflictInMemory(list, "my-app", "Deployment"))
}

func TestCheckVPAConflictInMemory_Match(t *testing.T) {
	detector := NewDetector(testr.New(t))
	vpa := newVPA("my-vpa", "Deployment", "my-app")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{vpa}}

	result := detector.CheckVPAConflictInMemory(list, "my-app", "Deployment")
	assert.NotNil(t, result)
	assert.Equal(t, ConflictVPA, result.Type)
	assert.Equal(t, "my-vpa", result.Name)
	assert.Contains(t, result.Message, "VPA my-vpa")
}

func TestCheckVPAConflictInMemory_DifferentName(t *testing.T) {
	detector := NewDetector(testr.New(t))
	vpa := newVPA("other-vpa", "Deployment", "other-app")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{vpa}}

	assert.Nil(t, detector.CheckVPAConflictInMemory(list, "my-app", "Deployment"))
}

func TestCheckVPAConflictInMemory_DifferentKind(t *testing.T) {
	detector := NewDetector(testr.New(t))
	vpa := newVPA("my-vpa", "StatefulSet", "my-app")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{vpa}}

	assert.Nil(t, detector.CheckVPAConflictInMemory(list, "my-app", "Deployment"))
}

func TestCheckVPAConflictInMemory_NoTargetRef(t *testing.T) {
	detector := NewDetector(testr.New(t))
	vpa := unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler",
	})
	vpa.SetName("broken-vpa")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{vpa}}

	assert.Nil(t, detector.CheckVPAConflictInMemory(list, "my-app", "Deployment"))
}

func TestCheckVPAConflictInMemory_MultipleVPAs_MatchSecond(t *testing.T) {
	detector := NewDetector(testr.New(t))
	vpa1 := newVPA("unrelated-vpa", "Deployment", "other-app")
	vpa2 := newVPA("matching-vpa", "Deployment", "my-app")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{vpa1, vpa2}}

	result := detector.CheckVPAConflictInMemory(list, "my-app", "Deployment")
	assert.NotNil(t, result)
	assert.Equal(t, "matching-vpa", result.Name)
}

// ---------- CheckPolicyConflictInMemory ----------

func newPolicy(name, targetKind, targetName string, weight int64) unstructured.Unstructured {
	p := unstructured.Unstructured{}
	p.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "attune.io", Version: "v1alpha1", Kind: "AttunePolicy",
	})
	p.SetName(name)
	p.SetNamespace("default")
	_ = unstructured.SetNestedField(p.Object, targetKind, "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(p.Object, targetName, "spec", "targetRef", "name")
	_ = unstructured.SetNestedField(p.Object, weight, "spec", "weight")
	return p
}

func TestCheckPolicyConflictInMemory_NilList(t *testing.T) {
	detector := NewDetector(testr.New(t))
	assert.Nil(t, detector.CheckPolicyConflictInMemory(nil, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_EmptyList(t *testing.T) {
	detector := NewDetector(testr.New(t))
	list := &unstructured.UnstructuredList{}
	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_SkipsSelf(t *testing.T) {
	detector := NewDetector(testr.New(t))
	self := newPolicy("current", "Deployment", "my-app", 999)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{self}}

	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_HigherWeightConflicts(t *testing.T) {
	detector := NewDetector(testr.New(t))
	other := newPolicy("high-priority", "Deployment", "my-app", 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "low-priority", 100)
	assert.NotNil(t, result)
	assert.Equal(t, ConflictPolicy, result.Type)
	assert.Equal(t, "high-priority", result.Name)
	assert.Contains(t, result.Message, "higher weight or priority (200 vs 100)")
}

func TestCheckPolicyConflictInMemory_LowerWeightNoConflict(t *testing.T) {
	detector := NewDetector(testr.New(t))
	other := newPolicy("low-priority", "Deployment", "my-app", 50)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "high-priority", 100))
}

func TestCheckPolicyConflictInMemory_EqualWeightNoConflict(t *testing.T) {
	detector := NewDetector(testr.New(t))
	other := newPolicy("peer", "Deployment", "my-app", 100)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_EqualWeightTiebreaker(t *testing.T) {
	detector := NewDetector(testr.New(t))
	// "alpha" < "zeta" lexicographically, so "alpha" wins the tiebreaker
	// and "zeta" (current) should defer.
	other := newPolicy("alpha", "Deployment", "my-app", 100)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "zeta", 100)
	assert.NotNil(t, result)
	assert.Equal(t, ConflictPolicy, result.Type)
	assert.Equal(t, "alpha", result.Name)
}

func TestCheckPolicyConflictInMemory_DifferentWorkload(t *testing.T) {
	detector := NewDetector(testr.New(t))
	other := newPolicy("other", "Deployment", "other-app", 999)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_DifferentKind(t *testing.T) {
	detector := NewDetector(testr.New(t))
	other := newPolicy("other", "StatefulSet", "my-app", 999)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{other}}

	assert.Nil(t, detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100))
}

func TestCheckPolicyConflictInMemory_MultiplePolicies(t *testing.T) {
	detector := NewDetector(testr.New(t))
	low := newPolicy("low", "Deployment", "my-app", 50)
	high := newPolicy("high", "Deployment", "my-app", 200)
	unrelated := newPolicy("unrelated", "Deployment", "other-app", 999)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{low, high, unrelated}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", nil, "current", 100)
	assert.NotNil(t, result)
	assert.Equal(t, "high", result.Name)
}

// ---------- Selector-based conflict detection ----------

func newSelectorPolicy(name string, targetKind string, matchLabels map[string]string, weight int64) unstructured.Unstructured {
	p := unstructured.Unstructured{}
	p.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "attune.io", Version: "v1alpha1", Kind: "AttunePolicy",
	})
	p.SetName(name)
	_ = unstructured.SetNestedField(p.Object, targetKind, "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(p.Object, weight, "spec", "weight")

	ml := make(map[string]interface{}, len(matchLabels))
	for k, v := range matchLabels {
		ml[k] = v
	}
	_ = unstructured.SetNestedField(p.Object, ml, "spec", "targetRef", "selector", "matchLabels")
	return p
}

func TestCheckPolicyConflictInMemory_SelectorMatchesWorkload(t *testing.T) {
	detector := NewDetector(testr.New(t))
	selectorPolicy := newSelectorPolicy("broad-policy", "Deployment", map[string]string{"app": "web"}, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{selectorPolicy}}

	workloadLabels := map[string]string{"app": "web", "version": "v2"}
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", workloadLabels, "narrow-policy", 100)

	assert.NotNil(t, result)
	assert.Equal(t, ConflictPolicy, result.Type)
	assert.Equal(t, "broad-policy", result.Name)
}

func TestCheckPolicyConflictInMemory_SelectorDoesNotMatchWorkload(t *testing.T) {
	detector := NewDetector(testr.New(t))
	selectorPolicy := newSelectorPolicy("broad-policy", "Deployment", map[string]string{"app": "api"}, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{selectorPolicy}}

	workloadLabels := map[string]string{"app": "web"}
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", workloadLabels, "narrow-policy", 100)

	assert.Nil(t, result)
}

func TestCheckPolicyConflictInMemory_SelectorLowerWeightNoConflict(t *testing.T) {
	detector := NewDetector(testr.New(t))
	selectorPolicy := newSelectorPolicy("low-policy", "Deployment", map[string]string{"app": "web"}, 50)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{selectorPolicy}}

	workloadLabels := map[string]string{"app": "web"}
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", workloadLabels, "high-policy", 100)

	assert.Nil(t, result)
}

func TestCheckPolicyConflictInMemory_TwoSelectorPoliciesBothMatch(t *testing.T) {
	detector := NewDetector(testr.New(t))
	p1 := newSelectorPolicy("low-sel", "Deployment", map[string]string{"app": "web"}, 50)
	p2 := newSelectorPolicy("high-sel", "Deployment", map[string]string{"team": "platform"}, 300)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p1, p2}}

	workloadLabels := map[string]string{"app": "web", "team": "platform"}
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment", workloadLabels, "current", 100)

	assert.NotNil(t, result)
	assert.Equal(t, "high-sel", result.Name)
}

func newExpressionPolicy(name, targetKind string, matchLabels map[string]string, expressions []interface{}, weight int64) unstructured.Unstructured {
	p := unstructured.Unstructured{}
	p.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "attune.io", Version: "v1alpha1", Kind: "AttunePolicy",
	})
	p.SetName(name)
	_ = unstructured.SetNestedField(p.Object, targetKind, "spec", "targetRef", "kind")
	_ = unstructured.SetNestedField(p.Object, weight, "spec", "weight")
	if matchLabels != nil {
		ml := make(map[string]interface{}, len(matchLabels))
		for k, v := range matchLabels {
			ml[k] = v
		}
		_ = unstructured.SetNestedField(p.Object, ml, "spec", "targetRef", "selector", "matchLabels")
	}
	if expressions != nil {
		_ = unstructured.SetNestedSlice(p.Object, expressions, "spec", "targetRef", "selector", "matchExpressions")
	}
	return p
}

func TestCheckPolicyConflictInMemory_MatchExpressionsIn(t *testing.T) {
	detector := NewDetector(testr.New(t))
	exprs := []interface{}{
		map[string]interface{}{
			"key":      "env",
			"operator": "In",
			"values":   []interface{}{"prod", "staging"},
		},
	}
	p := newExpressionPolicy("expr-policy", "Deployment", nil, exprs, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"env": "prod"}, "current", 100)
	assert.NotNil(t, result, "In expression matching 'prod' should detect conflict")
	assert.Equal(t, "expr-policy", result.Name)
}

func TestCheckPolicyConflictInMemory_MatchExpressionsInNoMatch(t *testing.T) {
	detector := NewDetector(testr.New(t))
	exprs := []interface{}{
		map[string]interface{}{
			"key":      "env",
			"operator": "In",
			"values":   []interface{}{"prod", "staging"},
		},
	}
	p := newExpressionPolicy("expr-policy", "Deployment", nil, exprs, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"env": "dev"}, "current", 100)
	assert.Nil(t, result, "In expression not matching 'dev' should not detect conflict")
}

func TestCheckPolicyConflictInMemory_MatchExpressionsExists(t *testing.T) {
	detector := NewDetector(testr.New(t))
	exprs := []interface{}{
		map[string]interface{}{
			"key":      "tier",
			"operator": "Exists",
		},
	}
	p := newExpressionPolicy("exists-policy", "Deployment", nil, exprs, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p}}

	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"tier": "frontend"}, "current", 100)
	assert.NotNil(t, result, "Exists expression should match when label is present")

	result = detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"app": "web"}, "current", 100)
	assert.Nil(t, result, "Exists expression should not match when label is absent")
}

func TestCheckPolicyConflictInMemory_CombinedMatchLabelsAndExpressions(t *testing.T) {
	detector := NewDetector(testr.New(t))
	exprs := []interface{}{
		map[string]interface{}{
			"key":      "env",
			"operator": "In",
			"values":   []interface{}{"prod"},
		},
	}
	p := newExpressionPolicy("combined-policy", "Deployment",
		map[string]string{"app": "web"}, exprs, 200)
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p}}

	// Both matchLabels and matchExpressions must match.
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"app": "web", "env": "prod"}, "current", 100)
	assert.NotNil(t, result, "both conditions met should detect conflict")

	// matchLabels match but expression doesn't.
	result = detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"app": "web", "env": "dev"}, "current", 100)
	assert.Nil(t, result, "expression not met should not detect conflict")

	// expression matches but matchLabels doesn't.
	result = detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"app": "api", "env": "prod"}, "current", 100)
	assert.Nil(t, result, "matchLabels not met should not detect conflict")
}

func TestCheckPolicyConflictInMemory_MalformedSelector(t *testing.T) {
	detector := NewDetector(testr.New(t))

	// Build a policy with an invalid matchExpressions operator so that
	// LabelSelectorAsSelector returns an error. The detector should log
	// the error and return no conflict (safe default).
	p := newSelectorPolicy("bad-selector", "Deployment", map[string]string{"app": "web"}, 200)
	exprs := []interface{}{
		map[string]interface{}{
			"key":      "zone",
			"operator": "InvalidOp",
			"values":   []interface{}{"us-east-1"},
		},
	}
	_ = unstructured.SetNestedSlice(p.Object, exprs, "spec", "targetRef", "selector", "matchExpressions")

	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{p}}
	result := detector.CheckPolicyConflictInMemory(list, "my-app", "Deployment",
		map[string]string{"app": "web", "zone": "us-east-1"}, "current", 100)
	assert.Nil(t, result, "malformed selector should be skipped, not treated as a match")
}

func TestFindMatchingHPA(t *testing.T) {
	detector := NewDetector(testr.New(t))

	hpas := []autoscalingv2.HorizontalPodAutoscaler{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "web",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "api",
				},
			},
		},
	}

	tests := []struct {
		name     string
		workload string
		kind     string
		wantName string
	}{
		{"match first", "web", "Deployment", "web-hpa"},
		{"match second", "api", "Deployment", "api-hpa"},
		{"wrong name", "db", "Deployment", ""},
		{"wrong kind", "web", "StatefulSet", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := detector.FindMatchingHPA(hpas, tc.workload, tc.kind)
			if tc.wantName == "" {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, tc.wantName, result.Name)
			}
		})
	}

	t.Run("empty list", func(t *testing.T) {
		assert.Nil(t, detector.FindMatchingHPA(nil, "web", "Deployment"))
	})
}
