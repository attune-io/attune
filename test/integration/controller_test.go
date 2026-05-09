//go:build integration

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

package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/controller"
	"github.com/SebTardif/kube-rightsize/internal/metrics"
)

// stubCollector implements metrics.MetricsCollector with no-op behavior.
type stubCollector struct{}

func (s *stubCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]metrics.Sample, error) {
	return nil, nil
}

func (s *stubCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	return 0, nil
}

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func setupEnvtest(t *testing.T) {
	t.Helper()

	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to start envtest")
	require.NotNil(t, cfg)

	err = rightsizev1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err, "failed to add scheme")

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err, "failed to create client")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	require.NoError(t, err, "failed to create manager")

	reconciler := &controller.RightSizePolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		MetricsFactory: func(address string) (metrics.MetricsCollector, error) {
			return &stubCollector{}, nil
		},
	}
	err = reconciler.SetupWithManager(mgr)
	require.NoError(t, err, "failed to setup controller")

	go func() {
		err := mgr.Start(ctx)
		if err != nil {
			t.Logf("manager stopped: %v", err)
		}
	}()

	// Create the test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "integration-test",
		},
	}
	err = k8sClient.Create(ctx, ns)
	require.NoError(t, err, "failed to create test namespace")
}

func teardownEnvtest(t *testing.T) {
	t.Helper()
	cancel()
	err := testEnv.Stop()
	assert.NoError(t, err, "failed to stop envtest")
}

func int32Ptr(i int32) *int32 { return &i }

func newTestDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app": name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

func newTestPolicy(name, namespace, deploymentName string) *rightsizev1alpha1.RightSizePolicy {
	return &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &deploymentName,
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
				},
				MinimumDataPoints: 1,
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("50m"),
					Max: resource.MustParse("4000m"),
				},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("64Mi"),
					Max: resource.MustParse("8Gi"),
				},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: "Recommend",
			},
		},
	}
}

func TestReconcile_CreatesPolicy_BecomesReady(t *testing.T) {
	setupEnvtest(t)
	defer teardownEnvtest(t)

	namespace := "integration-test"

	// Create a Deployment.
	deploy := newTestDeployment("test-app-ready", namespace)
	err := k8sClient.Create(ctx, deploy)
	require.NoError(t, err, "failed to create deployment")

	// Create a RightSizePolicy targeting the Deployment.
	policy := newTestPolicy("policy-ready", namespace, "test-app-ready")
	err = k8sClient.Create(ctx, policy)
	require.NoError(t, err, "failed to create policy")

	// Eventually the policy status should have conditions set.
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      "policy-ready",
			Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		return len(fetched.Status.Conditions) > 0
	}, 30*time.Second, 500*time.Millisecond, "policy should have conditions set")
}

func TestReconcile_PolicyWithNoWorkloads_SetsInsufficientData(t *testing.T) {
	setupEnvtest(t)
	defer teardownEnvtest(t)

	namespace := "integration-test"

	// Create a policy targeting a non-existent Deployment.
	policy := newTestPolicy("policy-no-workloads", namespace, "nonexistent-deploy")
	err := k8sClient.Create(ctx, policy)
	require.NoError(t, err, "failed to create policy")

	// Status condition should be InsufficientData.
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      "policy-no-workloads",
			Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		for _, c := range fetched.Status.Conditions {
			if c.Type == "Ready" && c.Reason == "InsufficientData" {
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "policy should have InsufficientData condition")
}

func TestReconcile_DeletedPolicy_NoError(t *testing.T) {
	setupEnvtest(t)
	defer teardownEnvtest(t)

	namespace := "integration-test"

	// Create and delete a policy.
	policy := newTestPolicy("policy-delete", namespace, "some-deploy")
	err := k8sClient.Create(ctx, policy)
	require.NoError(t, err, "failed to create policy")

	// Wait briefly for reconciler to pick it up.
	time.Sleep(1 * time.Second)

	err = k8sClient.Delete(ctx, policy)
	require.NoError(t, err, "failed to delete policy")

	// Verify the policy is gone (no reconcile errors expected).
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      "policy-delete",
			Namespace: namespace,
		}, &fetched)
		return err != nil // should be NotFound
	}, 30*time.Second, 500*time.Millisecond, "policy should be deleted")
}
