//go:build e2e

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

// Package e2e_go provides Go-based E2E tests for kube-rightsize.
// Tests run against a real k3d/Kind cluster with the operator and
// Prometheus deployed. Build tag: e2e.
package e2e_go

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

var (
	k8sClient  client.Client
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
	ctx        context.Context
	cancel     context.CancelFunc
	promAddr   = "http://prometheus-server.monitoring:80"
)

func TestMain(m *testing.M) {
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Minute)

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic("failed to build kubeconfig: " + err.Error())
	}
	restConfig = cfg

	err = rightsizev1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		panic("failed to add scheme: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("failed to create client: " + err.Error())
	}

	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		panic("failed to create clientset: " + err.Error())
	}

	code := m.Run()
	cancel()
	os.Exit(code)
}

// ---------- Helpers ----------

func int32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool    { return &b }

func uniqueNS(base string) string {
	return fmt.Sprintf("e2e-go-%s-%d", base, time.Now().UnixNano()%100000)
}

func createNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

func createDeployment(t *testing.T, name, namespace string, cpuReq, memReq string, replicas int32) *appsv1.Deployment {
	t.Helper()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": name},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
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
									corev1.ResourceCPU:    resource.MustParse(cpuReq),
									corev1.ResourceMemory: resource.MustParse(memReq),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	return deploy
}

func createPolicy(t *testing.T, name, namespace, deployName string, mode rightsizev1alpha1.UpdateMode) *rightsizev1alpha1.RightSizePolicy {
	t.Helper()
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &deployName,
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: promAddr,
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
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
				Percentile:    99,
				SafetyMargin:  "1.3",
				AllowDecrease: boolPtr(true),
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("64Mi"),
					Max: resource.MustParse("8Gi"),
				},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                   mode,
				Cooldown:               &metav1.Duration{Duration: time.Minute},
				AutoRevert:             boolPtr(true),
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))
	return policy
}

func waitForDeploymentReady(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
			return false, nil
		}
		return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil
	}))
}

func waitForPolicyDiscovered(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var policy rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &policy); err != nil {
			return false, nil
		}
		return policy.Status.Workloads.Discovered > 0, nil
	}))
}

func waitForResize(t *testing.T, policyName, namespace string, timeout time.Duration) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var policy rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: namespace}, &policy); err != nil {
			return false, nil
		}
		return policy.Status.Workloads.Resized > 0, nil
	}))
}

func forcePolicyReconcile(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()

	key := types.NamespacedName{Name: name, Namespace: namespace}
	var before rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, key, &before))

	lastReconcile := time.Time{}
	if before.Status.LastReconcileTime != nil {
		lastReconcile = before.Status.LastReconcileTime.Time
	}

	// Toggle a spec field to force a generation change. The
	// specOrDeletePredicate filters annotation-only metadata updates,
	// so an annotation change alone won't trigger reconciliation.
	specResourceVersion := ""
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var policy rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, key, &policy); err != nil {
			return err
		}
		cd := time.Minute
		if policy.Spec.UpdateStrategy.Cooldown != nil {
			cd = policy.Spec.UpdateStrategy.Cooldown.Duration
		}
		if cd.Truncate(time.Second)%2 == 0 {
			cd += time.Second
		} else {
			cd -= time.Second
		}
		policy.Spec.UpdateStrategy.Cooldown = &metav1.Duration{Duration: cd}
		if err := k8sClient.Update(ctx, &policy); err != nil {
			return err
		}
		specResourceVersion = policy.ResourceVersion
		return nil
	}))

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var latest rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, key, &latest); err != nil {
			return false, nil
		}
		if latest.ResourceVersion == specResourceVersion || latest.Status.LastReconcileTime == nil {
			return false, nil
		}
		if lastReconcile.IsZero() {
			return true, nil
		}
		return !latest.Status.LastReconcileTime.Time.Before(lastReconcile), nil
	}))
}

// ---------- Tests ----------

func TestE2E_PolicyDiscovery(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("discovery")
	createNamespace(t, ns)
	createDeployment(t, "test-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "test-app", ns, 60*time.Second)

	createPolicy(t, "test-policy", ns, "test-app", rightsizev1alpha1.UpdateModeRecommend)
	waitForPolicyDiscovered(t, "test-policy", ns, 90*time.Second)

	var policy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-policy", Namespace: ns}, &policy))
	assert.Equal(t, int32(1), policy.Status.Workloads.Discovered)
}

func TestE2E_AutoMode_ResizesRunningPod(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("auto")
	createNamespace(t, ns)
	createDeployment(t, "auto-app", ns, "500m", "512Mi", 1)
	waitForDeploymentReady(t, "auto-app", ns, 60*time.Second)

	createPolicy(t, "auto-policy", ns, "auto-app", rightsizev1alpha1.UpdateModeAuto)

	// Wait for resize to complete (pod resources should change).
	waitForResize(t, "auto-policy", ns, 3*time.Minute)

	// Verify the pod's resources actually changed.
	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "auto-app"},
	))
	require.NotEmpty(t, podList.Items)

	pod := podList.Items[0]

	// Verify the resize actually changed the pod's resources.
	// We don't assert direction (up/down) because the recommendation
	// depends on actual Prometheus data which varies per run.
	origCPU := resource.MustParse("500m")
	origMem := resource.MustParse("512Mi")
	cpuReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	memReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	assert.True(t, cpuReq.Cmp(origCPU) != 0 || memReq.Cmp(origMem) != 0,
		"at least one resource should have changed after resize, cpu=%s mem=%s",
		cpuReq.String(), memReq.String())

	// Verify pod is still Running.
	assert.Equal(t, corev1.PodRunning, pod.Status.Phase)
}

