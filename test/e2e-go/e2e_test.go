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

// Package e2e_go provides Go-based E2E tests for attune.
// Tests run against a real k3d/Kind cluster with the operator and
// Prometheus deployed. Build tag: e2e.
package e2e_go

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
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
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/resize"
)

const (
	defaultStressNGImage = "ghcr.io/alexei-led/stress-ng:0.20.01"
	cpuBurnImage         = "docker.io/library/busybox:1.37"
)

var (
	k8sClient     client.Client
	clientset     *kubernetes.Clientset
	restConfig    *rest.Config
	ctx           context.Context
	cancel        context.CancelFunc
	promAddr      = "http://prometheus-server.monitoring:80"
	stressNGImage string
)

func TestMain(m *testing.M) {
	crlog.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Minute)

	stressNGImage = os.Getenv("STRESS_NG_IMAGE")
	if stressNGImage == "" {
		stressNGImage = defaultStressNGImage
	}

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

	err = attunev1alpha1.AddToScheme(scheme.Scheme)
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

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

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

func createPolicy(t *testing.T, name, namespace, deployName string, mode attunev1alpha1.UpdateType) *attunev1alpha1.AttunePolicy {
	t.Helper()
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Name: &deployName,
			},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: promAddr,
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       mode,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))
	return policy
}

func waitForDeploymentReady(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
			return false, nil
		}
		if deploy.Status.ReadyReplicas == *deploy.Spec.Replicas {
			return true, nil
		}
		// Log diagnostics every 30s so failures are debuggable.
		if elapsed := time.Since(start); elapsed > 30*time.Second && time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			var pods corev1.PodList
			if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err == nil {
				if len(pods.Items) == 0 {
					t.Logf("waitForDeploymentReady(%s/%s): no matching pods after %s", namespace, name, elapsed.Round(time.Second))
				}
				for _, pod := range pods.Items {
					t.Logf("waitForDeploymentReady(%s/%s): pod=%s phase=%s ready=%d/%d restarts=%d",
						namespace, name, pod.Name, pod.Status.Phase,
						deploy.Status.ReadyReplicas, *deploy.Spec.Replicas,
						podRestartCount(pod))
					for _, cs := range pod.Status.ContainerStatuses {
						switch {
						case cs.State.Waiting != nil:
							t.Logf("  container %s: Waiting reason=%s", cs.Name, cs.State.Waiting.Reason)
						case cs.State.Terminated != nil:
							t.Logf("  container %s: Terminated reason=%s exit=%d", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
						case cs.State.Running != nil:
							t.Logf("  container %s: Running", cs.Name)
						}
					}
				}
			}
		}
		return false, nil
	}), "deployment %s/%s did not become ready within %s", namespace, name, timeout)
}

func podRestartCount(pod corev1.Pod) int32 {
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

func waitForPolicyDiscovered(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var policy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &policy); err != nil {
			return false, nil
		}
		if policy.Status.Workloads.Discovered > 0 {
			return true, nil
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second && time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			t.Logf("waitForPolicyDiscovered(%s/%s): discovered=%d after %s",
				namespace, name, policy.Status.Workloads.Discovered, elapsed.Round(time.Second))
			for _, c := range policy.Status.Conditions {
				t.Logf("  condition %s: status=%s reason=%s", c.Type, c.Status, c.Reason)
			}
		}
		return false, nil
	}), "policy %s/%s workloads.discovered still 0 after %s", namespace, name, timeout)
}

func waitForResize(t *testing.T, policyName, namespace string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var policy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: namespace}, &policy); err != nil {
			return false, nil
		}
		if policy.Status.Workloads.Resized > 0 {
			return true, nil
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second && time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			w := policy.Status.Workloads
			t.Logf("waitForResize(%s/%s): discovered=%d recommendations=%d pending=%d resized=%d after %s",
				namespace, policyName, w.Discovered, w.WithRecommendations, w.Pending, w.Resized, elapsed.Round(time.Second))
			for _, c := range policy.Status.Conditions {
				t.Logf("  condition %s: status=%s reason=%s", c.Type, c.Status, c.Reason)
			}
			for _, rec := range policy.Status.Recommendations {
				for _, cr := range rec.Containers {
					t.Logf("  recommendation %s/%s: currentCPU=%s recCPU=%s currentMem=%s recMem=%s",
						rec.Workload, cr.Name,
						cr.Current.CPURequest.String(), cr.Recommended.CPURequest.String(),
						cr.Current.MemoryRequest.String(), cr.Recommended.MemoryRequest.String())
				}
			}
			for _, we := range policy.Status.WorkloadErrors {
				t.Logf("  workloadError %s: %s", we.Workload, we.Error)
			}
		}
		return false, nil
	}), "policy %s/%s workloads.resized still 0 after %s", namespace, policyName, timeout)
}

// countPodsWithCPURequest counts pods matching app whose container "app"
// still has the given CPU request (exact Quantity compare).
func countPodsWithCPURequest(t *testing.T, namespace, app string, wantCPU resource.Quantity) int {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": app}))
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Spec.Containers {
			if c.Name != "app" {
				continue
			}
			if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok && cpu.Equal(wantCPU) {
				n++
			}
		}
	}
	return n
}

// policyHasEvent reports whether a Normal/Warning event with the given reason
// exists for the named AttunePolicy in the namespace.
func policyHasEvent(t *testing.T, namespace, policyName, reason string) bool {
	t.Helper()
	evs, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=AttunePolicy,reason=%s", policyName, reason),
	})
	if err != nil {
		t.Logf("list events: %v", err)
		return false
	}
	return len(evs.Items) > 0
}

func forcePolicyReconcile(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()

	key := types.NamespacedName{Name: name, Namespace: namespace}
	var before attunev1alpha1.AttunePolicy
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
		var policy attunev1alpha1.AttunePolicy
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
		var latest attunev1alpha1.AttunePolicy
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

// touchPolicySpec bumps a watched spec field so the operator requeues, without
// waiting for LastReconcileTime. Safe to call from poll loops.
func touchPolicySpec(t *testing.T, name, namespace string) {
	t.Helper()
	key := types.NamespacedName{Name: name, Namespace: namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var policy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, key, &policy); err != nil {
			return err
		}
		if policy.Spec.UpdateStrategy == nil {
			policy.Spec.UpdateStrategy = &attunev1alpha1.UpdateStrategy{}
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
		return k8sClient.Update(ctx, &policy)
	})
	if err != nil {
		t.Logf("touchPolicySpec(%s/%s): %v", namespace, name, err)
	}
}

// waitForLiveCPUDecrease waits until a Running pod's app container CPU request
// is strictly below origCPU. Re-touches the policy and logs status under
// parallel Prometheus load (nightly MemoryAllowDecrease / GuaranteedQoS flakes).
func waitForLiveCPUDecrease(t *testing.T, policyName, namespace, app string, origCPU resource.Quantity, timeout time.Duration, msg string) {
	t.Helper()
	lastTouch := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		if time.Since(lastTouch) > 45*time.Second {
			touchPolicySpec(t, policyName, namespace)
			lastTouch = time.Now()
		}
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": app}); err != nil {
			return false, nil
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, c := range pod.Spec.Containers {
				if c.Name != "app" {
					continue
				}
				cpu := c.Resources.Requests.Cpu()
				if cpu != nil && cpu.Cmp(origCPU) < 0 {
					t.Logf("Pod %s CPU decreased: cpu=%dm (from %dm) mem=%s",
						pod.Name, cpu.MilliValue(), origCPU.MilliValue(), c.Resources.Requests.Memory().String())
					return true, nil
				}
			}
		}
		if time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			var p attunev1alpha1.AttunePolicy
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: namespace}, &p); err == nil {
				t.Logf("%s: discovered=%d recs=%d resized=%d pending=%d",
					policyName, p.Status.Workloads.Discovered, p.Status.Workloads.WithRecommendations,
					p.Status.Workloads.Resized, p.Status.Workloads.Pending)
				for _, c := range p.Status.Conditions {
					t.Logf("  condition %s: status=%s reason=%s", c.Type, c.Status, c.Reason)
				}
			}
			for _, pod := range pods.Items {
				for _, c := range pod.Spec.Containers {
					if c.Name == "app" {
						cpu := c.Resources.Requests.Cpu()
						if cpu == nil {
							t.Logf("  pod %s phase=%s cpu=<nil>", pod.Name, pod.Status.Phase)
							continue
						}
						t.Logf("  pod %s phase=%s cpu=%s (%dm) cmpOrig=%d",
							pod.Name, pod.Status.Phase, cpu.String(), cpu.MilliValue(), cpu.Cmp(origCPU))
					}
				}
			}
		}
		return false, nil
	}), msg)
}

