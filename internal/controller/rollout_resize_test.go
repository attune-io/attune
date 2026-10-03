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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func rolloutSamples(now time.Time) *mockCollector {
	samples := make([]rsmetrics.Sample, 60)
	for i := range samples {
		samples[i] = rsmetrics.Sample{
			Timestamp: now.Add(-time.Duration(60-i) * 5 * time.Minute),
			Value:     0.1,
		}
	}
	grouped := map[string][]rsmetrics.Sample{"main": samples}
	return &mockCollector{
		queryRangeGroupedFunc: func(context.Context, string, time.Time, time.Time, time.Duration) (map[string][]rsmetrics.Sample, error) {
			return grouped, nil
		},
	}
}

func requireNonStaleRec(t *testing.T, result workloadProcessingResult) {
	t.Helper()
	require.Len(t, result.recommendations, 1)
	require.False(t, result.recommendations[0].Stale)
	require.Greater(t, result.workloadsWithRecs, int32(0))
}

func runRolloutProcess(r *AttunePolicyReconciler, policy *attunev1alpha1.AttunePolicy, w client.Object, now time.Time) workloadProcessingResult {
	collector := rolloutSamples(now)
	r.MetricsFactory = mockMetricsFactory(collector)
	r.SetNowFunc(func() time.Time { return now })
	policy.Spec.TargetRef.Name = nil
	policy.Spec.TargetRef.Selector = &metav1.LabelSelector{}
	return r.processWorkloads(context.Background(), policy, []client.Object{w}, collector, nil, nil)
}

func newRolloutReconciler(objs []client.Object, pods []*corev1.Pod) (*AttunePolicyReconciler, *events.FakeRecorder) {
	scheme := testScheme()
	all := make([]client.Object, 0, len(objs)+len(pods))
	all = append(all, objs...)
	runtimePods := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		all = append(all, p)
		runtimePods = append(runtimePods, p.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	r := NewAttunePolicyReconciler()
	r.Client = c
	r.Scheme = scheme
	r.Clientset = kubefake.NewSimpleClientset(runtimePods...)
	rec := events.NewFakeRecorder(20)
	r.Recorder = rec
	return r, rec
}

func burstableResizePod(name, workload string) *corev1.Pod {
	pod := newResizePod(workload, "100m", "128Mi", "500m", "256Mi")
	pod.Name = name
	pod.Status.QOSClass = corev1.PodQOSBurstable
	pod.Status.Phase = corev1.PodRunning
	return pod
}

func risingCPURecommendation(workload string) attunev1alpha1.WorkloadRecommendation {
	return newResizeRecommendation(workload, "100m", "128Mi", "500m", "256Mi", "200m", "128Mi", "500m", "256Mi")
}

func resizedPodNames(cs *kubefake.Clientset) []string {
	var names []string
	for _, action := range cs.Actions() {
		if action.GetVerb() != "update" || action.GetSubresource() != "resize" {
			continue
		}
		upd, ok := action.(k8stesting.UpdateAction)
		if !ok {
			continue
		}
		pod, ok := upd.GetObject().(*corev1.Pod)
		if !ok || pod == nil {
			continue
		}
		names = append(names, pod.Name)
	}
	return names
}

func recordedEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func countSubstr(items []string, needle string) int {
	n := 0
	for _, item := range items {
		if strings.Contains(item, needle) {
			n++
		}
	}
	return n
}

func observedDeployment(name string, specReplicas int32, status appsv1.DeploymentStatus) *appsv1.Deployment {
	dep := newTestDeployment(name, "default", map[string]string{"app": name})
	dep.Generation = 1
	dep.Spec.Replicas = int32Ptr(specReplicas)
	status.ObservedGeneration = 1
	dep.Status = status
	return dep
}

func mainContainer(t *testing.T) corev1.Container {
	t.Helper()
	cpuReq, err := resource.ParseQuantity("100m")
	require.NoError(t, err)
	cpuLim, err := resource.ParseQuantity("500m")
	require.NoError(t, err)
	memReq, err := resource.ParseQuantity("128Mi")
	require.NoError(t, err)
	memLim, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	return corev1.Container{
		Name:  "main",
		Image: "nginx",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    cpuReq,
				corev1.ResourceMemory: memReq,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpuLim,
				corev1.ResourceMemory: memLim,
			},
		},
	}
}