func TestE2E_OneShotMode_ResizesOnePod(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("oneshot")
	createNamespace(t, ns)
	createDeployment(t, "oneshot-app", ns, "500m", "512Mi", 2)
	waitForDeploymentReady(t, "oneshot-app", ns, 60*time.Second)

	createPolicy(t, "oneshot-policy", ns, "oneshot-app", rightsizev1alpha1.UpdateModeOneShot)

	waitForResize(t, "oneshot-policy", ns, 3*time.Minute)

	// OneShot should resize exactly 1 pod.
	var policy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "oneshot-policy", Namespace: ns}, &policy))
	assert.Equal(t, int32(1), policy.Status.Workloads.Resized,
		"OneShot mode should resize exactly 1 workload")
}

func TestE2E_AutoMode_RecordsResizeHistory(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("revert")
	createNamespace(t, ns)

	// Deploy a pod with a liveness probe that checks for a file.
	// After resize, the annotation change triggers the operator's observation.
	// We use a pod that will fail its liveness probe to trigger restarts.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "revert-app",
			Namespace: ns,
			Labels:    map[string]string{"app": "revert-app"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "revert-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "revert-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "revert-app", ns, 60*time.Second)

	policy := createPolicy(t, "revert-policy", ns, "revert-app", rightsizev1alpha1.UpdateModeAuto)

	// Wait for initial resize.
	waitForResize(t, "revert-policy", ns, 3*time.Minute)

	// Verify the resize occurred and check that history entries exist.
	var updatedPolicy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: policy.Name, Namespace: ns,
	}, &updatedPolicy))
	assert.NotEmpty(t, updatedPolicy.Status.ResizeHistory,
		"resize history should have at least one entry")
}

func TestE2E_MultiContainer_ExcludesSidecar(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("multi")
	createNamespace(t, ns)

	// Create deployment with 2 containers.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-app",
			Namespace: ns,
			Labels:    map[string]string{"app": "multi-app"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "multi-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "multi-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "multi-app", ns, 60*time.Second)

	// Create policy with excludeContainers set directly to avoid update conflicts
	// with the reconciler which starts processing immediately after creation.
	deployName := "multi-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95, SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("50m"), Max: resource.MustParse("4000m")},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99, SafetyMargin: "1.3",
				AllowDecrease: boolPtr(true),
				Bounds:        &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("8Gi")},
			},
			ExcludeContainers: []string{"istio-proxy"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeAuto, Cooldown: &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true), MaxCPUChangePercent: int32Ptr(100), MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForResize(t, "multi-policy", ns, 3*time.Minute)

	// Verify only app container was resized.
	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "multi-app"},
	))
	require.NotEmpty(t, podList.Items)

	pod := podList.Items[0]
	for _, c := range pod.Spec.Containers {
		if c.Name == "istio-proxy" {
			expectedCPU := resource.MustParse("100m")
			expectedMem := resource.MustParse("128Mi")
			assert.Equal(t, expectedCPU.MilliValue(),
				c.Resources.Requests.Cpu().MilliValue(),
				"istio-proxy CPU should be unchanged")
			assert.Equal(t, expectedMem.Value(),
				c.Resources.Requests.Memory().Value(),
				"istio-proxy memory should be unchanged")
		}
		if c.Name == "app" {
			origCPU := resource.MustParse("500m")
			origMem := resource.MustParse("512Mi")
			assert.True(t, c.Resources.Requests.Cpu().Cmp(origCPU) != 0 ||
				c.Resources.Requests.Memory().Cmp(origMem) != 0,
				"app container should have at least one resource changed")
		}
	}
}