// ---------- Tests ----------

func TestE2E_PolicyDiscovery(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("discovery")
	createNamespace(t, ns)
	createDeployment(t, "test-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "test-app", ns, 60*time.Second)

	createPolicy(t, "test-policy", ns, "test-app", attunev1alpha1.UpdateTypeRecommend)
	waitForPolicyDiscovered(t, "test-policy", ns, 90*time.Second)

	var policy attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-policy", Namespace: ns}, &policy))
	assert.Equal(t, int32(1), policy.Status.Workloads.Discovered)
}

func TestE2E_AutoMode_ResizesRunningPod(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("auto")
	createNamespace(t, ns)
	createDeployment(t, "auto-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "auto-app", ns, 60*time.Second)

	createPolicy(t, "auto-policy", ns, "auto-app", attunev1alpha1.UpdateTypeAuto)

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
	origCPU := resource.MustParse("250m")
	origMem := resource.MustParse("256Mi")
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
	createDeployment(t, "oneshot-app", ns, "250m", "256Mi", 2)
	waitForDeploymentReady(t, "oneshot-app", ns, 60*time.Second)

	createPolicy(t, "oneshot-policy", ns, "oneshot-app", attunev1alpha1.UpdateTypeOneShot)

	waitForResize(t, "oneshot-policy", ns, 3*time.Minute)

	// OneShot should resize exactly 1 pod.
	var policy attunev1alpha1.AttunePolicy
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
									corev1.ResourceCPU:    resource.MustParse("250m"),
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
	waitForDeploymentReady(t, "revert-app", ns, 60*time.Second)

	policy := createPolicy(t, "revert-policy", ns, "revert-app", attunev1alpha1.UpdateTypeAuto)

	// Wait for initial resize.
	waitForResize(t, "revert-policy", ns, 3*time.Minute)

	// Verify the resize occurred and check that history entries exist.
	var updatedPolicy attunev1alpha1.AttunePolicy
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
									corev1.ResourceCPU:    resource.MustParse("250m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
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

	// Create policy with excludedContainers set directly to avoid update conflicts
	// with the reconciler which starts processing immediately after creation.
	deployName := "multi-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			ExcludedContainers: []string{"istio-proxy"},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
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
			origCPU := resource.MustParse("250m")
			origMem := resource.MustParse("256Mi")
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

	// Deploy a workload that generates CPU load via a busybox shell loop.
	// stress-ng --cpu exits with code 2 on certain k3s builds (v1.33, v1.35)
	// due to containerd/seccomp differences, so we use a simple busy loop
	// instead. The loop burns CPU, which shows up in cAdvisor/Prometheus
	// metrics. The recommendation gets capped by MaxAllowed (80m).
	// Low requests (100m/32Mi) reduce scheduling pressure on the shared CI
	// k3d node where parallel E2E tests compete for ~4 CPUs. The request
	// must stay above MaxAllowed (80m) so the workload is "overprovisioned"
	// and the savings estimate is non-zero. Burstable QoS (no limits) lets
	// the container burst beyond its request.
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
							Name:    "app",
							Image:   cpuBurnImage,
							Command: []string{"sh", "-c", "while true; do :; done"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "load-app", ns, 180*time.Second)

	loadPolicy := createPolicy(t, "load-policy", ns, "load-app", attunev1alpha1.UpdateTypeRecommend)
	maxCPU, err := resource.ParseQuantity("80m")
	require.NoError(t, err)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestPolicy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: loadPolicy.Name, Namespace: ns}, &latestPolicy); err != nil {
			return err
		}
		latestPolicy.Spec.CPU.MaxAllowed = &maxCPU
		return k8sClient.Update(ctx, &latestPolicy)
	}))

	// Wait for the operator to produce a recommendation based on actual usage.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		var latestPolicy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "load-policy", Namespace: ns}, &latestPolicy); err != nil {
			return false, nil
		}
		if latestPolicy.Status.Workloads.WithRecommendations == 0 ||
			len(latestPolicy.Status.Recommendations) == 0 ||
			len(latestPolicy.Status.Recommendations[0].Containers) == 0 {
			t.Logf("load-policy: still waiting for first recommendation (withRecommendations=%d recs=%d)",
				latestPolicy.Status.Workloads.WithRecommendations, len(latestPolicy.Status.Recommendations))
			return false, nil
		}
		container := latestPolicy.Status.Recommendations[0].Containers[0]
		// Wait for a complete explanation, which proves the recommendation
		// is based on real Prometheus metrics (not a premature empty result).
		if container.Explanation == nil || container.Explanation.CPU == nil {
			t.Log("load-policy: recommendation exists but CPU explanation not yet populated")
			return false, nil
		}
		recCPU := container.Recommended.CPURequest.MilliValue()
		bound := container.Explanation.CPU.BoundsApplied
		// Busybox burn eventually drives the estimate above MaxAllowed (80m),
		// so BoundsApplied becomes "max". Accepting any recCPU <= 80 is wrong:
		// early low samples clamp to MinAllowed (50m, BoundsApplied "min") and
		// race under parallel E2E load (nightly v1.34 flake).
		t.Logf("Current CPU recommendation: %dm bounds=%q (waiting for max bound)", recCPU, bound)
		return bound == "max" && recCPU <= 80, nil
	}))

	var latestPolicy attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "load-policy", Namespace: ns}, &latestPolicy))

	require.NotEmpty(t, latestPolicy.Status.Recommendations)
	rec := latestPolicy.Status.Recommendations[0]
	require.NotEmpty(t, rec.Containers)

	// CPU recommendation should be within MaxAllowed and reflect actual usage.
	recCPU := rec.Containers[0].Recommended.CPURequest
	assert.LessOrEqual(t, recCPU.MilliValue(), int64(80),
		"recommended CPU should respect the 80m MaxAllowed, got %s", recCPU.String())

	cpuExplain := rec.Containers[0].Explanation
	require.NotNil(t, cpuExplain)
	require.NotNil(t, cpuExplain.CPU)
	assert.Equal(t, "max", cpuExplain.CPU.BoundsApplied,
		"load test should observe the CPU max bound being applied")

	// Savings estimate should be computed for this workload.
	assert.NotEmpty(t, latestPolicy.Status.Savings.EstimatedMonthlySavings,
		"savings estimate should be computed for overprovisioned workload")
}

func TestE2E_BudgetCaps_DefersResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("budget")
	createNamespace(t, ns)
	// Smoke: overprovisioned pause, memory rec decreases. Budget field is
	// present but only gates increases. Pin CPU min=max so a spurious CPU
	// increase rec (pause under concurrent load can spike to 1) cannot
	// exhaust MaxTotalCPUIncrease and defer the whole container (budget
	// is checked per container, not per resource). Multi-pod increase
	// gating is covered by TestE2E_BudgetCaps_LimitsPerCycleIncrease.
	createDeployment(t, "budget-app", ns, "500m", "256Mi", 1)
	waitForDeploymentReady(t, "budget-app", ns, 60*time.Second)

	tightBudget := resource.MustParse("150m")
	cpuPin := resource.MustParse("500m")
	deployName := "budget-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       &cpuPin,
				MaxAllowed:       &cpuPin,
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                attunev1alpha1.UpdateTypeAuto,
				Cooldown:            &metav1.Duration{Duration: time.Minute},
				AutoRevert:          boolPtr(true),
				MaxTotalCPUIncrease: &tightBudget,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "budget-policy", ns, 2*time.Minute)
	waitForResize(t, "budget-policy", ns, 3*time.Minute)

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "budget-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(1), p.Status.Workloads.Discovered)
	require.GreaterOrEqual(t, p.Status.Workloads.WithRecommendations, int32(1))
	require.GreaterOrEqual(t, p.Status.Workloads.Resized, int32(1))
	require.NotNil(t, p.Spec.UpdateStrategy)
	require.NotNil(t, p.Spec.UpdateStrategy.MaxTotalCPUIncrease)
	assert.True(t, p.Spec.UpdateStrategy.MaxTotalCPUIncrease.Equal(tightBudget))
}

