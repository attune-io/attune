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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
)

// ---------- nativeSidecars ----------

func TestWorkload_NativeSidecars(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways

	tests := []struct {
		name  string
		init  []corev1.Container
		want  int
		names []string
	}{
		{
			name: "empty list",
			init: nil,
			want: 0,
		},
		{
			name: "regular init containers only",
			init: []corev1.Container{
				{Name: "init-db"},
				{Name: "init-config"},
			},
			want: 0,
		},
		{
			name: "sidecar init containers",
			init: []corev1.Container{
				{Name: "envoy", RestartPolicy: &always},
				{Name: "log-forwarder", RestartPolicy: &always},
			},
			want:  2,
			names: []string{"envoy", "log-forwarder"},
		},
		{
			name: "mixed init and sidecar containers",
			init: []corev1.Container{
				{Name: "init-db"},
				{Name: "envoy", RestartPolicy: &always},
				{Name: "init-config"},
			},
			want:  1,
			names: []string{"envoy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nativeSidecars(tt.init)
			assert.Len(t, got, tt.want)
			for i, n := range tt.names {
				assert.Equal(t, n, got[i].Name)
			}
		})
	}
}

// ---------- isBatchWorkload ----------

func TestWorkload_IsBatchWorkload(t *testing.T) {
	tests := []struct {
		name     string
		workload client.Object
		want     bool
	}{
		{"Deployment", &appsv1.Deployment{}, false},
		{"StatefulSet", &appsv1.StatefulSet{}, false},
		{"DaemonSet", &appsv1.DaemonSet{}, false},
		{"Job", &batchv1.Job{}, true},
		{"CronJob", &batchv1.CronJob{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isBatchWorkload(tt.workload))
		})
	}
}

// ---------- getContainers ----------

func TestWorkload_GetContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	r := &RightSizePolicyReconciler{}

	tests := []struct {
		name      string
		workload  client.Object
		wantNames []string
	}{
		{
			name: "Deployment with containers",
			workload: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "app"},
								{Name: "sidecar"},
							},
						},
					},
				},
			},
			wantNames: []string{"app", "sidecar"},
		},
		{
			name: "Deployment with init and native sidecar containers",
			workload: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{
								{Name: "init-db"},
								{Name: "envoy", RestartPolicy: &always},
							},
							Containers: []corev1.Container{
								{Name: "app"},
							},
						},
					},
				},
			},
			wantNames: []string{"envoy", "app"},
		},
		{
			name: "CronJob containers",
			workload: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "worker"},
									},
								},
							},
						},
					},
				},
			},
			wantNames: []string{"worker"},
		},
		{
			name:      "nil workload",
			workload:  nil,
			wantNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getContainers(tt.workload)
			if tt.wantNames == nil {
				assert.Nil(t, got)
				return
			}
			require.Len(t, got, len(tt.wantNames))
			for i, n := range tt.wantNames {
				assert.Equal(t, n, got[i].Name)
			}
		})
	}
}

// ---------- isRollingOut ----------

func TestWorkload_IsRollingOut(t *testing.T) {
	r := &RightSizePolicyReconciler{}

	tests := []struct {
		name     string
		workload client.Object
		want     bool
	}{
		{
			name: "Deployment fully rolled out",
			workload: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 3, AvailableReplicas: 3},
			},
			want: false,
		},
		{
			name: "Deployment mid-rollout updatedReplicas < replicas",
			workload: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 1, AvailableReplicas: 3},
			},
			want: true,
		},
		{
			name: "Deployment unavailable replicas",
			workload: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 3, AvailableReplicas: 1},
			},
			want: true,
		},
		{
			name: "StatefulSet mid-rollout",
			workload: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: int32Ptr(5)},
				Status: appsv1.StatefulSetStatus{UpdatedReplicas: 2},
			},
			want: true,
		},
		{
			name: "StatefulSet fully rolled out",
			workload: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: int32Ptr(3)},
				Status: appsv1.StatefulSetStatus{UpdatedReplicas: 3},
			},
			want: false,
		},
		{
			name: "DaemonSet mid-rollout",
			workload: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{
					DesiredNumberScheduled: 5,
					UpdatedNumberScheduled: 3,
				},
			},
			want: true,
		},
		{
			name: "DaemonSet fully rolled out",
			workload: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{
					DesiredNumberScheduled: 3,
					UpdatedNumberScheduled: 3,
				},
			},
			want: false,
		},
		{
			name:     "Job always false",
			workload: &batchv1.Job{},
			want:     false,
		},
		{
			name:     "CronJob always false",
			workload: &batchv1.CronJob{},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.isRollingOut(tt.workload))
		})
	}
}

