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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestDiscoverWorkloads_FindsDeploymentByName(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})
	reconciler := newReconcilerWithClient(deploy)

	policy := newTestPolicy("test-policy", "default")

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "api-server", workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsDeploymentsByLabelSelector(t *testing.T) {
	deploy1 := newTestDeployment("api-server-1", "default", map[string]string{"tier": "api"})
	deploy2 := newTestDeployment("api-server-2", "default", map[string]string{"tier": "api"})
	deploy3 := newTestDeployment("worker", "default", map[string]string{"tier": "worker"})
	reconciler := newReconcilerWithClient(deploy1, deploy2, deploy3)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "api"},
				},
			},
		},
	}

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Len(t, workloads, 2)

	names := make(map[string]bool)
	for _, w := range workloads {
		names[w.GetName()] = true
	}
	assert.True(t, names["api-server-1"])
	assert.True(t, names["api-server-2"])
	assert.False(t, names["worker"])
}

func TestDiscoverWorkloads_ReturnsEmptyForNonMatchingSelector(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", map[string]string{"tier": "api"})
	reconciler := newReconcilerWithClient(deploy)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "nonexistent"},
				},
			},
		},
	}

	workloads, err := reconciler.discoverWorkloads(context.Background(), policy)
	require.NoError(t, err)
	assert.Empty(t, workloads)
}

func TestGetPodsForWorkload_MatchExpressionsExcludeCanary(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "track", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"canary"},
				}},
			},
		},
	}
	stable := newTestPod("web-stable", "default", map[string]string{"app": "web", "track": "stable"})
	canary := newTestPod("web-canary", "default", map[string]string{"app": "web", "track": "canary"})
	exprOnly := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "expr", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"api"},
				}},
			},
		},
	}
	apiPod := newTestPod("expr-api", "default", map[string]string{"tier": "api"})
	other := newTestPod("expr-other", "default", map[string]string{"tier": "batch"})
	reconciler := newReconcilerWithClient(deploy, stable, canary, exprOnly, apiPod, other)

	pods, err := reconciler.getPodsForWorkload(context.Background(), deploy)
	require.NoError(t, err)
	require.Len(t, pods, 1)
	assert.Equal(t, "web-stable", pods[0].Name)

	pods, err = reconciler.getPodsForWorkload(context.Background(), exprOnly)
	require.NoError(t, err, "matchExpressions-only selector is a pod selector")
	require.Len(t, pods, 1)
	assert.Equal(t, "expr-api", pods[0].Name)

	listed := reconciler.listPodsForWorkloads(context.Background(), []client.Object{deploy})
	require.Len(t, listed["web"], 1)
	assert.Equal(t, "web-stable", listed["web"][0].Name)
}

func TestGetPodsForWorkload_ReturnsMatchingPods(t *testing.T) {
	deploy := newTestDeployment("api-server", "default", nil)
	pod1 := newTestPod("api-server-abc-123", "default", map[string]string{"app": "api-server"})
	pod2 := newTestPod("api-server-abc-456", "default", map[string]string{"app": "api-server"})
	pod3 := newTestPod("worker-def-789", "default", map[string]string{"app": "worker"})
	reconciler := newReconcilerWithClient(deploy, pod1, pod2, pod3)

	pods, err := reconciler.getPodsForWorkload(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	podNames := make(map[string]bool)
	for _, p := range pods {
		podNames[p.Name] = true
	}
	assert.True(t, podNames["api-server-abc-123"])
	assert.True(t, podNames["api-server-abc-456"])
	assert.False(t, podNames["worker-def-789"])
}

func TestDiscoverWorkloads_FindsStatefulSetByName(t *testing.T) {
	name := "my-sts"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sts", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-sts"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-sts"}},
			},
		},
	}
	r := newReconcilerWithClient(sts)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "StatefulSet",
				Name: &name,
			},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "my-sts", workloads[0].GetName())
}

// ---------- listWorkloadsBySelector (StatefulSet + DaemonSet paths) ----------

func TestListWorkloadsBySelector_StatefulSets(t *testing.T) {
	sts1 := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "default", Labels: map[string]string{"tier": "db"}},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db-1"}},
		},
	}
	sts2 := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db-2", Namespace: "default", Labels: map[string]string{"tier": "db"}},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db-2"}},
		},
	}
	r := newReconcilerWithClient(sts1, sts2)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "db"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "StatefulSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 2)
}

func TestListWorkloadsBySelector_DaemonSets(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "log-agent", Namespace: "default", Labels: map[string]string{"role": "logging"}},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "log-agent"}},
		},
	}
	r := newReconcilerWithClient(ds)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "logging"}}
	workloads, err := r.listWorkloadsBySelector(context.Background(), "default", "DaemonSet", selector)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, "log-agent", workloads[0].GetName())
}