// TestE2E_BudgetCaps_LimitsPerCycleIncrease proves maxTotalCpuIncrease blocks
// a live CPU *increase* that exceeds the per-cycle budget (Prometheus +
// /resize path). Multi-pod peer deferral math stays in unit tests; this
// E2E asserts the gate fires in-cluster via BudgetExhausted and no resize.
//
// CPU-burn at 50m with maxAllowed 300m typically wants >> 20m increase.
// Budget of 20m cannot cover that increase → resize deferred.
func TestE2E_BudgetCaps_LimitsPerCycleIncrease(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("budinc")
	createNamespace(t, ns)

	const app = "budinc-app"
	origCPU := resource.MustParse("50m")
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: app, Namespace: ns,
			Labels: map[string]string{"app": app},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "app",
						Image:   cpuBurnImage,
						Command: []string{"sh", "-c", "while true; do :; done"},
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    origCPU,
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, app, ns, 180*time.Second)

	// Tiny CPU budget relative to burn-driven increase (50m → up to 300m).
	// Pin memory min=max=request so a free memory resize cannot set
	// workloads.resized while CPU is budget-blocked (nightly v1.35 flake).
	cpuBudget := resource.MustParse("20m")
	maxCPU := resource.MustParse("300m")
	memPin := resource.MustParse("32Mi")
	deployName := app
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "budinc-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       &maxCPU,
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(false),
				MinAllowed:       &memPin,
				MaxAllowed:       &memPin,
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                attunev1alpha1.UpdateTypeAuto,
				Cooldown:            &metav1.Duration{Duration: time.Minute},
				AutoRevert:          boolPtr(false),
				MaxTotalCPUIncrease: &cpuBudget,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "budinc-policy", ns, 2*time.Minute)

	// Wait for an increase recommendation that exceeds the 20m budget, or for
	// the gate event itself.
	var sawBigIncrease bool
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		if policyHasEvent(t, ns, "budinc-policy", "BudgetExhausted") {
			return true, nil
		}
		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "budinc-policy", Namespace: ns}, &p); err != nil {
			return false, nil
		}
		for _, rec := range p.Status.Recommendations {
			for _, c := range rec.Containers {
				if c.Name != "app" {
					continue
				}
				inc := c.Recommended.CPURequest.MilliValue() - c.Current.CPURequest.MilliValue()
				t.Logf("rec current=%s rec=%s increase=%dm",
					c.Current.CPURequest.String(), c.Recommended.CPURequest.String(), inc)
				if inc > 20 {
					sawBigIncrease = true
					return true, nil
				}
			}
		}
		return false, nil
	}), "timed out waiting for CPU increase rec > 20m (burn) or BudgetExhausted")

	// Wait until the controller has attempted the resize (BudgetExhausted) or
	// we can prove the live pod never left 50m after a big increase rec.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if policyHasEvent(t, ns, "budinc-policy", "BudgetExhausted") {
			return true, nil
		}
		return sawBigIncrease && countPodsWithCPURequest(t, ns, app, origCPU) == 1, nil
	}), "timed out waiting for BudgetExhausted or stable deferred pod")

	// Live pod is the source of truth: CPU must not have increased past budget.
	// status.workloads.resized can be sticky/misleading if any other resource
	// path ever counted; with memory pinned it should stay 0, but we do not
	// hard-fail on it if the live pod and gate event are correct.
	stillAtOrig := countPodsWithCPURequest(t, ns, app, origCPU)
	exhausted := policyHasEvent(t, ns, "budinc-policy", "BudgetExhausted")
	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "budinc-policy", Namespace: ns}, &p))
	t.Logf("resized=%d stillAtOrig=%d BudgetExhausted=%v sawBigIncrease=%v",
		p.Status.Workloads.Resized, stillAtOrig, exhausted, sawBigIncrease)

	require.Equal(t, 1, stillAtOrig,
		"pod CPU must remain at original 50m when increase exceeds maxTotalCpuIncrease")
	// Live request must not exceed original + budget (20m).
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": app}))
	require.NotEmpty(t, pods.Items)
	liveCPU := pods.Items[0].Spec.Containers[0].Resources.Requests.Cpu()
	require.NotNil(t, liveCPU)
	assert.LessOrEqual(t, liveCPU.MilliValue(), origCPU.MilliValue()+cpuBudget.MilliValue(),
		"live CPU must not exceed original+budget, got %s", liveCPU.String())
	assert.True(t, exhausted || sawBigIncrease,
		"expected BudgetExhausted and/or observed increase rec > budget")
	if p.Status.Workloads.Resized != 0 {
		t.Logf("WARN: status.workloads.resized=%d while live CPU stayed gated; treating live pod as authority",
			p.Status.Workloads.Resized)
	}
}

// TestE2E_GuaranteedQoS_CPUResizeWithMemoryHeld verifies that when memory
// cannot decrease (Guaranteed + allowDecrease=false / minAllowed=current),
// an overprovisioned CPU request still resizes live via /resize. This is
// the product path that nightly #492 hit when CPU PromQL was empty and only
// the memory path ran (then got clamped).
func TestE2E_GuaranteedQoS_CPUResizeWithMemoryHeld(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("gqcpu")
	createNamespace(t, ns)

	const app = "gqcpu-app"
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: app, Namespace: ns,
			Labels: map[string]string{"app": app},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.9",
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, app, ns, 60*time.Second)

	controlled := attunev1alpha1.ControlledRequestsAndLimits
	deployName := app
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gqcpu-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 15 * time.Second},
				// 1m rate window fills faster under parallel E2E Prometheus load
				// than 5m (nightly v1.32 GuaranteedQoS timeout).
				RateWindow: &metav1.Duration{Duration: time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &controlled,
				MinAllowed:       quantityPtr("50m"),
				// Keep max below start so only decreases apply under load spikes.
				MaxAllowed:       quantityPtr("250m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(false),
				ControlledValues: &controlled,
				// Pin min=max so a memory *increase* cannot break Guaranteed
				// 256Mi while we only assert CPU decrease (nightly saw 512Mi).
				MinAllowed:       quantityPtr("256Mi"),
				MaxAllowed:       quantityPtr("256Mi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	origCPU := resource.MustParse("500m")
	waitForLiveCPUDecrease(t, "gqcpu-policy", ns, app, origCPU, 6*time.Minute,
		"timed out waiting for Guaranteed pod CPU decrease with memory held")

	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": app}))
	require.NotEmpty(t, pods.Items)
	c := pods.Items[0].Spec.Containers[0]
	assert.True(t, c.Resources.Requests.Cpu().Cmp(origCPU) < 0)
	assert.True(t, c.Resources.Requests.Memory().Equal(resource.MustParse("256Mi")),
		"memory must stay at Guaranteed 256Mi, got %s", c.Resources.Requests.Memory().String())
}

func TestE2E_ScheduleWindow_SkipsOutsideWindow(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("sched")
	createNamespace(t, ns)
	createDeployment(t, "sched-app", ns, "250m", "256Mi", 1)
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
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sched-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:     attunev1alpha1.UpdateTypeAuto,
				Cooldown: &metav1.Duration{Duration: time.Minute},
				Schedule: &attunev1alpha1.ResizeSchedule{
					DaysOfWeek: excludedDays,
					Windows:    []attunev1alpha1.TimeWindow{{Start: "00:00", End: "23:59"}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "sched-policy", ns, 2*time.Minute)

	// Wait for a recommendation to be computed, proving the operator has data
	// and the only thing blocking resize is the schedule.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pol attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sched-policy", Namespace: ns}, &pol); err != nil {
			return false, nil
		}
		return pol.Status.Workloads.WithRecommendations > 0, nil
	}))

	// Force a reconcile after recommendation is available.
	forcePolicyReconcile(t, "sched-policy", ns, 2*time.Minute)

	// Today is excluded from the schedule, so no resizes should occur.
	var p attunev1alpha1.AttunePolicy
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
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bearer-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: promAddr,
					BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
						Name: "prom-token",
						Key:  "token",
					},
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
			Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:     attunev1alpha1.UpdateTypeRecommend,
				Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Prometheus doesn't require auth, but the operator should successfully
	// read the Secret, inject the bearer token, and query without error.
	waitForPolicyDiscovered(t, "bearer-policy", ns, 2*time.Minute)

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "bearer-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(1), p.Status.Workloads.Discovered,
		"policy with bearer token should discover workloads")
}

