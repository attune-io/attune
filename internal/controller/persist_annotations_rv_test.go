package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// rvBumpPatchClient records merge-patch bodies. A resourceVersion bump after
// the re-fetch must not make the tracking write conflict.
type rvBumpPatchClient struct {
	client.Client
	bodies      [][]byte
	patchCalls  int
	updateCalls int
}

func (c *rvBumpPatchClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updateCalls++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *rvBumpPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	c.bodies = append(c.bodies, append([]byte(nil), data...))
	c.patchCalls++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestWriteResizeTracking_ResourceVersionBumpDoesNotConflict(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations["keep-me"] = "leave-this"
	pod.Labels["keep-label"] = "leave-this"
	scheme := testScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	wrapper := &rvBumpPatchClient{Client: base}
	cs := kubefake.NewSimpleClientset(pod.DeepCopy())
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		getAction := action.(clienttesting.GetAction)
		obj, err := cs.Tracker().Get(gvr, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		live := &corev1.Pod{}
		key := client.ObjectKey{Namespace: getAction.GetNamespace(), Name: getAction.GetName()}
		if err := base.Get(context.Background(), key, live); err != nil {
			return true, nil, err
		}
		preRV := live.ResourceVersion
		if err := base.Update(context.Background(), live); err != nil {
			return true, nil, err
		}
		stale := obj.(*corev1.Pod).DeepCopy()
		stale.SetResourceVersion(preRV)
		return true, stale, nil
	})

	r := NewAttunePolicyReconciler()
	r.Client = wrapper
	r.Scheme = scheme
	r.Clientset = cs

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(context.Background(), working, persistMainRec(t),
		"test-policy", "api-server", now, 3, "")
	require.NoError(t, err, "patchCalls=%d updateCalls=%d", wrapper.patchCalls, wrapper.updateCalls)
	require.Empty(t, reason)
	require.Equal(t, "true", working.Labels[labelTracked])
	require.Equal(t, now.UTC().Format(time.RFC3339), working.Annotations[annotationResizedAt])
	require.Equal(t, 1, wrapper.patchCalls)
	require.Zero(t, wrapper.updateCalls)
	require.Len(t, wrapper.bodies, 1)
	body := string(wrapper.bodies[0])
	require.NotContains(t, body, "resourceVersion")
	require.NotContains(t, body, "keep-me")
	require.NotContains(t, body, "keep-label")
	require.Contains(t, body, annotationResizedAt)
	require.Contains(t, body, labelTracked)
	require.Equal(t, "leave-this", working.Annotations["keep-me"])
	require.Equal(t, "leave-this", working.Labels["keep-label"])
	require.Equal(t, "api-server", working.Labels["app"])
}

type twoConflictPatchClient struct {
	client.Client
	conflictsLeft int
}

func (c *twoConflictPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.conflictsLeft > 0 {
		c.conflictsLeft--
		return apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "pods"}, obj.GetName(), errors.New("conflict"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestPersistResizeAnnotations_TwoConflictsThenSuccess(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	wrapper := &twoConflictPatchClient{Client: base, conflictsLeft: 2}
	cs := kubefake.NewSimpleClientset(pod.DeepCopy())

	r := NewAttunePolicyReconciler()
	r.Client = wrapper
	r.Scheme = scheme
	r.Clientset = cs

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	working := pod.DeepCopy()
	reason, err := r.persistResizeAnnotations(context.Background(), working, persistMainRec(t),
		"test-policy", "api-server", now, 3, "")
	require.NoError(t, err)
	require.Empty(t, reason)
	require.Equal(t, "true", working.Labels[labelTracked])
	require.Equal(t, now.UTC().Format(time.RFC3339), working.Annotations[annotationResizedAt])
	require.Zero(t, wrapper.conflictsLeft)
}

func TestTrackAfterFailedRevert_ListsTrackedPod(t *testing.T) {
	pod := newResizePodWithStatus("api-server", "500m", "512Mi", "1000m", "1Gi", 3)
	scheme := testScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	cs := kubefake.NewSimpleClientset(pod.DeepCopy())

	r := NewAttunePolicyReconciler()
	r.Client = base
	r.Scheme = scheme
	r.Clientset = cs

	now := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	got, err := r.writeResizeTracking(context.Background(), pod, persistMainRec(t),
		"test-policy", "api-server", now, 3, "")
	require.NoError(t, err)
	require.NotNil(t, got)

	var listed corev1.PodList
	require.NoError(t, r.List(context.Background(), &listed,
		client.InNamespace(pod.Namespace),
		client.MatchingLabels{labelTracked: "true"}))
	require.Len(t, listed.Items, 1)
	require.Equal(t, pod.Name, listed.Items[0].Name)
}