func TestProcessWorkloads_Recommend_MidReplacementStillRecommends(t *testing.T) {
	now := time.Now()
	dep := observedDeployment("api", 3, appsv1.DeploymentStatus{
		Replicas:          3,
		UpdatedReplicas:   1,
		AvailableReplicas: 3,
	})
	policy := newTestPolicy("test-policy", "default")
	r, rec := newRolloutReconciler([]client.Object{dep}, nil)
	result := runRolloutProcess(r, policy, dep, now)
	requireNonStaleRec(t, result)
	require.Equal(t, 0, countSubstr(recordedEvents(rec), "RolloutInProgress"))
}

func TestProcessWorkloads_Auto_MidReplacementRecommendsAndSkipsResize(t *testing.T) {
	now := time.Now()
	dep := observedDeployment("api", 3, appsv1.DeploymentStatus{
		Replicas:          3,
		UpdatedReplicas:   1,
		AvailableReplicas: 3,
	})
	pod := burstableResizePod("api-abc-1", "api")
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	r, rec := newRolloutReconciler([]client.Object{dep}, []*corev1.Pod{pod})
	result := runRolloutProcess(r, policy, dep, now)
	requireNonStaleRec(t, result)
	require.True(t, r.isRollingOut(dep))

	count, _ := r.executeResizes(context.Background(), policy, []client.Object{dep},
		[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("api")},
		podMap("api", pod), nil, nil)
	require.Equal(t, 0, count)
	require.Empty(t, resizedPodNames(r.Clientset.(*kubefake.Clientset)))
	evs := recordedEvents(rec)
	require.Equal(t, 1, countSubstr(evs, "RolloutInProgress"))
	require.Equal(t, 1, countSubstr(evs, "Resize deferred for workload api: rollout in progress"))
}

func TestProcessWorkloads_Auto_ScaleOutStillResizesReadyPods(t *testing.T) {
	now := time.Now()
	dep := observedDeployment("api", 5, appsv1.DeploymentStatus{
		Replicas:          3,
		UpdatedReplicas:   3,
		AvailableReplicas: 3,
	})
	ready := burstableResizePod("api-ready", "api")
	pending := burstableResizePod("api-pending", "api")
	pending.Status.Phase = corev1.PodPending
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
	r, rec := newRolloutReconciler([]client.Object{dep}, []*corev1.Pod{ready, pending})
	result := runRolloutProcess(r, policy, dep, now)
	requireNonStaleRec(t, result)
	require.False(t, r.isRollingOut(dep))
	require.False(t, r.podSkippedForRollout(dep, pending, ""))
	require.False(t, r.podSkippedForRollout(dep, ready, ""))

	count, _ := r.executeResizes(context.Background(), policy, []client.Object{dep},
		[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("api")},
		podMap("api", ready, pending), nil, nil)
	require.Equal(t, 1, count)
	names := resizedPodNames(r.Clientset.(*kubefake.Clientset))
	require.Contains(t, names, ready.Name)
	require.NotContains(t, names, pending.Name)
	require.Equal(t, 0, countSubstr(recordedEvents(rec), "RolloutInProgress"))
}