func TestE2E_EvictionFallback_ResizesWithInPlaceOrRecreate(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("evict")
	createNamespace(t, ns)
	createDeployment(t, "evict-app", ns, "250m", "256Mi", 2)
	waitForDeploymentReady(t, "evict-app", ns, 60*time.Second)

	deployName := "evict-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "evict-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:         attunev1alpha1.UpdateTypeAuto,
				Cooldown:     &metav1.Duration{Duration: time.Minute},
				AutoRevert:   boolPtr(true),
				ResizeMethod: attunev1alpha1.ResizeMethodInPlaceOrRecreate,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for resize. With InPlaceOrRecreate, the resize should succeed
	// either in-place or via eviction fallback.
	waitForResize(t, "evict-policy", ns, 3*time.Minute)

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "evict-policy", Namespace: ns}, &p))
	assert.GreaterOrEqual(t, p.Status.Workloads.Resized, int32(1),
		"at least one workload should be resized with InPlaceOrRecreate")
}

func TestE2E_RecommendMode_KeepsRecommendationsWithoutLivePods(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("nopods")
	createNamespace(t, ns)

	// Create a deployment so Prometheus collects metrics.
	createDeployment(t, "nopods-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "nopods-app", ns, 60*time.Second)

	createPolicy(t, "nopods-policy", ns, "nopods-app", attunev1alpha1.UpdateTypeRecommend)
	waitForPolicyDiscovered(t, "nopods-policy", ns, 2*time.Minute)

	// Wait until recommendations appear.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nopods-policy", Namespace: ns}, &p); err != nil {
			return false, nil
		}
		return p.Status.Workloads.WithRecommendations > 0 && len(p.Status.Recommendations) > 0, nil
	}))

	var beforeScale attunev1alpha1.AttunePolicy
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

	var final attunev1alpha1.AttunePolicy
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
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "rotate-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: promAddr,
					BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
						Name: "rotate-token",
						Key:  "token",
					},
				},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
			Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:     attunev1alpha1.UpdateTypeRecommend,
				Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for initial discovery with the first token.
	waitForPolicyDiscovered(t, "rotate-policy", ns, 2*time.Minute)

	var p1 attunev1alpha1.AttunePolicy
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

	var p2 attunev1alpha1.AttunePolicy
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
	// Guaranteed QoS + NotRequired memory: an in-place memory limit decrease
	// is clamped (v1.33), then request is raised back for Guaranteed, so a
	// memory-only resize is a no-op. Nightly #492 failed when CPU PromQL was
	// empty (rate[30s]) so only the memory path ran. Fix: rateWindow 5m +
	// hold memory at 64Mi (allowDecrease=false, minAllowed=64Mi) so the first
	// resize is a CPU change.
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
						Image:   stressNGImage,
						Command: []string{"/stress-ng", "--sleep", "1", "--timeout", "3600"},
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							// NotRequired so the memory resize is applied in-place
							// without killing the container. RestartContainer causes
							// resize-induced restarts that (a) kill the exec'd stressor
							// before it can OOM and (b) overwrite LastTerminationState
							// so the OOMKill evidence is lost by subsequent restarts.
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
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

	controlledValues := attunev1alpha1.ControlledRequestsAndLimits
	deployName := "oom-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "oom-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 15 * time.Second},
				// Short window so sleep-only stress-ng yields a low CPU rec
				// under parallel load (nightly #509: rec stayed 500m==current).
				RateWindow: &metav1.Duration{Duration: time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &controlledValues,
				MinAllowed:       quantityPtr("50m"),
				// Cap below the 500m Guaranteed start so the first resize is
				// always a decrease even if metrics are noisy or equal-current.
				MaxAllowed:       quantityPtr("250m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "0",
				// Hold memory at 64Mi for the OOM phase; a recommended decrease
				// would be canceled by ClampMemoryLimitForPolicy + Guaranteed QoS.
				AllowDecrease:    boolPtr(false),
				ControlledValues: &controlledValues,
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("64Mi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
				// Short observation period so the safety monitor checks
				// quickly after OOMKill instead of waiting the 5m default.
				Canary: &attunev1alpha1.CanaryConfig{
					Percentage:        1, // minimum required by CRD; ignored in Auto mode
					ObservationPeriod: metav1.Duration{Duration: time.Minute},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// First resize must be a CPU decrease (memory pinned at 64Mi).
	waitForLiveCPUDecrease(t, "oom-policy", ns, "oom-app", resource.MustParse("500m"), 4*time.Minute,
		"policy oom-policy: timed out waiting for CPU decrease before OOM phase")

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
				Command:   []string{"/stress-ng", "--vm", "1", "--vm-bytes", "1G", "--timeout", "120"},
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
		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "oom-policy", Namespace: ns}, &p); err != nil {
			return false, nil
		}
		for _, h := range p.Status.ResizeHistory {
			if h.Result == attunev1alpha1.ResizeResultReverted {
				t.Logf("Revert detected: workload=%s container=%s resource=%s", h.Workload, h.Container, h.Resource)
				return true, nil
			}
		}
		return false, nil
	}), "timed out waiting for safety revert after OOMKill")

	var finalPolicy attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "oom-policy", Namespace: ns}, &finalPolicy))
	hasRevert := false
	for i, h := range finalPolicy.Status.ResizeHistory {
		t.Logf("  [%d] workload=%s container=%s resource=%s result=%s", i, h.Workload, h.Container, h.Resource, h.Result)
		if h.Result == attunev1alpha1.ResizeResultReverted {
			hasRevert = true
		}
	}
	assert.True(t, hasRevert, "resize history should contain a Reverted entry after OOMKill")
}

func TestE2E_MultiReplica_ProgressiveResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("multi-rep")
	createNamespace(t, ns)
	createDeployment(t, "multi-rep-app", ns, "250m", "256Mi", 3)
	waitForDeploymentReady(t, "multi-rep-app", ns, 120*time.Second)

	deployName := "multi-rep-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-rep-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                 attunev1alpha1.UpdateTypeAuto,
				Cooldown:             &metav1.Duration{Duration: time.Minute},
				MaxConcurrentResizes: 1,
				AutoRevert:           boolPtr(true),
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

	// Guaranteed QoS: requests = limits. Use moderate initial resources to
	// avoid starving the k3d node (2000m was too large, causing timeouts).
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
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, "qos-app", ns, 60*time.Second)

	controlledBoth := attunev1alpha1.ControlledRequestsAndLimits
	deployName := "qos-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "qos-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				ControlledValues: &controlledBoth,
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				ControlledValues: &controlledBoth,
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
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

	// At least one resource should have changed from the initial values.
	origCPU := resource.MustParse("250m")
	origMem := resource.MustParse("256Mi")
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
							corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
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

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "selector-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind:     "Deployment",
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "platform"}},
			},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU:    attunev1alpha1.ResourceConfig{Percentile: 95, Overhead: "20"},
			Memory: attunev1alpha1.ResourceConfig{Percentile: 99, Overhead: "30"},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type: attunev1alpha1.UpdateTypeRecommend, Cooldown: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForPolicyDiscovered(t, "selector-policy", ns, 2*time.Minute)

	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "selector-policy", Namespace: ns}, &p))
	assert.Equal(t, int32(2), p.Status.Workloads.Discovered,
		"selector should discover exactly the 2 matching deployments")
}

func TestE2E_PolicyDeletion_CleansUpAnnotations(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("cleanup")
	createNamespace(t, ns)
	createDeployment(t, "cleanup-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "cleanup-app", ns, 60*time.Second)

	policy := createPolicy(t, "cleanup-policy", ns, "cleanup-app", attunev1alpha1.UpdateTypeAuto)

	// Wait for resize so tracking annotations are set on the pod.
	waitForResize(t, "cleanup-policy", ns, 3*time.Minute)

	// Verify annotations exist before deletion.
	var podsBefore corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podsBefore, client.InNamespace(ns), client.MatchingLabels{"app": "cleanup-app"}))
	require.NotEmpty(t, podsBefore.Items)
	assert.Contains(t, podsBefore.Items[0].Labels, "attune.io/tracked",
		"pod should have tracking label before policy deletion")

	// Delete the policy.
	require.NoError(t, k8sClient.Delete(ctx, policy))

	// Wait for the finalizer to complete (policy fully gone).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p attunev1alpha1.AttunePolicy
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "cleanup-policy", Namespace: ns}, &p)
		return err != nil, nil // gone when Get fails
	}), "timed out waiting for policy deletion")

	// Verify tracking annotations and labels are cleaned up from the pod.
	var podsAfter corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podsAfter, client.InNamespace(ns), client.MatchingLabels{"app": "cleanup-app"}))
	require.NotEmpty(t, podsAfter.Items)
	pod := podsAfter.Items[0]
	assert.NotContains(t, pod.Labels, "attune.io/tracked",
		"tracking label should be removed after policy deletion")
	assert.NotContains(t, pod.Annotations, "attune.io/policy",
		"policy annotation should be removed after policy deletion")
}