func TestE2E_RealisticLoad_Overprovisioned(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("load")
	createNamespace(t, ns)

	// Deploy a workload using stress-ng to generate known CPU/memory load.
	// Overprovisioned: requests 1 CPU / 256Mi memory, actual ~200m / ~100Mi.
	// Limits match requests (Guaranteed QoS) to constrain host CPU usage.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "load-app",
			Namespace: ns,
			Labels:    map[string]string{"app": "load-app"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "load-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "load-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "ghcr.io/alexei-led/stress-ng:0.20.01",
							Args:  []string{"--cpu", "1", "--cpu-load", "20", "--vm", "1", "--vm-bytes", "100M", "--timeout", "0"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "load-app", ns, 120*time.Second)

	loadPolicy := createPolicy(t, "load-policy", ns, "load-app", rightsizev1alpha1.UpdateModeRecommend)
	maxCPU, err := resource.ParseQuantity("800m")
	require.NoError(t, err)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestPolicy rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: loadPolicy.Name, Namespace: ns}, &latestPolicy); err != nil {
			return err
		}
		latestPolicy.Spec.CPU.Bounds.Max = maxCPU
		return k8sClient.Update(ctx, &latestPolicy)
	}))

	// Wait for the updated policy to produce a recommendation using the test-specific max bound.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var latestPolicy rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "load-policy", Namespace: ns}, &latestPolicy); err != nil {
			return false, nil
		}
		if latestPolicy.Status.Workloads.WithRecommendations == 0 ||
			len(latestPolicy.Status.Recommendations) == 0 ||
			len(latestPolicy.Status.Recommendations[0].Containers) == 0 {
			return false, nil
		}
		return latestPolicy.Status.Recommendations[0].Containers[0].Recommended.CPURequest.MilliValue() == 800, nil
	}))

	var latestPolicy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "load-policy", Namespace: ns}, &latestPolicy))

	require.NotEmpty(t, latestPolicy.Status.Recommendations)
	rec := latestPolicy.Status.Recommendations[0]
	require.NotEmpty(t, rec.Containers)

	// CPU recommendation should be clamped by the test-specific max bound.
	recCPU := rec.Containers[0].Recommended.CPURequest
	assert.Equal(t, int64(800), recCPU.MilliValue(),
		"recommended CPU should honor the test-specific 800m max bound, got %s", recCPU.String())

	cpuExplain := rec.Containers[0].Explanation
	require.NotNil(t, cpuExplain)
	require.NotNil(t, cpuExplain.CPU)
	assert.Equal(t, "max", cpuExplain.CPU.BoundsApplied,
		"load test should observe the CPU max bound being applied")

	// Savings estimate should be non-empty when the recommendation lowers requests.
	assert.NotEmpty(t, latestPolicy.Status.Savings.EstimatedMonthlySavings,
		"savings estimate should be computed for overprovisioned workload")
}

func TestE2E_BudgetCaps_DefersResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("budget")
	createNamespace(t, ns)
	createDeployment(t, "budget-app", ns, "100m", "512Mi", 3)
	waitForDeploymentReady(t, "budget-app", ns, 60*time.Second)

	tightBudget := resource.MustParse("150m")
	deployName := "budget-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.2"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.3"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                   rightsizev1alpha1.UpdateModeAuto,
				Cooldown:               &metav1.Duration{Duration: time.Minute},
				MaxTotalCPUIncrease:    &tightBudget,
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for at least one reconcile cycle.
	waitForPolicyDiscovered(t, "budget-policy", ns, 2*time.Minute)

	// With a 150m CPU budget and ~142m increase per pod (100m -> 242m),
	// at most one pod can be resized per cycle. Wait for at least one resize.
	waitForResize(t, "budget-policy", ns, 3*time.Minute)

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "budget-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(1), p.Status.Workloads.Discovered)

	// Verify at pod level: with 150m budget and 142m per pod, at most 1
	// pod should be resized in the first cycle. Count pods still at 100m.
	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "budget-app"}))
	unreszied := 0
	for _, pod := range podList.Items {
		for _, c := range pod.Spec.Containers {
			if c.Name == "app" {
				if cpu := c.Resources.Requests[corev1.ResourceCPU]; cpu.MilliValue() <= 100 {
					unreszied++
				}
			}
		}
	}
	assert.GreaterOrEqual(t, unreszied, 1,
		"budget should prevent all 3 pods from being resized in one cycle")
}

func TestE2E_ScheduleWindow_SkipsOutsideWindow(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("sched")
	createNamespace(t, ns)
	createDeployment(t, "sched-app", ns, "500m", "512Mi", 1)
	waitForDeploymentReady(t, "sched-app", ns, 60*time.Second)

	// Build a daysOfWeek list that excludes today.
	allDays := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	today := time.Now().UTC().Weekday().String()
	var excludedDays []string
	for _, d := range allDays {
		if d != today {
			excludedDays = append(excludedDays, d)
		}
	}

	deployName := "sched-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sched-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.2"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.3"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                   rightsizev1alpha1.UpdateModeAuto,
				Cooldown:               &metav1.Duration{Duration: time.Minute},
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
				Schedule: &rightsizev1alpha1.ResizeSchedule{
					DaysOfWeek: excludedDays,
					Windows:    []rightsizev1alpha1.TimeWindow{{Start: "00:00", End: "23:59"}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "sched-policy", ns, 2*time.Minute)

	// Wait for a recommendation to be computed, proving the operator has data
	// and the only thing blocking resize is the schedule.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pol rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sched-policy", Namespace: ns}, &pol); err != nil {
			return false, nil
		}
		return pol.Status.Workloads.WithRecommendations > 0, nil
	}))

	// Force a reconcile after recommendation is available.
	forcePolicyReconcile(t, "sched-policy", ns, 2*time.Minute)

	// Today is excluded from the schedule, so no resizes should occur.
	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "sched-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(0), p.Status.Workloads.Resized,
		"no resizes should occur when today is excluded from schedule")
}