// ---------- getPodSelectorLabels ----------

func TestWorkload_GetPodSelectorLabels(t *testing.T) {
	r := &RightSizePolicyReconciler{}

	tests := []struct {
		name     string
		workload client.Object
		want     map[string]string
	}{
		{
			name: "Deployment with selector",
			workload: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
				},
			},
			want: map[string]string{"app": "web"},
		},
		{
			name: "CronJob with selector",
			workload: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"job": "batch"},
							},
						},
					},
				},
			},
			want: map[string]string{"job": "batch"},
		},
		{
			name: "CronJob without selector falls back to template labels",
			workload: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{"tmpl": "label"},
								},
							},
						},
					},
				},
			},
			want: map[string]string{"tmpl": "label"},
		},
		{
			name: "Job without selector falls back to template labels",
			workload: &batchv1.Job{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"run": "once"},
						},
					},
				},
			},
			want: map[string]string{"run": "once"},
		},
		{
			name: "Job with selector uses selector",
			workload: &batchv1.Job{
				Spec: batchv1.JobSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"sel": "yes"},
					},
				},
			},
			want: map[string]string{"sel": "yes"},
		},
		{
			name:     "Deployment without selector returns nil",
			workload: &appsv1.Deployment{},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getPodSelectorLabels(tt.workload)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------- newWorkloadAdapter ----------

func TestWorkload_NewWorkloadAdapter(t *testing.T) {
	tests := []struct {
		name     string
		obj      client.Object
		wantType string
		wantNil  bool
	}{
		{"Deployment", &appsv1.Deployment{}, "*controller.deploymentAdapter", false},
		{"StatefulSet", &appsv1.StatefulSet{}, "*controller.statefulSetAdapter", false},
		{"DaemonSet", &appsv1.DaemonSet{}, "*controller.daemonSetAdapter", false},
		{"CronJob", &batchv1.CronJob{}, "*controller.cronJobAdapter", false},
		{"Job", &batchv1.Job{}, "*controller.jobAdapter", false},
		{"ReplicaSet", &appsv1.ReplicaSet{}, "*controller.replicaSetAdapter", false},
		{"nil returns nil", nil, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newWorkloadAdapter(tt.obj)
			if tt.wantNil {
				assert.Nil(t, a)
				return
			}
			require.NotNil(t, a)
			assert.Equal(t, tt.obj, a.Object())
		})
	}
}

// ---------- workloadKinds registry ----------

func TestWorkload_RegistryCoversAllKinds(t *testing.T) {
	kinds := []string{"Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "ReplicaSet"}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			wk, ok := workloadKinds[kind]
			require.True(t, ok, "workloadKinds missing %s", kind)
			assert.NotNil(t, wk.newObject())
			assert.NotNil(t, wk.newList())
		})
	}
}

func TestWorkload_RegistryUnsupportedKind(t *testing.T) {
	_, ok := workloadKinds["ConfigMap"]
	assert.False(t, ok)
}

// ---------- buildPrometheusQuery ----------