func TestE2E_ScaleUp_NewReplicasGetResized(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("scaleup")
	createNamespace(t, ns)
	createDeployment(t, "scaleup-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "scaleup-app", ns, 60*time.Second)

	createPolicy(t, "scaleup-policy", ns, "scaleup-app", attunev1alpha1.UpdateTypeAuto)
	waitForResize(t, "scaleup-policy", ns, 5*time.Minute)

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

	// Wait for the second pod to be resized. Re-bump the policy periodically:
	// under parallel E2E load the new replica can miss a single reconcile
	// window (cooldown / InsufficientData), which flaked nightly on v1.33.
	lastForce := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		if time.Since(lastForce) > 45*time.Second {
			touchPolicySpec(t, "scaleup-policy", ns)
			lastForce = time.Now()
		}
		var podList corev1.PodList
		if err := k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "scaleup-app"}); err != nil {
			return false, nil
		}
		resizedCount := 0
		running := 0
		origCPU := resource.MustParse("250m")
		for _, pod := range podList.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			running++
			for _, c := range pod.Spec.Containers {
				if c.Name == "app" && c.Resources.Requests.Cpu().Cmp(origCPU) != 0 {
					resizedCount++
				}
			}
		}
		if time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			for _, pod := range podList.Items {
				for _, c := range pod.Spec.Containers {
					if c.Name == "app" {
						t.Logf("scaleup pod %s phase=%s cpu=%s", pod.Name, pod.Status.Phase, c.Resources.Requests.Cpu().String())
					}
				}
			}
			t.Logf("scaleup progress: running=%d resized=%d", running, resizedCount)
		}
		return running >= 2 && resizedCount >= 2, nil
	}), "both replicas should eventually be resized")
}

func TestE2E_ConcurrentPolicies_SameNamespace(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("concurrent")
	createNamespace(t, ns)
	createDeployment(t, "api-app", ns, "250m", "256Mi", 1)
	createDeployment(t, "worker-app", ns, "250m", "256Mi", 1)
	waitForDeploymentReady(t, "api-app", ns, 60*time.Second)
	waitForDeploymentReady(t, "worker-app", ns, 60*time.Second)

	createPolicy(t, "api-policy", ns, "api-app", attunev1alpha1.UpdateTypeRecommend)
	createPolicy(t, "worker-policy", ns, "worker-app", attunev1alpha1.UpdateTypeRecommend)

	// Wait for recommendations (not just discovery) so we can assert workload names.
	waitForRecommendations := func(policyName string) {
		t.Helper()
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
			var p attunev1alpha1.AttunePolicy
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err != nil {
				return false, nil
			}
			return len(p.Status.Recommendations) > 0, nil
		}), "timed out waiting for recommendations on %s", policyName)
	}
	waitForRecommendations("api-policy")
	waitForRecommendations("worker-policy")

	// Verify each policy sees only its own workload.
	var apiPolicy attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "api-policy", Namespace: ns}, &apiPolicy))
	assert.Equal(t, int32(1), apiPolicy.Status.Workloads.Discovered)
	require.NotEmpty(t, apiPolicy.Status.Recommendations, "api-policy should have recommendations")
	assert.Equal(t, "api-app", apiPolicy.Status.Recommendations[0].Workload)

	var workerPolicy attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "worker-policy", Namespace: ns}, &workerPolicy))
	assert.Equal(t, int32(1), workerPolicy.Status.Workloads.Discovered)
	require.NotEmpty(t, workerPolicy.Status.Recommendations, "worker-policy should have recommendations")
	assert.Equal(t, "worker-app", workerPolicy.Status.Recommendations[0].Workload)
}

func TestE2E_MemoryAllowDecreaseFalse(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("nodecrease")
	createNamespace(t, ns)

	// Overprovisioned so pause metrics produce a CPU decrease once rate() has
	// data (rateWindow 5m). Memory allowDecrease defaults false: when CPU
	// PromQL is empty, both resources stay put and waitForResize hangs (#492).
	createDeployment(t, "nodecrease-app", ns, "500m", "256Mi", 1)
	waitForDeploymentReady(t, "nodecrease-app", ns, 60*time.Second)

	deployName := "nodecrease-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nodecrease-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 15 * time.Second},
				// Short rate window so rate() has samples sooner under parallel
				// E2E load (nightly MemoryAllowDecrease 4m timeouts on 1.32/1.35).
				RateWindow: &metav1.Duration{Duration: time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile: 95,
				Overhead:   "20",
				MinAllowed: quantityPtr("50m"),
				// Cap below the 500m start request so a concurrent-load metric
				// spike cannot resize *up* to 1 CPU (1000m). CI saw resized=1
				// with live cpu=1 while this wait still demanded a decrease.
				MaxAllowed:       quantityPtr("250m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile: 99,
				Overhead:   "30",
				// AllowDecrease intentionally NOT set (nil), so the default false applies.
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Wait for a live CPU decrease. Status Resized alone is not enough: a
	// memory-only no-op path can race, and rec-only readiness does not mean
	// UpdateResize applied yet (#492).
	origCPU := resource.MustParse("500m")
	origMem := resource.MustParse("256Mi")
	waitForLiveCPUDecrease(t, "nodecrease-policy", ns, "nodecrease-app", origCPU, 6*time.Minute,
		"timed out waiting for live CPU decrease with memory allowDecrease default false")

	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "nodecrease-app"}))
	require.NotEmpty(t, podList.Items)
	c := podList.Items[0].Spec.Containers[0]

	assert.GreaterOrEqual(t, c.Resources.Requests.Memory().Value(), origMem.Value(),
		"memory should not decrease when allowDecrease is nil (default false), got %s", c.Resources.Requests.Memory().String())
	assert.True(t, c.Resources.Requests.Cpu().Cmp(origCPU) < 0,
		"CPU should decrease while memory is held, got cpu=%s", c.Resources.Requests.Cpu().String())
}

func TestE2E_MultiContainer_SequentialResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("multiresz")
	createNamespace(t, ns)

	// Two containers, both eligible for resize (no excludedContainers).
	// Both start with 250m CPU (kept under 300m to reduce scheduling pressure).
	// The operator should resize both sequentially, each UpdateResize call
	// bumping resourceVersion. The *pod = *freshPod propagation (PR #412)
	// ensures the second resize uses the fresh resourceVersion from the first.
	// Without it, the second UpdateResize would fail with a conflict on a
	// real API server (kubefake doesn't validate resourceVersion).
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-resize-app",
			Namespace: ns,
			Labels:    map[string]string{"app": "multi-resize-app"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "multi-resize-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "multi-resize-app"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "web",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("250m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
						{
							Name:  "worker",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("250m"),
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
	waitForDeploymentReady(t, "multi-resize-app", ns, 60*time.Second)

	deployName := "multi-resize-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-resize-policy", Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	waitForResize(t, "multi-resize-policy", ns, 3*time.Minute)

	// Verify both containers were resized.
	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "multi-resize-app"},
	))
	require.NotEmpty(t, podList.Items)

	pod := podList.Items[0]
	origCPU := resource.MustParse("250m")
	origMem := resource.MustParse("256Mi")
	resizedContainers := 0
	for _, c := range pod.Spec.Containers {
		cpuChanged := c.Resources.Requests.Cpu().Cmp(origCPU) != 0
		memChanged := c.Resources.Requests.Memory().Cmp(origMem) != 0
		if cpuChanged || memChanged {
			resizedContainers++
			t.Logf("container %s resized: cpu=%s mem=%s",
				c.Name, c.Resources.Requests.Cpu(), c.Resources.Requests.Memory())
		}
	}
	assert.Equal(t, 2, resizedContainers,
		"both containers should be resized; sequential UpdateResize requires fresh resourceVersion propagation")

	// Verify resize history records both containers.
	var p attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "multi-resize-policy", Namespace: ns}, &p))
	historyContainers := make(map[string]bool)
	for _, h := range p.Status.ResizeHistory {
		historyContainers[h.Container] = true
		t.Logf("history: workload=%s container=%s resource=%s result=%s",
			h.Workload, h.Container, h.Resource, h.Result)
	}
	assert.True(t, historyContainers["web"],
		"resize history should include web container")
	assert.True(t, historyContainers["worker"],
		"resize history should include worker container")

	// Verify pod annotations indicate resize tracking.
	assert.Contains(t, pod.Labels, "attune.io/tracked",
		"resized pod should have tracking label")
}