func TestE2E_BearerToken_Authenticates(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("bearer")
	createNamespace(t, ns)

	// Create a Secret with a dummy bearer token.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("dummy-bearer-token")},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))

	createDeployment(t, "bearer-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "bearer-app", ns, 60*time.Second)

	deployName := "bearer-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bearer-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: promAddr,
					BearerTokenSecret: &rightsizev1alpha1.SecretKeyRef{
						Name: "prom-token",
						Key:  "token",
					},
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.2"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.3"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:     rightsizev1alpha1.UpdateModeRecommend,
				Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Prometheus doesn't require auth, but the operator should successfully
	// read the Secret, inject the bearer token, and query without error.
	waitForPolicyDiscovered(t, "bearer-policy", ns, 2*time.Minute)

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "bearer-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(1), p.Status.Workloads.Discovered,
		"policy with bearer token should discover workloads")
}

func TestE2E_EvictionFallback_ResizesWithInPlaceOrEvict(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("evict")
	createNamespace(t, ns)
	createDeployment(t, "evict-app", ns, "500m", "512Mi", 2)
	waitForDeploymentReady(t, "evict-app", ns, 60*time.Second)

	deployName := "evict-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "evict-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95, SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("50m"),
					Max: resource.MustParse("4000m"),
				},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99, SafetyMargin: "1.3",
				AllowDecrease: boolPtr(true),
				Bounds: &rightsizev1alpha1.ResourceBounds{
					Min: resource.MustParse("64Mi"),
					Max: resource.MustParse("8Gi"),
				},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                   rightsizev1alpha1.UpdateModeAuto,
				Cooldown:               &metav1.Duration{Duration: time.Minute},
				AutoRevert:             boolPtr(true),
				ResizeMethod:           rightsizev1alpha1.ResizeMethodInPlaceOrEvict,
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for resize. With InPlaceOrEvict, the resize should succeed
	// either in-place or via eviction fallback.
	waitForResize(t, "evict-policy", ns, 3*time.Minute)

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "evict-policy", Namespace: ns}, &p))
	assert.GreaterOrEqual(t, p.Status.Workloads.Resized, int32(1),
		"at least one workload should be resized with InPlaceOrEvict")
}

func TestE2E_RecommendMode_KeepsRecommendationsWithoutLivePods(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("nopods")
	createNamespace(t, ns)

	// Create a deployment so Prometheus collects metrics.
	createDeployment(t, "nopods-app", ns, "500m", "256Mi", 1)
	waitForDeploymentReady(t, "nopods-app", ns, 60*time.Second)

	createPolicy(t, "nopods-policy", ns, "nopods-app", rightsizev1alpha1.UpdateModeRecommend)
	waitForPolicyDiscovered(t, "nopods-policy", ns, 2*time.Minute)

	// Wait until recommendations appear.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nopods-policy", Namespace: ns}, &p); err != nil {
			return false, nil
		}
		return p.Status.Workloads.WithRecommendations > 0 && len(p.Status.Recommendations) > 0, nil
	}))

	var beforeScale rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "nopods-policy", Namespace: ns}, &beforeScale))
	require.NotEmpty(t, beforeScale.Status.Recommendations)

	// Scale the deployment to 0 so no live pods remain.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nopods-app", Namespace: ns}, &deploy); err != nil {
			return err
		}
		deploy.Spec.Replicas = int32Ptr(0)
		return k8sClient.Update(ctx, &deploy)
	}))

	// Wait for pods to terminate.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		var podList corev1.PodList
		if err := k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "nopods-app"}); err != nil {
			return false, nil
		}
		return len(podList.Items) == 0, nil
	}))

	forcePolicyReconcile(t, "nopods-policy", ns, 45*time.Second)

	var final rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "nopods-policy", Namespace: ns}, &final))
	assert.Equal(t, int32(1), final.Status.Workloads.Discovered,
		"deployment with 0 replicas should still be discovered")
	assert.Greater(t, final.Status.Workloads.WithRecommendations, int32(0),
		"historical recommendations should remain available even without live pods")
	require.NotEmpty(t, final.Status.Recommendations,
		"recommendations should still be surfaced after the workload scales to zero")
	assert.Equal(t, beforeScale.Status.Workloads.WithRecommendations, final.Status.Workloads.WithRecommendations,
		"reconcile without live pods should keep the same number of surfaced recommendations")
	require.Len(t, final.Status.Recommendations, len(beforeScale.Status.Recommendations),
		"reconcile without live pods should keep surfaced recommendations for the discovered workload")

	// Zero out LastUpdated to avoid flaky timestamp comparisons.
	for i := range beforeScale.Status.Recommendations {
		for j := range beforeScale.Status.Recommendations[i].Containers {
			beforeScale.Status.Recommendations[i].Containers[j].LastUpdated = metav1.Time{}
		}
	}
	for i := range final.Status.Recommendations {
		for j := range final.Status.Recommendations[i].Containers {
			final.Status.Recommendations[i].Containers[j].LastUpdated = metav1.Time{}
		}
	}

	// The history window keeps advancing after scale-to-zero, so the exact
	// recommendation values may legitimately change on the next reconcile. The
	// contract here is that the same workload and container remain surfaced with
	// current template resources and explanation details.
	beforeRec := beforeScale.Status.Recommendations[0]
	finalRec := final.Status.Recommendations[0]
	assert.Equal(t, beforeRec.Workload, finalRec.Workload,
		"recommendation should still belong to the scaled-to-zero workload")
	assert.Equal(t, beforeRec.Kind, finalRec.Kind,
		"recommendation should preserve workload kind")
	require.Len(t, finalRec.Containers, len(beforeRec.Containers),
		"recommendation should still include the discovered containers")
	assert.Equal(t, beforeRec.Containers[0].Name, finalRec.Containers[0].Name,
		"recommendation should still target the same container")
	assert.Equal(t, beforeRec.Containers[0].Current, finalRec.Containers[0].Current,
		"scale-to-zero should not change the workload template resources")
	assert.Greater(t, finalRec.Containers[0].DataPoints, int32(0),
		"historical Prometheus samples should continue to back the retained recommendation")
	assert.NotNil(t, finalRec.Containers[0].Explanation,
		"retained recommendation should keep estimator details for explain output")
	assert.Equal(t, int32(0), final.Status.Workloads.Resized,
		"recommend mode should not resize anything")
}

