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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

func TestMergeDefaults_NoDefaults(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
			},
		},
	}

	defaults := r.fetchDefaults(context.Background(), "default")
	r.mergeDefaults(policy, defaults)

	// Nothing should change when no defaults exist.
	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.2", policy.Spec.CPU.SafetyMargin)
	assert.Equal(t, int32(99), policy.Spec.Memory.Percentile)
	assert.Equal(t, "1.3", policy.Spec.Memory.SafetyMargin)
}

func TestMergeDefaults_CPUPercentileMerged(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   0, // zero: should be filled from defaults
				SafetyMargin: "1.5",
			},
		},
	}

	fetchedDefaults := r.fetchDefaults(context.Background(), "default")
	r.mergeDefaults(policy, fetchedDefaults)

	assert.Equal(t, int32(95), policy.Spec.CPU.Percentile)
	// SafetyMargin was already set on the policy, so it stays.
	assert.Equal(t, "1.5", policy.Spec.CPU.SafetyMargin)
}

func TestMergeDefaults_SafetyMarginMerged(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "1.2",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "", // empty: should be filled from defaults
			},
		},
	}

	fetchedDefaults := r.fetchDefaults(context.Background(), "default")
	r.mergeDefaults(policy, fetchedDefaults)

	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.2", policy.Spec.CPU.SafetyMargin)
}

func TestMergeDefaults_PolicyTakesPrecedence(t *testing.T) {
	scheme := testScheme()
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.5",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaults).
		Build()

	r := &RightSizePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	policy := &rightsizev1alpha1.RightSizePolicy{
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "1.3",
			},
		},
	}

	fetchedDefaults := r.fetchDefaults(context.Background(), "default")
	r.mergeDefaults(policy, fetchedDefaults)

	// Policy values take precedence over defaults.
	assert.Equal(t, int32(90), policy.Spec.CPU.Percentile)
	assert.Equal(t, "1.3", policy.Spec.CPU.SafetyMargin)
}

func TestFetchDefaults_NamespaceScopedOverridesCluster(t *testing.T) {
	clusterDefaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 90},
		},
	}
	nsDefaults := &rightsizev1alpha1.RightSizeNamespaceDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "production-defaults", Namespace: "production"},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 99},
		},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterDefaults, nsDefaults).Build()
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	// Namespace with a RightSizeNamespaceDefaults should use it.
	result := r.fetchDefaults(context.Background(), "production")
	assert.NotNil(t, result)
	assert.Equal(t, int32(99), result.Spec.CPU.Percentile)

	// Namespace without RightSizeNamespaceDefaults falls back to cluster.
	result = r.fetchDefaults(context.Background(), "staging")
	assert.NotNil(t, result)
	assert.Equal(t, int32(90), result.Spec.CPU.Percentile)
}

func TestFetchDefaults_ListError(t *testing.T) {
	scheme := testScheme()
	errClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("simulated API server error")
			},
		}).Build()
	r := &RightSizePolicyReconciler{Client: errClient, Scheme: scheme}

	// Both namespace and cluster List calls fail; fetchDefaults returns nil.
	result := r.fetchDefaults(context.Background(), "default")
	assert.Nil(t, result, "fetchDefaults should return nil when List fails")
}

func TestFetchDefaults_UsesBoundedLists(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listOpts := &client.ListOptions{}
				for _, opt := range opts {
					opt.ApplyToList(listOpts)
				}
				assert.Equal(t, int64(1), listOpts.Limit, "fetchDefaults should bound list calls to a single item")
				return c.List(ctx, list, opts...)
			},
		}).
		WithObjects(&rightsizev1alpha1.RightSizeDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
			Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
				CPU: &rightsizev1alpha1.ResourceConfig{Percentile: 90},
			},
		}).
		Build()
	// No namespace defaults exist, so fetchDefaults will hit both list paths.
	r := &RightSizePolicyReconciler{Client: fakeClient, Scheme: scheme}

	result := r.fetchDefaults(context.Background(), "default")
	require.NotNil(t, result)
	assert.Equal(t, int32(90), result.Spec.CPU.Percentile)
}