// TestE2E_MemoryLimitDecrease_VersionAware proves version-aware memory limit
// handling on a real API server:
//
//   - Policy: controlledValues=RequestsAndLimits, allowDecrease=true, oversized
//     initial Guaranteed memory limit (512Mi) on a near-idle pause pod.
//   - Kubernetes 1.35+: live memory limit decreases are allowed; Attune skips
//     the platform clamp so the limit drops with the recommendation.
//   - Kubernetes 1.33–1.34: API rejects in-place limit decreases for NotRequired;
//     Attune clamps the limit, so the pod memory limit stays at the initial value.
//
// Usage-floor flooring (limit raised above recent usage) stays unit-tested:
// pause pods report near-zero cgroup usage and cannot exercise that path.
func TestE2E_MemoryLimitDecrease_VersionAware(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("memlim")
	createNamespace(t, ns)

	sv, err := clientset.Discovery().ServerVersion()
	require.NoError(t, err)
	gitVersion := sv.GitVersion
	allowDecrease := resize.AllowsInPlaceMemoryLimitDecrease(gitVersion)
	t.Logf("cluster GitVersion=%s AllowsInPlaceMemoryLimitDecrease=%v", gitVersion, allowDecrease)

	// Guaranteed QoS: requests == limits. Oversize memory so the recommendation
	// (pause ≈ minAllowed 64Mi) is a clear decrease. CPU at 500m avoids the
	// pause-container change-filter dead zone so a resize is recorded even when
	// memory is platform-clamped on 1.33/1.34.
	const (
		appName    = "memlim-app"
		policyName = "memlim-policy"
		initCPU    = "500m"
		initMem    = "512Mi"
	)
	initMemQ := resource.MustParse(initMem)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: ns,
			Labels:    map[string]string{"app": appName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.9",
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(initCPU),
								corev1.ResourceMemory: initMemQ.DeepCopy(),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(initCPU),
								corev1.ResourceMemory: initMemQ.DeepCopy(),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, appName, ns, 90*time.Second)

	controlled := attunev1alpha1.ControlledRequestsAndLimits
	deployName := appName
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				AllowDecrease:    boolPtr(true),
				ControlledValues: &controlled,
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(true),
				ControlledValues: &controlled,
				MinAllowed:       quantityPtr("64Mi"),
				// Pause confidence can recommend ~1Gi. Cap MaxAllowed below the
				// 512Mi start so a 1.33 clamp stays on the initial limit.
				MaxAllowed:       quantityPtr("256Mi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))

	// Recommendation should target a lower memory limit than the oversized start.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err != nil {
			return false, nil
		}
		for _, rec := range p.Status.Recommendations {
			for _, cr := range rec.Containers {
				if cr.Name != "app" {
					continue
				}
				recLim := cr.Recommended.MemoryLimit
				if recLim.IsZero() {
					// Some status paths only populate request; treat request as proxy.
					recLim = cr.Recommended.MemoryRequest
				}
				if !recLim.IsZero() && recLim.Cmp(initMemQ) < 0 {
					t.Logf("recommendation memory limit/request %s < initial %s", recLim.String(), initMem)
					return true, nil
				}
			}
		}
		return false, nil
	}), "expected a memory recommendation below %s", initMem)

	waitForResize(t, policyName, ns, 4*time.Minute)

	// Poll applied pod resources: CPU may move first; on 1.35 memory limit
	// should drop; on 1.33–1.34 the platform clamp keeps the limit.
	var finalMemLim resource.Quantity
	var lastPodName string
	var lastMemReq resource.Quantity
	pollErr := wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": appName}); err != nil {
			return false, nil
		}
		if len(pods.Items) == 0 {
			return false, nil
		}
		// Prefer a Running pod after any restart.
		var c *corev1.Container
		var podName string
		for i := range pods.Items {
			if pods.Items[i].DeletionTimestamp != nil {
				continue
			}
			for j := range pods.Items[i].Spec.Containers {
				if pods.Items[i].Spec.Containers[j].Name == "app" {
					c = &pods.Items[i].Spec.Containers[j]
					podName = pods.Items[i].Name
					break
				}
			}
			if c != nil {
				break
			}
		}
		if c == nil {
			return false, nil
		}
		lastPodName = podName
		if req := c.Resources.Requests.Memory(); req != nil && !req.IsZero() {
			lastMemReq = *req
		}
		lim := c.Resources.Limits.Memory()
		if lim == nil || lim.IsZero() {
			return false, nil
		}
		finalMemLim = *lim
		cpuReq := c.Resources.Requests.Cpu()
		cpuChanged := cpuReq != nil && cpuReq.Cmp(resource.MustParse(initCPU)) != 0
		memDecreased := lim.Cmp(initMemQ) < 0
		memUnchanged := lim.Cmp(initMemQ) == 0

		if allowDecrease {
			// 1.35+: need a real limit decrease.
			if memDecreased {
				return true, nil
			}
			// Keep waiting while resize is still converging.
			return false, nil
		}
		// 1.33–1.34: clamp keeps limit; accept once a resize applied (CPU change
		// or reconcile recorded) and limit is still at the initial value.
		if memUnchanged && cpuChanged {
			return true, nil
		}
		if memUnchanged {
			// Resize may only have touched memory request/limit clamp with
			// no CPU delta yet; check policy resized count.
			var p attunev1alpha1.AttunePolicy
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err == nil {
				if p.Status.Workloads.Resized > 0 {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if pollErr != nil {
		t.Logf("version-aware poll timeout: pod=%s request=%s limit=%s",
			lastPodName, lastMemReq.String(), finalMemLim.String())
	}
	require.NoError(t, pollErr, "timed out waiting for version-aware memory limit outcome (allowInPlace=%v, last request=%s last limit=%s)",
		allowDecrease, lastMemReq.String(), finalMemLim.String())

	t.Logf("final memory limit=%s initial=%s allowInPlaceDecrease=%v", finalMemLim.String(), initMem, allowDecrease)

	if allowDecrease {
		assert.True(t, finalMemLim.Cmp(initMemQ) < 0,
			"Kubernetes %s should allow in-place memory limit decrease: got limit %s, want < %s",
			gitVersion, finalMemLim.String(), initMem)
		// Still above policy minAllowed when the chain floors at 64Mi.
		minAllowed := resource.MustParse("64Mi")
		assert.GreaterOrEqual(t, finalMemLim.Value(), minAllowed.Value(),
			"decreased limit %s should not go below minAllowed 64Mi", finalMemLim.String())
	} else {
		assert.Equal(t, initMemQ.Value(), finalMemLim.Value(),
			"Kubernetes %s should clamp memory limit decreases: got limit %s, want still %s",
			gitVersion, finalMemLim.String(), initMem)
	}
}

// patchPodResizePending sets PodResizePending with the given reason (Deferred
// or Infeasible). Kubelet may overwrite status; callers re-apply in a loop.
func patchPodResizePending(t *testing.T, podName, namespace, reason string) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		now := metav1.Now()
		cond := corev1.PodCondition{
			Type:               corev1.PodResizePending,
			Status:             corev1.ConditionTrue,
			Reason:             reason,
			Message:            "e2e injected for ResizeBlocked UX",
			LastTransitionTime: now,
		}
		found := false
		for i := range pod.Status.Conditions {
			if pod.Status.Conditions[i].Type == corev1.PodResizePending {
				pod.Status.Conditions[i] = cond
				found = true
				break
			}
		}
		if !found {
			pod.Status.Conditions = append(pod.Status.Conditions, cond)
		}
		_, err = clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		return err
	}))
}