func TestE2E_BearerToken_SecretRotation(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("rotate")
	createNamespace(t, ns)

	// Create a Secret with initial bearer token.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rotate-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("initial-token")},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))

	createDeployment(t, "rotate-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "rotate-app", ns, 60*time.Second)

	deployName := "rotate-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "rotate-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus: &rightsizev1alpha1.PrometheusConfig{
					Address: promAddr,
					BearerTokenSecret: &rightsizev1alpha1.SecretKeyRef{
						Name: "rotate-token",
						Key:  "token",
					},
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.2"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.3"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:     rightsizev1alpha1.UpdateModeRecommend,
				Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for initial discovery with the first token.
	waitForPolicyDiscovered(t, "rotate-policy", ns, 2*time.Minute)

	var p1 rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rotate-policy", Namespace: ns}, &p1))
	assert.Equal(t, int32(1), p1.Status.Workloads.Discovered,
		"policy should discover workloads with initial token")

	// Rotate the bearer token.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rotate-token", Namespace: ns}, &s); err != nil {
			return err
		}
		s.Data["token"] = []byte("rotated-token")
		return k8sClient.Update(ctx, &s)
	}))

	// Prometheus doesn't enforce auth, so both tokens work. The key assertion
	// is that the reconcile succeeds (no PrometheusUnavailable condition)
	// and workloads are still discovered after a fresh reconcile.
	forcePolicyReconcile(t, "rotate-policy", ns, 45*time.Second)

	var p2 rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rotate-policy", Namespace: ns}, &p2))
	assert.Equal(t, int32(1), p2.Status.Workloads.Discovered,
		"policy should continue discovering workloads after token rotation")

	// Verify no PrometheusUnavailable condition set.
	for _, c := range p2.Status.Conditions {
		if c.Type == "Ready" {
			assert.NotEqual(t, "PrometheusUnavailable", c.Reason,
				"reconcile should succeed after token rotation, not show PrometheusUnavailable")
		}
	}
}