func TestListWorkloadsBySelector_UnsupportedKind(t *testing.T) {
	r := newReconcilerWithClient()

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "ConfigMap", selector)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// ---------- getWorkloadByName (DaemonSet + unsupported kind) ----------

func TestDiscoverWorkloads_FindsDaemonSetByName(t *testing.T) {
	name := "node-agent"
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
	r := newReconcilerWithClient(ds)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "DaemonSet",
				Name: &name,
			},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	assert.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsCronJobByName(t *testing.T) {
	name := "nightly-report"
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "report", Image: "report:latest"}},
						},
					},
				},
			},
		},
	}
	r := newReconcilerWithClient(cj)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "CronJob", Name: &name},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestDiscoverWorkloads_FindsJobByName(t *testing.T) {
	name := "data-migration"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "migrate", Image: "migrate:latest"}},
				},
			},
		},
	}
	r := newReconcilerWithClient(job)

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{Kind: "Job", Name: &name},
		},
	}

	workloads, err := r.discoverWorkloads(context.Background(), policy)
	assert.NoError(t, err)
	require.Len(t, workloads, 1)
	assert.Equal(t, name, workloads[0].GetName())
}

func TestGetPodSelectorLabels_CronJob(t *testing.T) {
	r := NewAttunePolicyReconciler()
	cj := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "report"}},
					},
				},
			},
		},
	}
	labels := r.getPodSelectorLabels(cj)
	assert.Equal(t, map[string]string{"app": "report"}, labels)
}

func TestListWorkloadsBySelector_CronJobs(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "report", Namespace: "default", Labels: map[string]string{"tier": "batch"}},
	}
	r := newReconcilerWithClient(cj)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "batch"}}
	result, err := r.listWorkloadsBySelector(context.Background(), "default", "CronJob", selector)
	assert.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "report", result[0].GetName())
}

func TestListWorkloadsBySelector_Jobs(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "default", Labels: map[string]string{"tier": "batch"}},
	}
	r := newReconcilerWithClient(job)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "batch"}}
	result, err := r.listWorkloadsBySelector(context.Background(), "default", "Job", selector)
	assert.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "migrate", result[0].GetName())
}

func TestDiscoverWorkloads_UnsupportedKind(t *testing.T) {
	name := "my-configmap"
	r := newReconcilerWithClient()

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "ConfigMap",
				Name: &name,
			},
		},
	}

	_, err := r.discoverWorkloads(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// ---------- getPodSelectorLabels (StatefulSet + DaemonSet) ----------

func TestGetPodSelectorLabels_StatefulSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	sts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
		},
	}
	labels := r.getPodSelectorLabels(sts)
	assert.Equal(t, map[string]string{"app": "db"}, labels)
}

func TestGetPodSelectorLabels_DaemonSet(t *testing.T) {
	r := NewAttunePolicyReconciler()
	ds := &appsv1.DaemonSet{
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
		},
	}
	labels := r.getPodSelectorLabels(ds)
	assert.Equal(t, map[string]string{"app": "agent"}, labels)
}

func TestGetPodSelectorLabels_NilSelector(t *testing.T) {
	r := NewAttunePolicyReconciler()
	dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{}}
	labels := r.getPodSelectorLabels(dep)
	assert.Nil(t, labels)
}

// ---------- getPodsForWorkload error path ----------

func TestGetPodsForWorkload_EmptySelectorLabels(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "no-selector", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{},
	}
	reconciler := newReconcilerWithClient(deploy)

	_, err := reconciler.getPodsForWorkload(context.Background(), deploy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no pod selector labels")
}

// ---------- listWorkloadsBySelector invalid selector ----------

func TestListWorkloadsBySelector_InvalidSelector(t *testing.T) {
	r := newReconcilerWithClient()

	// matchExpressions with invalid operator to trigger parse error.
	invalidSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tier", Operator: "BadOperator", Values: []string{"api"}},
		},
	}
	_, err := r.listWorkloadsBySelector(context.Background(), "default", "Deployment", invalidSelector)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing label selector")
}

// ---------- discoverWorkloads with missing name ----------

func TestDiscoverWorkloads_NoNameOrSelector(t *testing.T) {
	r := newReconcilerWithClient()

	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			TargetRef: attunev1alpha1.TargetRef{
				Kind: "Deployment",
			},
		},
	}

	_, err := r.discoverWorkloads(context.Background(), policy)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must specify either name or selector")
}
