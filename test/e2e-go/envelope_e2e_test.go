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

package e2e_go

import (
	"context"
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

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/cluster"
	"github.com/attune-io/attune/internal/resize"
)

func TestE2E_PodLevelEnvelope_DiscoverGated(t *testing.T) {
	t.Parallel()
	caps, err := cluster.Discover(ctx, clientset.Discovery(), clientset.CoreV1().Nodes())
	require.NoError(t, err)
	if !caps.InPlacePodLevelResources && !caps.PodLevelResourcesField {
		t.Skipf("skip: no pod-level resources (InPlacePodLevelResources=%v PodLevelResourcesField=%v GitVersion=%s)",
			caps.InPlacePodLevelResources, caps.PodLevelResourcesField, caps.GitVersion)
	}

	ns := uniqueNS("envelope")
	createNamespace(t, ns)
	createDeploymentWithEnvelope(t, "env-app", ns, "100m", "256Mi")
	waitForDeploymentReady(t, "env-app", ns, deployReadyTimeout)
	waitForCadvisorMetrics(t, ns)

	deployName := "env-app"
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "env-policy", Namespace: ns},
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
				MinAllowed:       quantityPtr("200m"),
				MaxAllowed:       quantityPtr("400m"),
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "30",
				AllowDecrease:    boolPtr(false),
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

	startCPU := resource.MustParse("100m")
	if caps.InPlacePodLevelResources {
		waitForEnvelopeCPUIncrease(t, "env-policy", ns, "env-app", startCPU, 4*time.Minute)
		return
	}

	waitForEnvelopeSkip(t, "env-policy", ns, "env-app", startCPU, 3*time.Minute)
}

func createDeploymentWithEnvelope(t *testing.T, name, namespace, cpuReq, memReq string) *appsv1.Deployment {
	t.Helper()
	cpu := resource.MustParse(cpuReq)
	mem := resource.MustParse(memReq)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": name},
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
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    cpu,
							corev1.ResourceMemory: mem,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "registry.k8s.io/pause:3.9",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpu,
									corev1.ResourceMemory: mem,
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

func waitForEnvelopeCPUIncrease(t *testing.T, policyName, namespace, app string, startCPU resource.Quantity, timeout time.Duration) {
	t.Helper()
	lastTouch := time.Now()
	lastDiag := time.Time{}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		if time.Since(lastTouch) > 45*time.Second {
			touchPolicySpec(t, policyName, namespace)
			lastTouch = time.Now()
		}
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + app,
		})
		if err != nil {
			return false, nil
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Spec.Resources == nil {
				continue
			}
			envCPU := pod.Spec.Resources.Requests.Cpu()
			if envCPU == nil || envCPU.Cmp(startCPU) <= 0 {
				continue
			}
			for _, c := range pod.Spec.Containers {
				if c.Name != "app" {
					continue
				}
				cpu := c.Resources.Requests.Cpu()
				if cpu != nil && cpu.Cmp(startCPU) > 0 {
					t.Logf("pod %s container cpu=%s envelope cpu=%s (from %s)",
						pod.Name, cpu.String(), envCPU.String(), startCPU.String())
					return true, nil
				}
			}
		}
		if time.Since(lastDiag) > 30*time.Second {
			lastDiag = time.Now()
			logPolicyExplanationState(t, policyName, namespace)
			for i := range pods.Items {
				p := &pods.Items[i]
				env := "<nil>"
				if p.Spec.Resources != nil {
					env = p.Spec.Resources.Requests.Cpu().String()
				}
				for _, c := range p.Spec.Containers {
					if c.Name != "app" {
						continue
					}
					t.Logf("  pod %s phase=%s cpu=%s envelope=%s",
						p.Name, p.Status.Phase, c.Resources.Requests.Cpu().String(), env)
				}
			}
		}
		return false, nil
	}), "expected container increase and raised spec.resources in the same resize")
}

func waitForEnvelopeSkip(t *testing.T, policyName, namespace, app string, startCPU resource.Quantity, timeout time.Duration) {
	t.Helper()
	lastTouch := time.Now()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		if time.Since(lastTouch) > 45*time.Second {
			touchPolicySpec(t, policyName, namespace)
			lastTouch = time.Now()
		}
		var policy attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: namespace}, &policy); err != nil {
			return false, nil
		}
		for _, h := range policy.Status.ResizeHistory {
			if h.Reason == resize.ReasonEnvelopeConstraint {
				return true, nil
			}
		}
		return false, nil
	}), "expected history reason envelope_constraint")

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + app,
	})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items)
	for _, c := range pods.Items[0].Spec.Containers {
		if c.Name != "app" {
			continue
		}
		cpu := c.Resources.Requests.Cpu()
		require.NotNil(t, cpu)
		assert.False(t, cpu.Cmp(startCPU) > 0,
			"in-place off must skip an increase past the envelope, got %s", cpu.String())
	}
}
