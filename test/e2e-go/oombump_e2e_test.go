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
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// TestE2E_OOMBump_RaisesMemoryWithoutPriorResize is the remaining #878
// case. TestE2E_OOMKill_TriggersRevert resizes CPU first and pins memory
// at 64Mi, so it cannot show a bump of the original request.
func TestE2E_OOMBump_RaisesMemoryWithoutPriorResize(t *testing.T) {
	t.Parallel()
	ns := uniqueNS("oombump")
	createNamespace(t, ns)

	const (
		appName    = "oom-bump-app"
		policyName = "oom-bump-policy"
		replicas   = int32(3)
	)
	startMem := resource.MustParse("64Mi")
	startCPU := resource.MustParse("100m")
	memLimit := resource.MustParse("512Mi")
	maxMem := resource.MustParse("1Gi")
	minCPU := startCPU.DeepCopy()
	maxCPU := startCPU.DeepCopy()
	minMem := startMem.DeepCopy()
	requestsOnly := attunev1alpha1.ControlledRequestsOnly
	includeExpl := true

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: ns,
			Labels:    map[string]string{"app": appName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "app",
						Image:   stressNGImage,
						Command: []string{"/stress-ng", "--sleep", "1", "--timeout", "3600"},
						ResizePolicy: []corev1.ContainerResizePolicy{
							{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
							// NotRequired keeps lastState.terminated after an
							// in-place memory change. RestartContainer would
							// replace the OOM evidence.
							{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    startCPU.DeepCopy(),
								corev1.ResourceMemory: startMem.DeepCopy(),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: memLimit.DeepCopy(),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForDeploymentReady(t, appName, ns, 180*time.Second)
	requireFullyUpdated(t, appName, ns, replicas)

	deployName := appName
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Deployment", Name: &deployName},
			MetricsSource: attunev1alpha1.MetricsSource{
				Prometheus:        &attunev1alpha1.PrometheusConfig{Address: promAddr},
				MinimumDataPoints: int32Ptr(1),
				HistoryWindow:     &metav1.Duration{Duration: time.Hour},
				QueryStep:         &metav1.Duration{Duration: 15 * time.Second},
				RateWindow:        &metav1.Duration{Duration: time.Minute},
			},
			CPU: attunev1alpha1.ResourceConfig{
				Percentile:       95,
				Overhead:         "0",
				ControlledValues: &requestsOnly,
				// Pin CPU at the start request so the first change cannot be a CPU resize.
				MinAllowed:       &minCPU,
				MaxAllowed:       &maxCPU,
				MaxChangePercent: int32Ptr(100),
			},
			Memory: attunev1alpha1.ResourceConfig{
				Percentile:       99,
				Overhead:         "0",
				AllowDecrease:    boolPtr(false),
				ControlledValues: &requestsOnly,
				MinAllowed:       &minMem,
				// Headroom for the bump. A max equal to the start request would clamp it away.
				MaxAllowed:       &maxMem,
				MaxChangePercent: int32Ptr(100),
				OOMBump:          &attunev1alpha1.OOMBump{},
			},
			UpdateStrategy: &attunev1alpha1.UpdateStrategy{
				Type:                        attunev1alpha1.UpdateTypeAuto,
				Cooldown:                    &metav1.Duration{Duration: time.Minute},
				AutoRevert:                  boolPtr(true),
				IncludeExplanationsInStatus: &includeExpl,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, policy))
	requireMemoryStill(t, ns, appName, startMem)

	oomPod := runningPodName(t, ns, appName)
	t.Logf("exec OOM stressor into %s", oomPod)
	go execOOM(t, ns, oomPod)

	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, listErr := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + appName,
		})
		if listErr != nil {
			return false, nil
		}
		dep, getErr := clientset.AppsV1().Deployments(ns).Get(ctx, appName, metav1.GetOptions{})
		if getErr != nil {
			return false, nil
		}
		var pol attunev1alpha1.AttunePolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &pol); err != nil {
			return false, nil
		}
		if dep.Status.UpdatedReplicas != replicas || dep.Status.ReadyReplicas < replicas-1 {
			return false, nil
		}
		oomRaised := false
		siblingRaised := false
		for i := range pods.Items {
			pod := &pods.Items[i]
			mem := containerMemoryRequest(pod, "app")
			if mem == nil || mem.Cmp(startMem) <= 0 {
				continue
			}
			if podOOMKilled(pod, "app") {
				raw := pod.Annotations["attune.io/oom-bump.app"]
				if strings.HasPrefix(raw, "count=1,") {
					oomRaised = true
				}
				continue
			}
			siblingRaised = true
		}
		if !oomRaised || !siblingRaised || !recommendationIsOOMBump(&pol, appName, startMem) {
			return false, nil
		}
		t.Logf("oom bump applied: updated=%d ready=%d", dep.Status.UpdatedReplicas, dep.Status.ReadyReplicas)
		return true, nil
	})
	if err != nil {
		logOOMBumpState(t, policyName, ns, appName)
	}
	require.NoError(t, err, "timed out waiting for an OOM bump of the original memory request")
}