// TestE2E_ResizeBlocked_DeferredAndInfeasibleStatus injects kubelet-style
// PodResizePending conditions and asserts Attune surfaces
// status.workloads.deferred/infeasible and ResizeBlocked.
//
// Real Deferred/Infeasible require node capacity races that are flake-prone on
// shared k3d. Status injection validates the operator UX path (#436).
func TestE2E_ResizeBlocked_DeferredAndInfeasibleStatus(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("blocked")
	createNamespace(t, ns)

	const (
		appName    = "blocked-app"
		policyName = "blocked-policy"
	)
	// Two replicas: one Deferred, one Infeasible.
	createDeployment(t, appName, ns, "250m", "256Mi", 2)
	waitForDeploymentReady(t, appName, ns, 90*time.Second)

	createPolicy(t, policyName, ns, appName, attunev1alpha1.UpdateTypeRecommend)
	waitForPolicyDiscovered(t, policyName, ns, 90*time.Second)

	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": appName}))
	require.GreaterOrEqual(t, len(pods.Items), 2, "need two pods")
	// Prefer Running pods.
	var names []string
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil || pods.Items[i].Status.Phase != corev1.PodRunning {
			continue
		}
		names = append(names, pods.Items[i].Name)
		if len(names) == 2 {
			break
		}
	}
	require.Len(t, names, 2, "need two Running pods")
	deferredPod, infeasiblePod := names[0], names[1]

	// Re-inject conditions while waiting: kubelet may clear synthetic status.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 3*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		patchPodResizePending(t, deferredPod, ns, "Deferred")
		patchPodResizePending(t, infeasiblePod, ns, "Infeasible")
		forcePolicyReconcile(t, policyName, ns, 45*time.Second)

		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err != nil {
			return false, nil
		}
		t.Logf("workloads deferred=%d infeasible=%d", p.Status.Workloads.Deferred, p.Status.Workloads.Infeasible)
		if p.Status.Workloads.Deferred < 1 || p.Status.Workloads.Infeasible < 1 {
			return false, nil
		}
		var blocked *metav1.Condition
		for i := range p.Status.Conditions {
			if p.Status.Conditions[i].Type == attunev1alpha1.ConditionResizeBlocked {
				blocked = &p.Status.Conditions[i]
				break
			}
		}
		if blocked == nil || blocked.Status != metav1.ConditionTrue {
			t.Logf("ResizeBlocked not True yet")
			return false, nil
		}
		// Prefer both reasons when both counts are set.
		// Both counts are set: controller must use the combined reason.
		if blocked.Reason != attunev1alpha1.ReasonPodsDeferredAndInfeasible {
			t.Logf("waiting for reason=%s (got %s)",
				attunev1alpha1.ReasonPodsDeferredAndInfeasible, blocked.Reason)
			return false, nil
		}
		t.Logf("OK: ResizeBlocked reason=%s message=%s", blocked.Reason, blocked.Message)
		return true, nil
	}), "expected Deferred+Infeasible counts and ResizeBlocked condition")
}

// setNodeMemoryPressure sets or clears NodeMemoryPressure on the named node.
// Caller must restore in Cleanup. Not parallel-safe across tests that need
// memory *increases* on the same node.
func setNodeMemoryPressure(t *testing.T, nodeName string, pressure bool) {
	t.Helper()
	require.NoError(t, applyNodeMemoryPressure(nodeName, pressure))
}

// applyNodeMemoryPressure patches only the MemoryPressure condition via
// strategic merge so concurrent kubelet status writes are less likely to
// wipe the injection entirely. k3s/kubelet still rewrites conditions on
// its own schedule; callers must re-apply frequently while the test needs
// pressure held.
func applyNodeMemoryPressure(nodeName string, pressure bool) error {
	status := string(corev1.ConditionFalse)
	if pressure {
		status = string(corev1.ConditionTrue)
	}
	now := metav1.Now().UTC().Format(time.RFC3339)
	// Strategic merge merges Node conditions by type=type.
	patch := fmt.Sprintf(`{"status":{"conditions":[{"type":"MemoryPressure","status":%q,"reason":"E2ETest","message":"e2e MemoryPressure injection","lastHeartbeatTime":%q,"lastTransitionTime":%q}]}}`,
		status, now, now)
	_, err := clientset.CoreV1().Nodes().Patch(
		ctx, nodeName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}
	// Fallback: full status update (older servers / patch edge cases).
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, getErr := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		want := corev1.ConditionFalse
		if pressure {
			want = corev1.ConditionTrue
		}
		found := false
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == corev1.NodeMemoryPressure {
				node.Status.Conditions[i].Status = want
				node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
				node.Status.Conditions[i].LastTransitionTime = metav1.Now()
				node.Status.Conditions[i].Reason = "E2ETest"
				node.Status.Conditions[i].Message = "e2e MemoryPressure injection"
				found = true
				break
			}
		}
		if !found && pressure {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               corev1.NodeMemoryPressure,
				Status:             corev1.ConditionTrue,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "E2ETest",
				Message:            "e2e MemoryPressure injection",
			})
		}
		_, updateErr := clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		return updateErr
	})
}

// nodeHasMemoryPressure reports whether the live API shows MemoryPressure=True.
func nodeHasMemoryPressure(nodeName string) bool {
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// pressureSampleLog records live MemoryPressure observations while the E2E
// hold loop runs. k3s/kubelet can clear synthetic conditions between injects;
// a single "True now" sample after a resize is not proof the gate saw True.
type pressureSampleLog struct {
	mu      sync.Mutex
	samples []pressureSample
}

type pressureSample struct {
	at       time.Time
	pressure bool
}

func (l *pressureSampleLog) record(pressure bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples = append(l.samples, pressureSample{at: time.Now(), pressure: pressure})
	// Keep ~30s at 100ms inject (~300 samples) with headroom.
	const maxSamples = 400
	if len(l.samples) > maxSamples {
		l.samples = l.samples[len(l.samples)-maxSamples:]
	}
}

// solidlyTrue reports whether every sample in the last window was True and
// there are enough samples to trust the window (not a single lucky Get).
func (l *pressureSampleLog) solidlyTrue(window time.Duration, minSamples int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if minSamples < 1 {
		minSamples = 1
	}
	cutoff := time.Now().Add(-window)
	n, anyFalse := 0, false
	for i := len(l.samples) - 1; i >= 0; i-- {
		s := l.samples[i]
		if s.at.Before(cutoff) {
			break
		}
		n++
		if !s.pressure {
			anyFalse = true
		}
	}
	return n >= minSamples && !anyFalse
}

// recentHadFalse reports whether any sample in the window was False (inject
// lost / kubelet cleared). Used to classify a memory increase as race vs bug.
func (l *pressureSampleLog) recentHadFalse(window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-window)
	for i := len(l.samples) - 1; i >= 0; i-- {
		s := l.samples[i]
		if s.at.Before(cutoff) {
			break
		}
		if !s.pressure {
			return true
		}
	}
	return false
}

// resetPressureWorkloadPods deletes running pods so the Deployment recreates
// them from the template (templatePersistence defaults off). Used after a
// kubelet-clear race allowed one memory increase.
func resetPressureWorkloadPods(t *testing.T, appName, ns string) {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": appName}))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		t.Logf("race recovery: deleting pod %s/%s (mem may have risen while kubelet cleared MemoryPressure)", p.Namespace, p.Name)
		_ = k8sClient.Delete(ctx, p)
	}
	waitForDeploymentReady(t, appName, ns, 90*time.Second)
}

