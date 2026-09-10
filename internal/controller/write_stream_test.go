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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func countResizeWrites(actions []k8stesting.Action) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetSubresource() == "resize" {
			n++
		}
	}
	return n
}

func TestExecuteResizes_ConvergedSecondPassNoResizeWrites(t *testing.T) {
	pod := newResizePod("api-server", "200m", "256Mi", "200m", "256Mi")
	deploy := newTestDeployment("api-server", "default", map[string]string{"app": "api-server"})
	r, _ := newResizeReconciler(pod, deploy)
	policy := newTestPolicy("p", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	recs := []attunev1alpha1.WorkloadRecommendation{
		newResizeRecommendation("api-server", "200m", "256Mi", "0", "0", "200m", "256Mi", "0", "0"),
	}

	count, _ := r.executeResizes(context.Background(), policy, []client.Object{deploy},
		recs, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)

	cs := r.Clientset.(*kubefake.Clientset)
	cs.ClearActions()
	count, _ = r.executeResizes(context.Background(), policy, []client.Object{deploy},
		recs, podMap("api-server", pod), nil, nil)
	assert.Equal(t, 0, count)
	assert.Equal(t, 0, countResizeWrites(cs.Actions()),
		"second pass on a converged pod must not UpdateResize")
}

func TestApplyTemplatePersistence_MatchingTemplateIssuesNoPatch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "nginx",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}

	patches := 0
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cw client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return cw.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewAttunePolicyReconciler()
	r.Client = cl
	r.Scheme = scheme

	policy := newTestPolicy("p", "default")
	memDec := true
	policy.Spec.Memory.AllowDecrease = &memDec
	policy.Spec.UpdateStrategy.TemplatePersistence = &attunev1alpha1.TemplatePersistence{
		Enabled: boolPtr(true),
		When:    attunev1alpha1.TemplatePersistenceOnRecommendation,
	}
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: "api",
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name: "app",
			Current: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("500m"),
				MemoryRequest: resource.MustParse("512Mi"),
			},
			Recommended: attunev1alpha1.ResourceValues{
				CPURequest:    resource.MustParse("200m"),
				MemoryRequest: resource.MustParse("256Mi"),
			},
		}},
	}}

	history := r.applyTemplatePersistence(context.Background(), policy, []client.Object{deploy}, recs,
		attunev1alpha1.TemplatePersistenceOnRecommendation, nil)
	assert.Empty(t, history)
	assert.Equal(t, 0, patches, "cached template already at want must not Patch")
}