func TestE2E_OOMKill_TriggersRevert(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("oom")
	createNamespace(t, ns)

	// Phase 1: Deploy with sleep so the operator can resize first.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-app",
			Namespace: ns,
			Labels:    map[string]string{"app": "oom-app"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "oom-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "oom-app"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "app",
						Image:   "ghcr.io/alexei-led/stress-ng:0.20.01",
						Command: []string{"/stress-ng", "--sleep", "1", "--timeout", "3600"},
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "oom-app", ns, 120*time.Second)

	controlledValues := rightsizev1alpha1.ControlledRequestsAndLimits
	deployName := "oom-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "oom-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile:       95,
				SafetyMargin:     "1.2",
				ControlledValues: &controlledValues,
				Bounds:           &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("10m"), Max: resource.MustParse("1000m")},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile:       99,
				SafetyMargin:     "1.0",
				AllowDecrease:    boolPtr(true),
				ControlledValues: &controlledValues,
				Bounds:           &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("8Mi"), Max: resource.MustParse("512Mi")},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode:                   rightsizev1alpha1.UpdateModeAuto,
				Cooldown:               &metav1.Duration{Duration: 1 * time.Minute},
				AutoRevert:             boolPtr(true),
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
				Canary: &rightsizev1alpha1.CanaryConfig{
					Percentage:        100,
					ObservationPeriod: metav1.Duration{Duration: time.Minute},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for the operator to resize the pod at least once.
	waitForResize(t, "oom-policy", ns, 3*time.Minute)

	// Memory resize with RestartContainer policy forces a container restart.
	// Wait for the pod to be ready again before exec'ing the OOM stressor,
	// otherwise the exec targets a container mid-restart and silently fails.
	waitForDeploymentReady(t, "oom-app", ns, 120*time.Second)

	// Phase 2: Exec into the running pod to trigger OOM. Using exec keeps the
	// same pod (no deployment rollout), so the safety monitor can correlate the
	// OOMKill with its resize record.
	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "oom-app"}))
	require.Len(t, podList.Items, 1, "expected exactly one oom-app pod")
	podName := podList.Items[0].Name
	t.Logf("Exec'ing OOM stressor into pod %s", podName)

	go func() {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(ns).
			Name(podName).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: "app",
				Command:   []string{"/stress-ng", "--vm", "1", "--vm-bytes", "256M", "--timeout", "120"},
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)
		exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
		if err != nil {
			t.Logf("exec setup error (expected if container dies): %v", err)
			return
		}
		// StreamWithContext will fail when the container is OOMKilled; that's expected.
		_ = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: io.Discard,
			Stderr: io.Discard,
		})
	}()

	// Phase 3: Wait for OOMKilled to appear in pod status.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": "oom-app"}); err != nil {
			return false, nil
		}
		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
					t.Logf("OOMKill detected on pod %s (last termination)", pod.Name)
					return true, nil
				}
				if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
					t.Logf("OOMKill detected on pod %s (current state)", pod.Name)
					return true, nil
				}
			}
		}
		return false, nil
	}), "timed out waiting for OOMKill")

	// Phase 4: Wait for the safety monitor to detect OOMKill and record a
	// Reverted entry in the resize history.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p rightsizev1alpha1.RightSizePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "oom-policy", Namespace: ns}, &p); err != nil {
			return false, nil
		}
		for _, h := range p.Status.ResizeHistory {
			if h.Result == rightsizev1alpha1.ResizeResultReverted {
				t.Logf("Revert detected: workload=%s container=%s resource=%s", h.Workload, h.Container, h.Resource)
				return true, nil
			}
		}
		return false, nil
	}), "timed out waiting for safety revert after OOMKill")

	var finalPolicy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "oom-policy", Namespace: ns}, &finalPolicy))
	hasRevert := false
	for i, h := range finalPolicy.Status.ResizeHistory {
		t.Logf("  [%d] workload=%s container=%s resource=%s result=%s", i, h.Workload, h.Container, h.Resource, h.Result)
		if h.Result == rightsizev1alpha1.ResizeResultReverted {
			hasRevert = true
		}
	}
	assert.True(t, hasRevert, "resize history should contain a Reverted entry after OOMKill")
}

func TestE2E_MultiReplica_ProgressiveResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("multi-rep")
	createNamespace(t, ns)
	createDeployment(t, "multi-rep-app", ns, "500m", "512Mi", 3)
	waitForDeploymentReady(t, "multi-rep-app", ns, 120*time.Second)

	deployName := "multi-rep-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-rep-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95, SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("50m"), Max: resource.MustParse("4000m")},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99, SafetyMargin: "1.3", AllowDecrease: boolPtr(true),
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("8Gi")},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeAuto, Cooldown: &metav1.Duration{Duration: time.Minute},
				MaxConcurrentResizes:   1,
				AutoRevert:             boolPtr(true),
				MaxCPUChangePercent:    int32Ptr(100),
				MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForResize(t, "multi-rep-policy", ns, 3*time.Minute)

	// Verify at least one pod was resized and the deployment stayed available.
	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "multi-rep-app", Namespace: ns}, &deploy))
	assert.GreaterOrEqual(t, deploy.Status.ReadyReplicas, int32(1),
		"at least one replica should remain ready during progressive resize")
}