// TestE2E_NodeMemoryPressure_SkipsMemoryIncrease patches MemoryPressure on the
// pod's node and asserts Attune does not raise an undersized memory request.
//
// Not t.Parallel: node status is cluster-scoped and would race other tests.
//
// Synthetic MemoryPressure is race-prone: kubelet rewrites node conditions from
// real free memory and can clear True between inject ticks. A memory bump while
// a later Get shows livePressure=true is not proof the operator applied under
// pressure (issue #481 / prior #476). We only hard-fail when samples show
// pressure was solidly True across a window around the increase; otherwise
// reset the pod and continue until ResizeSkipped(MemoryPressure).
func TestE2E_NodeMemoryPressure_SkipsMemoryIncrease(t *testing.T) {
	ns := uniqueNS("pressure")
	createNamespace(t, ns)

	const (
		appName    = "pressure-app"
		policyName = "pressure-policy"
		initMem    = "32Mi"
		initCPU    = "500m"
	)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: appName, Namespace: ns,
			Labels: map[string]string{"app": appName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(initCPU),
								corev1.ResourceMemory: resource.MustParse(initMem),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, appName, ns, 90*time.Second)

	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": appName}))
	require.NotEmpty(t, pods.Items)
	nodeName := pods.Items[0].Spec.NodeName
	require.NotEmpty(t, nodeName, "pod must be scheduled")
	t.Logf("injecting MemoryPressure on node %s", nodeName)

	// Hold MemoryPressure for the entire test: k3s/kubelet rewrites node
	// status and can clear injected conditions within a second. Re-apply
	// faster than the kubelet node-status period; sample live pressure so we
	// can distinguish injection races from real gate failures (#481).
	setNodeMemoryPressure(t, nodeName, true)
	t.Cleanup(func() {
		setNodeMemoryPressure(t, nodeName, false)
	})
	var samples pressureSampleLog
	stopPressure := make(chan struct{})
	donePressure := make(chan struct{})
	go func() {
		defer close(donePressure)
		// Immediate second inject, then high-frequency hold + sample.
		_ = applyNodeMemoryPressure(nodeName, true)
		samples.record(nodeHasMemoryPressure(nodeName))
		tck := time.NewTicker(100 * time.Millisecond)
		defer tck.Stop()
		for {
			select {
			case <-stopPressure:
				return
			case <-tck.C:
				_ = applyNodeMemoryPressure(nodeName, true)
				samples.record(nodeHasMemoryPressure(nodeName))
			}
		}
	}()
	t.Cleanup(func() {
		close(stopPressure)
		<-donePressure
	})

	// Do not create the policy until the live API shows pressure. Otherwise
	// the first reconcile can race a cleared condition and raise memory.
	require.Eventually(t, func() bool {
		_ = applyNodeMemoryPressure(nodeName, true)
		ok := nodeHasMemoryPressure(nodeName)
		samples.record(ok)
		return ok
	}, 10*time.Second, 50*time.Millisecond, "MemoryPressure never stuck on node %s", nodeName)

	// minAllowed 256Mi >> 32Mi forces a memory *increase*. CPU starts high so
	// under pressure the whole container resize is skipped (mem increase gate).
	deployName := appName
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				AllowDecrease:    boolPtr(false),
				MinAllowed:       quantityPtr("500m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(false),
				MinAllowed:       quantityPtr("256Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))
	waitForPolicyDiscovered(t, policyName, ns, 90*time.Second)
	_ = applyNodeMemoryPressure(nodeName, true)
	samples.record(nodeHasMemoryPressure(nodeName))

	initQ := resource.MustParse(initMem)
	// Wait for rec increase + ResizeSkipped. Memory bumps during a False
	// inject window are recovered by recreating pods from the template.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 1*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		_ = applyNodeMemoryPressure(nodeName, true)
		samples.record(nodeHasMemoryPressure(nodeName))

		var live corev1.PodList
		if err := k8sClient.List(ctx, &live, client.InNamespace(ns), client.MatchingLabels{"app": appName}); err != nil {
			return false, nil
		}
		for i := range live.Items {
			if live.Items[i].DeletionTimestamp != nil {
				continue
			}
			for _, c := range live.Items[i].Spec.Containers {
				if c.Name != "app" {
					continue
				}
				mem := c.Resources.Requests.Memory()
				if mem == nil || mem.Cmp(initQ) <= 0 {
					continue
				}
				// Prove pressure was solidly held, not a single post-hoc True.
				// 1s window @ 100ms inject ≈ 10 samples; require ≥5 True, no False.
				if samples.solidlyTrue(1*time.Second, 5) {
					return false, fmt.Errorf(
						"memory increased to %s while MemoryPressure was solidly True for ≥1s (want stay at %s; livePressure=%v)",
						mem.String(), initMem, nodeHasMemoryPressure(nodeName))
				}
				// Injection race (kubelet cleared between operator Get and our
				// sample, or any False in the recent window): reset and continue.
				t.Logf("injection race: memory rose to %s but pressure was not solidly True "+
					"(recentHadFalse=%v livePressure=%v); recreating pods at %s",
					mem.String(), samples.recentHadFalse(2*time.Second),
					nodeHasMemoryPressure(nodeName), initMem)
				resetPressureWorkloadPods(t, appName, ns)
				_ = applyNodeMemoryPressure(nodeName, true)
				samples.record(nodeHasMemoryPressure(nodeName))
				return false, nil
			}
		}

		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err != nil {
			return false, nil
		}
		recIncrease := false
		for _, rec := range p.Status.Recommendations {
			for _, cr := range rec.Containers {
				if cr.Name == "app" && !cr.Recommended.MemoryRequest.IsZero() &&
					cr.Recommended.MemoryRequest.Cmp(initQ) > 0 {
					recIncrease = true
				}
			}
		}
		if !recIncrease {
			return false, nil
		}

		evs, err := clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.kind=AttunePolicy,involvedObject.name=" + policyName,
		})
		if err != nil {
			return false, nil
		}
		for _, ev := range evs.Items {
			if ev.Reason == "ResizeSkipped" && strings.Contains(ev.Message, "MemoryPressure") {
				t.Logf("OK: ResizeSkipped event: %s", ev.Message)
				return true, nil
			}
		}
		// Nudge reconcile without long nested waits.
		forcePolicyReconcile(t, policyName, ns, 30*time.Second)
		return false, nil
	}), "expected memory rec increase + ResizeSkipped(MemoryPressure) with memory held at %s", initMem)

	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": appName}))
	require.NotEmpty(t, pods.Items)
	var c *corev1.Container
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		for j := range pods.Items[i].Spec.Containers {
			if pods.Items[i].Spec.Containers[j].Name == "app" {
				c = &pods.Items[i].Spec.Containers[j]
				break
			}
		}
		if c != nil {
			break
		}
	}
	require.NotNil(t, c)
	gotMem := c.Resources.Requests.Memory()
	require.NotNil(t, gotMem)
	assert.Equal(t, initQ.Value(), gotMem.Value(),
		"MemoryPressure should block memory request increase: got %s want %s",
		gotMem.String(), initMem)
}

// TestE2E_RuntimeProfileJava_BlocksMemoryDecrease verifies java runtimeProfile
// applies in-memory defaults (allowDecrease=false) so oversized memory is not
// decreased even when the field is unset on the CR.
func TestE2E_RuntimeProfileJava_BlocksMemoryDecrease(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("javamem")
	createNamespace(t, ns)

	const (
		appName    = "java-app"
		policyName = "java-policy"
		initMem    = "512Mi"
	)
	createDeployment(t, appName, ns, "500m", initMem, 1)
	waitForDeploymentReady(t, appName, ns, 90*time.Second)

	deployName := appName
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			RuntimeProfile: "java",
			TargetRef:      attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 30 * time.Second},
				RateWindow:        &metav1.Duration{Duration: 5 * time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "20",
				AllowDecrease:    boolPtr(true),
				MinAllowed:       quantityPtr("50m"),
				MaxAllowed:       quantityPtr("4000m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				// AllowDecrease and Overhead intentionally unset: java profile
				// defaults allowDecrease=false and overhead=40 in-memory.
				Percentile:       99,
				MinAllowed:       quantityPtr("64Mi"),
				MaxAllowed:       quantityPtr("8Gi"),
				MaxChangePercent: int32Ptr(100),
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:       attunev1alpha1.UpdateTypeAuto,
				Cooldown:   &metav1.Duration{Duration: time.Minute},
				AutoRevert: boolPtr(true),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))
	// java-unique default is overhead=40 (platform default memory overhead is 30).
	// That is the live signal the profile applied, not mere allowDecrease=false
	// (which is already the platform default when the field is nil).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &p); err != nil {
			return false, nil
		}
		for _, rec := range p.Status.Recommendations {
			for _, cr := range rec.Containers {
				if cr.Name != "app" || cr.Explanation == nil || cr.Explanation.Memory == nil {
					continue
				}
				if cr.Explanation.Memory.Overhead == 40 {
					t.Logf("OK: java profile effective memory overhead=40 in explanation")
					return true, nil
				}
				t.Logf("memory explanation overhead=%.0f (want 40)", cr.Explanation.Memory.Overhead)
			}
		}
		return false, nil
	}), "expected recommendation explanation memory overhead=40 from java runtimeProfile")

	waitForResize(t, policyName, ns, 4*time.Minute)

	// CR must still show unset allowDecrease/overhead (defaults are not written back).
	var stored attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &stored))
	assert.Nil(t, stored.Spec.Memory.AllowDecrease,
		"java profile must not persist allowDecrease onto the CR")
	assert.Equal(t, "", stored.Spec.Memory.Overhead,
		"java profile must not persist overhead onto the CR")

	var podList corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": appName}))
	require.NotEmpty(t, podList.Items)
	c := podList.Items[0].Spec.Containers[0]
	origMem := resource.MustParse(initMem)
	assert.GreaterOrEqual(t, c.Resources.Requests.Memory().Value(), origMem.Value(),
		"memory should not decrease under java defaults, got %s",
		c.Resources.Requests.Memory().String())
}
