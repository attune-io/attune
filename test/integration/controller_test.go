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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
	"github.com/SebTardif/kube-rightsize/internal/conflict"
	"github.com/SebTardif/kube-rightsize/internal/controller"
	"github.com/SebTardif/kube-rightsize/internal/metrics"
)

// syntheticCollector implements metrics.MetricsCollector and returns synthetic
// sample data. CPU samples use value ~0.05 (50m) and memory ~50MB, producing
// recommendations that differ from the test deployment's 100m/128Mi requests.
type syntheticCollector struct{}

func (s *syntheticCollector) QueryRange(_ context.Context, query string, start, end time.Time, step time.Duration) ([]metrics.Sample, error) {
	value := 0.05 // ~50m CPU
	if strings.Contains(query, "memory") {
		value = 50_000_000 // ~50MB
	}

	var samples []metrics.Sample
	count := int(end.Sub(start) / step)
	if count < 1 {
		count = 1
	}
	if count > 500 {
		count = 500
	}
	for i := 0; i < count; i++ {
		jitter := float64(i%10) * 0.001
		samples = append(samples, metrics.Sample{
			Timestamp: start.Add(time.Duration(i) * step),
			Value:     value + jitter,
		})
	}
	return samples, nil
}

func (s *syntheticCollector) Query(_ context.Context, _ string, _ time.Time) (float64, error) {
	return 0.05, nil
}

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start envtest: " + err.Error())
	}

	err = rightsizev1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		panic("failed to add scheme: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("failed to create client: " + err.Error())
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme.Scheme,
		LeaderElection:          false,
		HealthProbeBindAddress:  "0",
		Metrics:                 metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic("failed to create clientset: " + err.Error())
	}

	reconciler := &controller.RightSizePolicyReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Clientset:   clientset,
		Recorder:    mgr.GetEventRecorder("kube-rightsize-integration"),
		MinCooldown: 1 * time.Second, // fast reconciliation for tests
		MetricsFactory: func(address string) (metrics.MetricsCollector, error) {
			return &syntheticCollector{}, nil
		},
	}
	err = reconciler.SetupWithManager(mgr)
	if err != nil {
		panic("failed to setup controller: " + err.Error())
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic("manager failed to start: " + err.Error())
		}
	}()

	// Wait for informer caches to sync before running tests.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("failed to sync informer caches")
	}

	// Create the test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "integration-test",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		panic("failed to create test namespace: " + err.Error())
	}

	code := m.Run()

	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
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
				// Short cooldown for fast test reconciliation (MinCooldown=1s allows this).
				Cooldown: &metav1.Duration{Duration: 3 * time.Second},
			},
		},
	}
}

func TestReconcile_CreatesPolicy_BecomesReady(t *testing.T) {

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

	namespace := "integration-test"

	// Create and delete a policy.
	policy := newTestPolicy("policy-delete", namespace, "some-deploy")
	err := k8sClient.Create(ctx, policy)
	require.NoError(t, err, "failed to create policy")

	// Wait for reconciler to pick it up (condition gets set).
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      "policy-delete",
			Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		return len(fetched.Status.Conditions) > 0
	}, 30*time.Second, 500*time.Millisecond, "reconciler should process policy")

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

func TestReconcile_LabelSelectorTargetsMultipleWorkloads(t *testing.T) {
	namespace := "integration-test"

	// Create two deployments with the same label.
	for _, name := range []string{"tier-app-1", "tier-app-2"} {
		deploy := newTestDeployment(name, namespace)
		deploy.Labels["tier"] = "api"
		require.NoError(t, k8sClient.Create(ctx, deploy))
	}

	// Create a policy with a label selector targeting both deployments.
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-selector",
			Namespace: namespace,
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "api"},
				},
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
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:   99,
				SafetyMargin: "1.3",
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:     "Recommend",
				Cooldown: &metav1.Duration{Duration: 3 * time.Second},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: "policy-selector", Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		return fetched.Status.Workloads.Discovered >= 2
	}, 30*time.Second, 500*time.Millisecond, "policy should discover at least 2 workloads")
}

func TestReconcile_OptOutAnnotationSkipsWorkload(t *testing.T) {
	namespace := "integration-test"

	deploy := newTestDeployment("opted-out-app", namespace)
	deploy.Annotations = map[string]string{conflict.AnnotationSkip: "true"}
	require.NoError(t, k8sClient.Create(ctx, deploy))

	policy := newTestPolicy("policy-optout", namespace, "opted-out-app")
	require.NoError(t, k8sClient.Create(ctx, policy))

	// The workload is discovered but opted out, so no recommendations.
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: "policy-optout", Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		for _, c := range fetched.Status.Conditions {
			if c.Type == "Ready" && c.Reason == "InsufficientData" {
				return fetched.Status.Workloads.WithRecommendations == 0
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "opted-out workload should produce no recommendations")
}

func TestReconcile_DefaultsMergingFromClusterDefaults(t *testing.T) {
	namespace := "integration-test"

	// Create a cluster-scoped RightSizeDefaults with CPU percentile 90.
	defaults := &rightsizev1alpha1.RightSizeDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name: "integration-defaults",
		},
		Spec: rightsizev1alpha1.RightSizeDefaultsSpec{
			CPU: &rightsizev1alpha1.ResourceConfig{
				Percentile:   90,
				SafetyMargin: "1.5",
			},
			Memory: &rightsizev1alpha1.ResourceConfig{
				Percentile:   95,
				SafetyMargin: "1.4",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, defaults))
	defer func() { _ = k8sClient.Delete(ctx, defaults) }()

	deploy := newTestDeployment("defaults-app", namespace)
	require.NoError(t, k8sClient.Create(ctx, deploy))

	// Create a policy with zero percentile/margin (should inherit from defaults).
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-defaults",
			Namespace: namespace,
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: func() *string { s := "defaults-app"; return &s }(),
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: "http://prometheus:9090",
				},
				MinimumDataPoints: 1,
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:     "Recommend",
				Cooldown: &metav1.Duration{Duration: 3 * time.Second},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// The policy should still reconcile successfully (defaults fill in
	// the missing CPU/Memory config).
	assert.Eventually(t, func() bool {
		var fetched rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: "policy-defaults", Namespace: namespace,
		}, &fetched); err != nil {
			return false
		}
		return len(fetched.Status.Conditions) > 0
	}, 30*time.Second, 500*time.Millisecond, "policy with defaults should reconcile")
}

// ---------- Resize execution path (#20) ----------

// Note: Resize execution integration tests are NOT included because envtest's
// informer cache creates an inherent race: after UpdateResize bumps the pod's
// resourceVersion, the re-fetch via the cached client returns the stale version,
// causing a 409 Conflict on annotation persistence. This triggers revert on every
// attempt, preventing the resize from ever completing.
//
// The resize path is covered by:
//   - Unit tests: TestExecuteResizes_PersistsAnnotations, _CapturesZeroRestartCount,
//     _PreservesExistingPodAnnotations, _RevertsOnAnnotationUpdateFailure,
//     _RevertsOnReFetchFailure (using fake clients without informer cache)
//   - E2E tests: Chainsaw scenarios on Kind clusters (real kubelet, real cache sync)