func TestWorkload_PodSkippedForRollout(t *testing.T) {
	t.Run("StatefulSet_stale_generation_skips_all_pods", func(t *testing.T) {
		now := time.Now()
		replicas := int32(5)
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 4},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{mainContainer(t)}},
				},
			},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 3,
				Replicas:           5,
				UpdatedReplicas:    2,
				CurrentRevision:    "rev-old",
				UpdateRevision:     "rev-new",
			},
		}
		first := burstableResizePod("db-0", "db")
		first.Labels[appsv1.ControllerRevisionHashLabelKey] = "rev-new"
		second := burstableResizePod("db-1", "db")
		second.Labels[appsv1.ControllerRevisionHashLabelKey] = "rev-new"
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, _ := newRolloutReconciler([]client.Object{sts}, []*corev1.Pod{first, second})
		requireNonStaleRec(t, runRolloutProcess(r, policy, sts, now))
		require.False(t, r.isRollingOut(sts))
		require.True(t, r.podSkippedForRollout(sts, first, ""))
		require.True(t, r.podSkippedForRollout(sts, second, ""))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{sts},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("db")},
			podMap("db", first, second), nil, nil)
		require.Equal(t, 0, count)
		require.Empty(t, resizedPodNames(r.Clientset.(*kubefake.Clientset)))
	})

	t.Run("StatefulSet_partition_holds_old_pods", func(t *testing.T) {
		now := time.Now()
		replicas := int32(5)
		partition := int32(2)
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 1},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
				UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
					Type:          appsv1.RollingUpdateStatefulSetStrategyType,
					RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{mainContainer(t)}},
				},
			},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 1,
				Replicas:           5,
				UpdatedReplicas:    3,
				CurrentRevision:    "rev-old",
				UpdateRevision:     "rev-new",
			},
		}
		fresh := burstableResizePod("db-fresh", "db")
		fresh.Labels[appsv1.ControllerRevisionHashLabelKey] = "rev-new"
		old := burstableResizePod("db-old", "db")
		old.Labels[appsv1.ControllerRevisionHashLabelKey] = "rev-old"
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{sts}, []*corev1.Pod{fresh, old})
		requireNonStaleRec(t, runRolloutProcess(r, policy, sts, now))
		require.False(t, r.isRollingOut(sts))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{sts},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("db")},
			podMap("db", fresh, old), nil, nil)
		require.Equal(t, 1, count)
		names := resizedPodNames(r.Clientset.(*kubefake.Clientset))
		require.Contains(t, names, fresh.Name)
		require.NotContains(t, names, old.Name)
		require.Equal(t, 1, countSubstr(recordedEvents(rec), "RolloutInProgress"))
	})

	t.Run("DaemonSet_RollingUpdate_old_pod_skipped", func(t *testing.T) {
		now := time.Now()
		ds := testDaemonSet(t, 3, 5)
		currentRev := controllerRev("ds-current", 3, ds.UID)
		oldRev := controllerRev("ds-old", 1, ds.UID)
		current := burstableResizePod("ds-current-pod", "ds")
		current.Labels[appsv1.ControllerRevisionHashLabelKey] = currentRev.Name
		old := burstableResizePod("ds-old-pod", "ds")
		old.Labels[appsv1.ControllerRevisionHashLabelKey] = oldRev.Name
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{ds, currentRev, oldRev}, []*corev1.Pod{current, old})
		result := runRolloutProcess(r, policy, ds, now)
		requireNonStaleRec(t, result)
		require.False(t, r.isRollingOut(ds))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{ds},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("ds")},
			podMap("ds", current, old), nil, nil)
		require.Equal(t, 1, count)
		names := resizedPodNames(r.Clientset.(*kubefake.Clientset))
		require.Contains(t, names, current.Name)
		require.NotContains(t, names, old.Name)
		require.Equal(t, 1, countSubstr(recordedEvents(rec), "RolloutInProgress"))
	})

	t.Run("DaemonSet_new_node_current_pods_still_eligible", func(t *testing.T) {
		now := time.Now()
		ds := testDaemonSet(t, 4, 5)
		currentRev := controllerRev("ds-current", 2, ds.UID)
		first := burstableResizePod("ds-a", "ds")
		first.Labels[appsv1.ControllerRevisionHashLabelKey] = currentRev.Name
		second := burstableResizePod("ds-b", "ds")
		second.Labels[appsv1.ControllerRevisionHashLabelKey] = currentRev.Name
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{ds, currentRev}, []*corev1.Pod{first, second})
		requireNonStaleRec(t, runRolloutProcess(r, policy, ds, now))
		require.False(t, r.isRollingOut(ds))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{ds},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("ds")},
			podMap("ds", first, second), nil, nil)
		require.Equal(t, 1, count)
		names := resizedPodNames(r.Clientset.(*kubefake.Clientset))
		require.ElementsMatch(t, []string{first.Name, second.Name}, names)
		require.Equal(t, 0, countSubstr(recordedEvents(rec), "RolloutInProgress"))
	})

	t.Run("DaemonSet_legacy_annotation_and_OnDelete", func(t *testing.T) {
		r := NewAttunePolicyReconciler()
		ds := &appsv1.DaemonSet{}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			podTemplateGenerationAnnotation: "rev-a",
		}}}
		require.False(t, r.podSkippedForRollout(ds, pod, "rev-a"))
		require.True(t, r.podSkippedForRollout(ds, pod, "rev-b"))
		require.False(t, r.podSkippedForRollout(ds, pod, ""))
		ds.Spec.UpdateStrategy.Type = appsv1.OnDeleteDaemonSetStrategyType
		require.False(t, r.podSkippedForRollout(ds, pod, "rev-b"))
	})

	t.Run("Deployment_paused_skips_old_pod_template_hash", func(t *testing.T) {
		r := NewAttunePolicyReconciler()
		dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Paused: true, Replicas: int32Ptr(3)}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			appsv1.DefaultDeploymentUniqueLabelKey: "hash-old",
		}}}
		require.True(t, r.podSkippedForRollout(dep, pod, "hash-new"))
		require.False(t, r.podSkippedForRollout(dep, pod, "hash-old"))
		require.False(t, r.podSkippedForRollout(dep, pod, ""))
		dep.Spec.Paused = false
		require.False(t, r.podSkippedForRollout(dep, pod, "hash-new"))
	})

	t.Run("DaemonSet_controllerrevision_list_error_skips_every_pod", func(t *testing.T) {
		ds := testDaemonSet(t, 3, 5)
		current := burstableResizePod("ds-current-pod", "ds")
		current.Labels[appsv1.ControllerRevisionHashLabelKey] = "ds-current"
		old := burstableResizePod("ds-old-pod", "ds")
		old.Labels[appsv1.ControllerRevisionHashLabelKey] = "ds-old"
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{ds}, []*corev1.Pod{current, old})
		r.Client = daemonSetRevisionForbiddenClient(t, ds, current, old)
		require.False(t, r.isRollingOut(ds))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{ds},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("ds")},
			podMap("ds", current, old), nil, nil)
		require.Equal(t, 0, count)
		require.Empty(t, resizedPodNames(r.Clientset.(*kubefake.Clientset)))
		evs := recordedEvents(rec)
		require.Equal(t, 1, countSubstr(evs, "DaemonSetRevisionUnavailable"))
		require.Equal(t, 0, countSubstr(evs, "RolloutInProgress"))
	})

	t.Run("DaemonSet_OnDelete_ignores_controllerrevision_list_error", func(t *testing.T) {
		ds := testDaemonSet(t, 3, 5)
		ds.Spec.UpdateStrategy.Type = appsv1.OnDeleteDaemonSetStrategyType
		pod := burstableResizePod("ds-pod", "ds")
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{ds}, []*corev1.Pod{pod})
		r.Client = daemonSetRevisionForbiddenClient(t, ds, pod)

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{ds},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("ds")},
			podMap("ds", pod), nil, nil)
		require.Equal(t, 1, count)
		require.Equal(t, 0, countSubstr(recordedEvents(rec), "DaemonSetRevisionUnavailable"))
	})

	t.Run("Deployment_paused_resizes_newest_replicaset_hash", func(t *testing.T) {
		now := time.Now()
		dep := observedDeployment("api", 3, appsv1.DeploymentStatus{
			Replicas:          4,
			UpdatedReplicas:   2,
			AvailableReplicas: 3,
		})
		dep.UID = "dep-uid"
		dep.Spec.Paused = true
		currentRS := pausedReplicaSet("api-new", "3", "hash-new", dep.UID)
		oldRS := pausedReplicaSet("api-old", "2", "hash-old", dep.UID)
		current := burstableResizePod("api-current", "api")
		current.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-new"
		old := burstableResizePod("api-old-pod", "api")
		old.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-old"
		policy := newTestPolicy("test-policy", "default")
		policy.Spec.UpdateStrategy.Type = attunev1alpha1.UpdateTypeAuto
		r, rec := newRolloutReconciler([]client.Object{dep, currentRS, oldRS}, []*corev1.Pod{current, old})
		requireNonStaleRec(t, runRolloutProcess(r, policy, dep, now))
		require.False(t, r.isRollingOut(dep))

		count, _ := r.executeResizes(context.Background(), policy, []client.Object{dep},
			[]attunev1alpha1.WorkloadRecommendation{risingCPURecommendation("api")},
			podMap("api", current, old), nil, nil)
		require.Equal(t, 1, count)
		names := resizedPodNames(r.Clientset.(*kubefake.Clientset))
		require.Contains(t, names, current.Name)
		require.NotContains(t, names, old.Name)
		require.Equal(t, 1, countSubstr(recordedEvents(rec), "RolloutInProgress"))
	})
}

func daemonSetRevisionForbiddenClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := testScheme()
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*appsv1.ControllerRevisionList); ok {
					return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, "", fmt.Errorf("injected"))
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
}

func pausedReplicaSet(name, revision, hash string, owner types.UID) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				appsv1.DefaultDeploymentUniqueLabelKey: hash,
			},
			Annotations: map[string]string{
				deploymentRevisionAnnotation: revision,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "api",
				UID:        owner,
			}},
		},
	}
}

func testDaemonSet(t *testing.T, updated, desired int32) *appsv1.DaemonSet {
	t.Helper()
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ds",
			Namespace:  "default",
			UID:        types.UID("ds-uid"),
			Generation: 1,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ds"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{mainContainer(t)}},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1,
			DesiredNumberScheduled: desired,
			UpdatedNumberScheduled: updated,
		},
	}
}

func controllerRev(name string, rev int64, owner types.UID) *appsv1.ControllerRevision {
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       "ds",
				UID:        owner,
			}},
		},
		Revision: rev,
	}
}