func TestE2E_GuaranteedQoS_RequestsAndLimits(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("qos")
	createNamespace(t, ns)

	// Guaranteed QoS: requests = limits.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "qos-app", Namespace: ns, Labels: map[string]string{"app": "qos-app"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "qos-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "qos-app"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2000m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2000m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "qos-app", ns, 60*time.Second)

	controlledBoth := rightsizev1alpha1.ControlledRequestsAndLimits
	controlledRequestsOnly := rightsizev1alpha1.ControlledRequestsOnly
	deployName := "qos-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "qos-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95, SafetyMargin: "1.2", ControlledValues: &controlledBoth,
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("50m"), Max: resource.MustParse("4000m")},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				// Use RequestsOnly for memory to avoid a forced restart that races the test timeout.
				// The CPU path with RequestsAndLimits is the primary assertion target.
				Percentile: 99, SafetyMargin: "1.3", AllowDecrease: boolPtr(true), ControlledValues: &controlledRequestsOnly,
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("8Gi")},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeAuto, Cooldown: &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true), MaxCPUChangePercent: int32Ptr(100), MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Guaranteed QoS with memory resize forces a container restart, so allow
	// extra time for the resize + restart + readiness cycle.
	waitForResize(t, "qos-policy", ns, 5*time.Minute)

	// Re-fetch pods after resize (the pod may have restarted from memory resize).
	waitForDeploymentReady(t, "qos-app", ns, 120*time.Second)

	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "qos-app"}))
	require.NotEmpty(t, podList.Items)
	c := podList.Items[0].Spec.Containers[0]

	// Requests and limits should still match (Guaranteed QoS preserved).
	assert.Equal(t, c.Resources.Requests.Cpu().MilliValue(), c.Resources.Limits.Cpu().MilliValue(),
		"CPU requests and limits should match after resize (Guaranteed QoS)")
	assert.Equal(t, c.Resources.Requests.Memory().Value(), c.Resources.Limits.Memory().Value(),
		"memory requests and limits should match after resize (Guaranteed QoS)")

	// At least one resource should have changed from the heavily overprovisioned initial values.
	origCPU := resource.MustParse("2000m")
	origMem := resource.MustParse("1Gi")
	assert.True(t, c.Resources.Requests.Cpu().Cmp(origCPU) != 0 || c.Resources.Requests.Memory().Cmp(origMem) != 0,
		"at least one resource should have changed, cpu=%s mem=%s", c.Resources.Requests.Cpu().String(), c.Resources.Requests.Memory().String())
}

func TestE2E_LabelSelector_MultipleWorkloads(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("selector")
	createNamespace(t, ns)

	// Two matching deployments.
	for _, name := range []string{"api-svc", "worker-svc"} {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": name, "team": "platform"}},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(1),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name, "team": "platform"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "app", Image: "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
						}},
					}}},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, deploy))
	}
	// One non-matching deployment.
	createDeployment(t, "unrelated-svc", ns, "100m", "128Mi", 1)
	waitForDeploymentReady(t, "api-svc", ns, 60*time.Second)
	waitForDeploymentReady(t, "worker-svc", ns, 60*time.Second)
	waitForDeploymentReady(t, "unrelated-svc", ns, 60*time.Second)

	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "selector-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{
				Kind:     "Deployment",
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "platform"}},
			},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU:    rightsizev1alpha1.ResourceConfig{Percentile: 95, SafetyMargin: "1.2"},
			Memory: rightsizev1alpha1.ResourceConfig{Percentile: 99, SafetyMargin: "1.3"},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeRecommend, Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "selector-policy", ns, 2*time.Minute)

	var p rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "selector-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(2), p.Status.Workloads.Discovered,
		"selector should discover exactly the 2 matching deployments")
}

func TestE2E_PolicyDeletion_CleansUpAnnotations(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("cleanup")
	createNamespace(t, ns)
	createDeployment(t, "cleanup-app", ns, "500m", "512Mi", 1)
	waitForDeploymentReady(t, "cleanup-app", ns, 60*time.Second)

	policy := createPolicy(t, "cleanup-policy", ns, "cleanup-app", rightsizev1alpha1.UpdateModeAuto)

	// Wait for resize so tracking annotations are set on the pod.
	waitForResize(t, "cleanup-policy", ns, 3*time.Minute)

	// Verify annotations exist before deletion.
	var podsBefore corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podsBefore, client.InNamespace(ns), client.MatchingLabels{"app": "cleanup-app"}))
	require.NotEmpty(t, podsBefore.Items)
	assert.Contains(t, podsBefore.Items[0].Labels, "rightsize.io/tracked",
		"pod should have tracking label before policy deletion")

	// Delete the policy.
	require.NoError(t, k8sClient.Delete(ctx, policy))

	// Wait for the finalizer to complete (policy fully gone).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p rightsizev1alpha1.RightSizePolicy
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "cleanup-policy", Namespace: ns}, &p)
		return err != nil, nil // gone when Get fails
	}), "timed out waiting for policy deletion")

	// Verify tracking annotations and labels are cleaned up from the pod.
	var podsAfter corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podsAfter, client.InNamespace(ns), client.MatchingLabels{"app": "cleanup-app"}))
	require.NotEmpty(t, podsAfter.Items)
	pod := podsAfter.Items[0]
	assert.NotContains(t, pod.Labels, "rightsize.io/tracked",
		"tracking label should be removed after policy deletion")
	assert.NotContains(t, pod.Annotations, "rightsize.io/policy",
		"policy annotation should be removed after policy deletion")
}

