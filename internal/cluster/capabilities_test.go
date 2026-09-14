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

package cluster

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAllowsInPlaceMemoryLimitDecrease(t *testing.T) {
	t.Parallel()
	assert.False(t, AllowsInPlaceMemoryLimitDecrease("v1.33.0"))
	assert.False(t, AllowsInPlaceMemoryLimitDecrease("v1.34.7"))
	assert.True(t, AllowsInPlaceMemoryLimitDecrease("v1.35.0"))
	assert.True(t, AllowsInPlaceMemoryLimitDecrease("v1.35.4-k3s1"))
	assert.True(t, AllowsInPlaceMemoryLimitDecrease("v1.36.0+abc"))
	assert.False(t, AllowsInPlaceMemoryLimitDecrease(""))
	assert.False(t, AllowsInPlaceMemoryLimitDecrease("bogus"))
}

func TestParseGitVersion(t *testing.T) {
	t.Parallel()
	major, minor, ok := ParseGitVersion("v1.36.4-k3s1")
	require.True(t, ok)
	assert.Equal(t, uint(1), major)
	assert.Equal(t, uint(36), minor)

	_, _, ok = ParseGitVersion("")
	assert.False(t, ok)
	_, _, ok = ParseGitVersion("v1")
	assert.False(t, ok)
}

func TestInPlaceFromDeclaredFeatures(t *testing.T) {
	t.Parallel()
	ready := func(name string, feats []string) corev1.Node {
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
				DeclaredFeatures: feats,
			},
		}
	}
	notReady := ready("nr", []string{FeatureInPlacePodLevelResources})
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	tests := []struct {
		name    string
		nodes   []corev1.Node
		major   uint
		minor   uint
		want    bool
		nilList bool
	}{
		{
			name:  "name present",
			nodes: []corev1.Node{ready("n1", []string{FeatureInPlacePodLevelResources})},
			major: 1, minor: 35,
			want: true,
		},
		{
			name:  "non-empty omit is off",
			nodes: []corev1.Node{ready("n1", []string{"SomeOtherFeature"})},
			major: 1, minor: 36,
			want: false,
		},
		{
			name:  "empty list plus 1.36 is on",
			nodes: []corev1.Node{ready("n1", nil)},
			major: 1, minor: 36,
			want: true,
		},
		{
			name:  "empty list plus 1.35 is off",
			nodes: []corev1.Node{ready("n1", nil)},
			major: 1, minor: 35,
			want: false,
		},
		{
			name:  "name wins over sibling omit",
			nodes: []corev1.Node{ready("a", []string{"Other"}), ready("b", []string{FeatureInPlacePodLevelResources})},
			major: 1, minor: 35,
			want: true,
		},
		{
			name:  "not-ready name is ignored",
			nodes: []corev1.Node{notReady, ready("n1", nil)},
			major: 1, minor: 35,
			want: false,
		},
		{
			name:    "nil list uses GitVersion",
			nilList: true,
			major:   1, minor: 36,
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var list *corev1.NodeList
			if !tt.nilList {
				list = &corev1.NodeList{Items: tt.nodes}
			}
			assert.Equal(t, tt.want, inPlaceFromDeclaredFeatures(list, tt.major, tt.minor))
		})
	}
}

func TestHasPodsResize_CoreV1Only(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	fd.FakedServerVersion = &k8sversion.Info{GitVersion: "v1.35.0"}
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "apps/v1",
		APIResources: []metav1.APIResource{{Name: "pods/resize"}},
	}}
	assert.False(t, hasPodsResize(fd, logr.Discard()), "apps/v1 pods/resize is not core")

	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "pods/resize", Namespaced: true}},
	}}
	assert.True(t, hasPodsResize(fd, logr.Discard()))
}

func TestDiscover_OpenAPIErrorKeepsMemoryDecrease(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	fd.FakedServerVersion = &k8sversion.Info{Major: "1", Minor: "35", GitVersion: "v1.35.8"}
	// Fake OpenAPISchema errors; Discover must not return that as err
	// and must keep 1.35 memory decrease on.
	caps, err := Discover(context.Background(), fd, cs.CoreV1().Nodes())
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.True(t, caps.AllowInPlaceMemoryLimitDecrease, "OpenAPI error must not re-clamp 1.35+")
	assert.Equal(t, "v1.35.8", caps.GitVersion)
	assert.Equal(t, uint(1), caps.Major)
	assert.Equal(t, uint(35), caps.Minor)
	assert.False(t, caps.InPlacePodLevelResources, "empty nodes + 1.35")
	assert.False(t, caps.SchedulerResizePreemption)
	assert.False(t, caps.MemoryBackedVolumeResize)
	assert.False(t, caps.ExclusiveCPUInPlace)
}

func TestDiscover_ServerVersionError(t *testing.T) {
	t.Parallel()
	caps, err := Discover(context.Background(), nil, nil)
	require.Error(t, err)
	require.NotNil(t, caps)
	assert.False(t, caps.AllowInPlaceMemoryLimitDecrease)
}

func TestDiscover_NodeListErrorFallsBackToVersion(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	fd.FakedServerVersion = &k8sversion.Info{GitVersion: "v1.36.4-k3s1"}
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "pods/resize"}},
	}}

	caps, err := Discover(context.Background(), fd, failingNodeLister{})
	require.NoError(t, err)
	assert.True(t, caps.PodsResize)
	assert.True(t, caps.InPlacePodLevelResources, "list error + 1.36 => on")
	assert.True(t, caps.AllowInPlaceMemoryLimitDecrease)
}

type failingNodeLister struct{}

func (failingNodeLister) List(context.Context, metav1.ListOptions) (*corev1.NodeList, error) {
	return nil, fmt.Errorf("list denied")
}

func TestDiscover_DeclaredFeatureNameOn(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
			DeclaredFeatures: []string{FeatureInPlacePodLevelResources},
		},
	}
	cs := fake.NewSimpleClientset(node)
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	fd.FakedServerVersion = &k8sversion.Info{GitVersion: "v1.35.8"}
	caps, err := Discover(context.Background(), fd, cs.CoreV1().Nodes())
	require.NoError(t, err)
	assert.True(t, caps.InPlacePodLevelResources)
}