func requireFullyUpdated(t *testing.T, name, namespace string, replicas int32) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		dep, getErr := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, nil
		}
		return dep.Status.UpdatedReplicas == replicas && dep.Status.ReadyReplicas == replicas, nil
	})
	require.NoError(t, err, "deployment %s/%s was not fully updated", namespace, name)
}

func requireMemoryStill(t *testing.T, namespace, app string, start resource.Quantity) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
		require.NoError(t, err)
		for i := range pods.Items {
			mem := containerMemoryRequest(&pods.Items[i], "app")
			require.NotNil(t, mem)
			require.Equal(t, 0, mem.Cmp(start), "pod %s memory changed before the OOM: %s", pods.Items[i].Name, mem.String())
			_, bumped := pods.Items[i].Annotations["attune.io/oom-bump.app"]
			require.False(t, bumped, "pod %s already has an oom bump annotation", pods.Items[i].Name)
		}
		time.Sleep(2 * time.Second)
	}
}

func runningPodName(t *testing.T, namespace, app string) string {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
	require.NoError(t, err)
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return pods.Items[i].Name
		}
	}
	t.Fatal("no running pod to OOM")
	return ""
}

func execOOM(t *testing.T, namespace, podName string) {
	t.Helper()
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
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
		t.Logf("exec setup error (expected if the container dies): %v", err)
		return
	}
	_ = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: io.Discard, Stderr: io.Discard})
}

func containerMemoryRequest(pod *corev1.Pod, name string) *resource.Quantity {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != name {
			continue
		}
		q := pod.Spec.Containers[i].Resources.Requests.Memory()
		if q == nil {
			return nil
		}
		out := q.DeepCopy()
		return &out
	}
	return nil
}

func podOOMKilled(pod *corev1.Pod, name string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != name {
			continue
		}
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			return true
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

func recommendationIsOOMBump(policy *attunev1alpha1.AttunePolicy, workload string, start resource.Quantity) bool {
	for _, rec := range policy.Status.Recommendations {
		if rec.Workload != workload {
			continue
		}
		for _, c := range rec.Containers {
			if c.Name != "app" || c.Recommended.MemoryRequest.Cmp(start) <= 0 {
				continue
			}
			if c.Explanation != nil && c.Explanation.Memory != nil &&
				strings.Contains(c.Explanation.Memory.FinalAdjustment, "oomBump") {
				return true
			}
		}
	}
	return false
}

func logOOMBumpState(t *testing.T, policyName, namespace, app string) {
	t.Helper()
	var pol attunev1alpha1.AttunePolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: namespace}, &pol); err != nil {
		t.Logf("oom bump diag: get policy: %v", err)
	} else {
		for _, rec := range pol.Status.Recommendations {
			for _, c := range rec.Containers {
				adj := ""
				if c.Explanation != nil && c.Explanation.Memory != nil {
					adj = c.Explanation.Memory.FinalAdjustment
				}
				t.Logf("oom bump diag: rec %s/%s mem=%s adj=%s stale=%v",
					rec.Workload, c.Name, c.Recommended.MemoryRequest.String(), adj, rec.Stale)
			}
		}
		for i, h := range pol.Status.ResizeHistory {
			t.Logf("oom bump diag: history[%d] %s %s %s %s", i, h.Workload, h.Resource, h.Result, h.Reason)
		}
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Logf("oom bump diag: list pods: %v", err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		mem := containerMemoryRequest(pod, "app")
		t.Logf("oom bump diag: pod %s phase=%s mem=%v oom=%v ann=%q",
			pod.Name, pod.Status.Phase, mem, podOOMKilled(pod, "app"), pod.Annotations["attune.io/oom-bump.app"])
	}
}