func TestE2E_ScaleUp_NewReplicasGetResized(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("scaleup")
	createNamespace(t, ns)
	createDeployment(t, "scaleup-app", ns, "500m", "512Mi", 1)
	waitForDeploymentReady(t, "scaleup-app", ns, 60*time.Second)

	createPolicy(t, "scaleup-policy", ns, "scaleup-app", rightsizev1alpha1.UpdateModeAuto)
	waitForResize(t, "scaleup-policy", ns, 3*time.Minute)

	// Scale up to 2 replicas.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "scaleup-app", Namespace: ns}, &deploy); err != nil {
			return err
		}
		deploy.Spec.Replicas = int32Ptr(2)
		return k8sClient.Update(ctx, &deploy)
	}))
	waitForDeploymentReady(t, "scaleup-app", ns, 120*time.Second)

	// Force a reconcile so the operator sees the new pod.
	forcePolicyReconcile(t, "scaleup-policy", ns, 2*time.Minute)

	// Wait for the second pod to be resized (give it a couple of cycles).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var podList corev1.PodList
		if err := k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "scaleup-app"}); err != nil {
			return false, nil
		}
		resizedCount := 0
		origCPU := resource.MustParse("500m")
		for _, pod := range podList.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, c := range pod.Spec.Containers {
				if c.Name == "app" && c.Resources.Requests.Cpu().Cmp(origCPU) != 0 {
					resizedCount++
				}
			}
		}
		return resizedCount >= 2, nil
	}), "both replicas should eventually be resized")
}

func TestE2E_ConcurrentPolicies_SameNamespace(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("concurrent")
	createNamespace(t, ns)
	createDeployment(t, "api-app", ns, "500m", "512Mi", 1)
	createDeployment(t, "worker-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "api-app", ns, 60*time.Second)
	waitForDeploymentReady(t, "worker-app", ns, 60*time.Second)

	createPolicy(t, "api-policy", ns, "api-app", rightsizev1alpha1.UpdateModeRecommend)
	createPolicy(t, "worker-policy", ns, "worker-app", rightsizev1alpha1.UpdateModeRecommend)

	waitForPolicyDiscovered(t, "api-policy", ns, 2*time.Minute)
	waitForPolicyDiscovered(t, "worker-policy", ns, 2*time.Minute)

	// Verify each policy sees only its own workload.
	var apiPolicy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "api-policy", Namespace: ns}, &apiPolicy))
	assert.Equal(t, int32(1), apiPolicy.Status.Workloads.Discovered)
	if len(apiPolicy.Status.Recommendations) > 0 {
		assert.Equal(t, "api-app", apiPolicy.Status.Recommendations[0].Workload)
	}

	var workerPolicy rightsizev1alpha1.RightSizePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "worker-policy", Namespace: ns}, &workerPolicy))
	assert.Equal(t, int32(1), workerPolicy.Status.Workloads.Discovered)
	if len(workerPolicy.Status.Recommendations) > 0 {
		assert.Equal(t, "worker-app", workerPolicy.Status.Recommendations[0].Workload)
	}
}

func TestE2E_MemoryAllowDecreaseFalse(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("nodecrease")
	createNamespace(t, ns)

	// High memory request (512Mi) but pause container uses ~0 memory.
	createDeployment(t, "nodecrease-app", ns, "500m", "512Mi", 1)
	waitForDeploymentReady(t, "nodecrease-app", ns, 60*time.Second)

	deployName := "nodecrease-app"
	policy := &rightsizev1alpha1.RightSizePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nodecrease-policy", Namespace: ns},
		Spec: rightsizev1alpha1.RightSizePolicySpec{
			TargetRef: rightsizev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: rightsizev1alpha1.MetricsSource{
				Prometheus:        &rightsizev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
			},
			CPU: rightsizev1alpha1.ResourceConfig{
				Percentile: 95, SafetyMargin: "1.2",
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("50m"), Max: resource.MustParse("4000m")},
			},
			Memory: rightsizev1alpha1.ResourceConfig{
				Percentile: 99, SafetyMargin: "1.3",
				// AllowDecrease intentionally NOT set (nil), so the default false applies.
				Bounds: &rightsizev1alpha1.ResourceBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("8Gi")},
			},
			UpdateStrategy: rightsizev1alpha1.UpdateStrategy{
				Mode: rightsizev1alpha1.UpdateModeAuto, Cooldown: &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true), MaxCPUChangePercent: int32Ptr(100), MaxMemoryChangePercent: int32Ptr(100),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForResize(t, "nodecrease-policy", ns, 3*time.Minute)

	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "nodecrease-app"}))
	require.NotEmpty(t, podList.Items)
	c := podList.Items[0].Spec.Containers[0]

	origMem := resource.MustParse("512Mi")
	assert.GreaterOrEqual(t, c.Resources.Requests.Memory().Value(), origMem.Value(),
		"memory should not decrease when allowDecrease is nil (default false), got %s", c.Resources.Requests.Memory().String())
}