func TestWorkload_BuildPrometheusQuery(t *testing.T) {
	defaultWindow := 5 * time.Minute
	tests := []struct {
		name       string
		namespace  string
		podRegex   string
		container  string
		metric     string
		rateWindow time.Duration
		want       string
	}{
		{
			name:       "cpu metric with deployment regex",
			namespace:  "prod",
			podRegex:   "my-app-[a-z0-9]+-[a-z0-9]{5}",
			metric:     "cpu",
			rateWindow: defaultWindow,
			want:       `rate(container_cpu_usage_seconds_total{namespace="prod",pod=~"my-app-[a-z0-9]+-[a-z0-9]{5}"}[5m])`,
		},
		{
			name:       "cpu metric with container filter",
			namespace:  "prod",
			podRegex:   "my-app-[a-z0-9]+-[a-z0-9]{5}",
			container:  "web",
			metric:     "cpu",
			rateWindow: defaultWindow,
			want:       `rate(container_cpu_usage_seconds_total{namespace="prod",pod=~"my-app-[a-z0-9]+-[a-z0-9]{5}",container="web"}[5m])`,
		},
		{
			name:       "cpu metric with 15m rate window",
			namespace:  "prod",
			podRegex:   "my-app-.*",
			metric:     "cpu",
			rateWindow: 15 * time.Minute,
			want:       `rate(container_cpu_usage_seconds_total{namespace="prod",pod=~"my-app-.*"}[15m])`,
		},
		{
			name:       "cpu metric with 1h rate window",
			namespace:  "prod",
			podRegex:   "app-.*",
			metric:     "cpu",
			rateWindow: time.Hour,
			want:       `rate(container_cpu_usage_seconds_total{namespace="prod",pod=~"app-.*"}[1h])`,
		},
		{
			name:       "memory metric with statefulset regex",
			namespace:  "staging",
			podRegex:   "worker-[0-9]+",
			metric:     "memory",
			rateWindow: defaultWindow,
			want:       `container_memory_working_set_bytes{namespace="staging",pod=~"worker-[0-9]+"}`,
		},
		{
			name:       "memory metric with container filter",
			namespace:  "staging",
			podRegex:   "worker-[0-9]+",
			container:  "main",
			metric:     "memory",
			rateWindow: defaultWindow,
			want:       `container_memory_working_set_bytes{namespace="staging",pod=~"worker-[0-9]+",container="main"}`,
		},
		{
			name:       "unknown metric returns empty",
			namespace:  "ns",
			podRegex:   "p.*",
			metric:     "disk",
			rateWindow: defaultWindow,
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPrometheusQuery(tt.namespace, tt.podRegex, tt.container, tt.metric, tt.rateWindow)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWorkload_FormatPromDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{15 * time.Minute, "15m"},
		{time.Hour, "1h"},
		{2 * time.Hour, "2h"},
		{90 * time.Second, "90s"},
		{0, "5m"},
		{-1, "5m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, formatPromDuration(tt.d))
		})
	}
}

// ---------- discoverWorkloads ----------

func TestWorkload_DiscoverWorkloads(t *testing.T) {
	ctx := context.Background()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			Labels:    map[string]string{"team": "backend"},
		},
	}

	tests := []struct {
		name    string
		policy  *rightsizev1alpha1.RightSizePolicy
		objects []client.Object
		wantLen int
		wantNil bool
		wantErr string
	}{
		{
			name: "by name found",
			policy: &rightsizev1alpha1.RightSizePolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: rightsizev1alpha1.RightSizePolicySpec{
					TargetRef: rightsizev1alpha1.TargetRef{
						Kind: "Deployment",
						Name: stringPtr("web"),
					},
				},
			},
			objects: []client.Object{dep},
			wantLen: 1,
		},
		{
			name: "by name not found returns nil",
			policy: &rightsizev1alpha1.RightSizePolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: rightsizev1alpha1.RightSizePolicySpec{
					TargetRef: rightsizev1alpha1.TargetRef{
						Kind: "Deployment",
						Name: stringPtr("missing"),
					},
				},
			},
			objects: []client.Object{dep},
			wantNil: true,
		},
		{
			name: "by selector matches",
			policy: &rightsizev1alpha1.RightSizePolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: rightsizev1alpha1.RightSizePolicySpec{
					TargetRef: rightsizev1alpha1.TargetRef{
						Kind: "Deployment",
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"team": "backend"},
						},
					},
				},
			},
			objects: []client.Object{dep},
			wantLen: 1,
		},
		{
			name: "neither name nor selector returns error",
			policy: &rightsizev1alpha1.RightSizePolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: rightsizev1alpha1.RightSizePolicySpec{
					TargetRef: rightsizev1alpha1.TargetRef{
						Kind: "Deployment",
					},
				},
			},
			wantErr: "targetRef must specify either name or selector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objects) > 0 {
				builder = builder.WithObjects(tt.objects...)
			}
			r := &RightSizePolicyReconciler{Client: builder.Build()}

			got, err := r.discoverWorkloads(ctx, tt.policy)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.Len(t, got, tt.wantLen)
		})
	}
}

// ---------- getPodsForWorkload ----------

func TestWorkload_GetPodsForWorkload(t *testing.T) {
	ctx := context.Background()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
		},
	}

	matchPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
	}

	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-xyz",
			Namespace: "default",
			Labels:    map[string]string{"app": "other"},
		},
	}

	tests := []struct {
		name     string
		workload client.Object
		objects  []client.Object
		wantLen  int
		wantErr  string
	}{
		{
			name:     "finds matching pods",
			workload: dep,
			objects:  []client.Object{dep, matchPod, otherPod},
			wantLen:  1,
		},
		{
			name: "workload with no selector labels returns error",
			workload: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
			},
			wantErr: "workload default/empty has no pod selector labels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objects) > 0 {
				builder = builder.WithObjects(tt.objects...)
			}
			r := &RightSizePolicyReconciler{Client: builder.Build()}

			pods, err := r.getPodsForWorkload(ctx, tt.workload)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, pods, tt.wantLen)
		})
	}
}

// ---------- Adapter method coverage ----------

func TestWorkload_AdapterPodSpec(t *testing.T) {
	cpuReq := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}

	tests := []struct {
		name string
		obj  client.Object
	}{
		{"Job", &batchv1.Job{
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "worker", Resources: corev1.ResourceRequirements{Requests: cpuReq}}},
					},
				},
			},
		}},
		{"CronJob", &batchv1.CronJob{
			Spec: batchv1.CronJobSpec{
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "cron-worker", Resources: corev1.ResourceRequirements{Requests: cpuReq}}},
							},
						},
					},
				},
			},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newWorkloadAdapter(tt.obj)
			require.NotNil(t, a)
			ps := a.PodSpec()
			require.NotNil(t, ps)
			assert.NotEmpty(t, ps.Containers)
			assert.True(t, a.IsBatch())
			assert.False(t, a.IsRollingOut())
			assert.NotEmpty(t, a.PodNameRegexSuffix())
		})
	}
}

func TestWorkload_AdapterPodSelectorLabels(t *testing.T) {
	tests := []struct {
		name     string
		obj      client.Object
		wantNil  bool
		wantKeys []string
	}{
		{"Job with selector", &batchv1.Job{
			Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job": "test"}},
			},
		}, false, []string{"job"}},
		{"Job without selector (falls back to template labels)", &batchv1.Job{
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
				},
			},
		}, false, []string{"app"}},
		{"CronJob without selector (falls back to template labels)", &batchv1.CronJob{
			Spec: batchv1.CronJobSpec{
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "cron"}},
						},
					},
				},
			},
		}, false, []string{"app"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newWorkloadAdapter(tt.obj)
			require.NotNil(t, a)
			labels := a.PodSelectorLabels()
			if tt.wantNil {
				assert.Nil(t, labels)
			} else {
				for _, k := range tt.wantKeys {
					assert.Contains(t, labels, k)
				}
			}
		})
	}
}

func TestWorkload_JobIndexedCompletionRegex(t *testing.T) {
	indexed := batchv1.IndexedCompletion
	job := &batchv1.Job{
		Spec: batchv1.JobSpec{CompletionMode: &indexed},
	}
	a := newWorkloadAdapter(job)
	require.NotNil(t, a)
	assert.Contains(t, a.PodNameRegexSuffix(), "[0-9]+")

	cronJob := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{CompletionMode: &indexed},
			},
		},
	}
	ca := newWorkloadAdapter(cronJob)
	require.NotNil(t, ca)
	assert.Contains(t, ca.PodNameRegexSuffix(), "[0-9]+")
}
