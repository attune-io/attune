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

package safety

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

func TestCheckPod_EnforceNamespaceRewritesAndHidesValue(t *testing.T) {
	t.Parallel()
	pod := readyPod("web-0", "team-a")
	monitor := NewMonitor(fake.NewSimpleClientset(pod), logr.Discard())
	querier := &mockSLOQuerier{value: 1234.5}
	monitor.WithSLOChecker(querier, []attunev1alpha1.SLOGuardrail{{
		Name:       "x",
		Query:      "sum(foo)",
		Threshold:  "1",
		Comparison: "above",
	}})
	monitor.SetSLOAuth(SLOAuthEnforceNamespace, "team-a")

	verdict, err := monitor.CheckPod(context.Background(), elapsedRecord("team-a"), time.Now())
	require.NoError(t, err)
	assert.False(t, verdict.Safe)
	assert.Contains(t, querier.gotQuery, `namespace="team-a"`)
	assert.NotContains(t, verdict.Message, "1234")
	assert.Contains(t, verdict.Message, "above threshold")
}

func TestCheckPod_ConflictingNamespaceIsNotQueried(t *testing.T) {
	t.Parallel()
	pod := readyPod("web-0", "team-a")
	monitor := NewMonitor(fake.NewSimpleClientset(pod), logr.Discard())
	querier := &mockSLOQuerier{value: 99}
	monitor.WithSLOChecker(querier, []attunev1alpha1.SLOGuardrail{{
		Name:             "x",
		Query:            `foo{namespace="team-b"}`,
		Threshold:        "1",
		Comparison:       "above",
		EvaluationWindow: &metav1.Duration{Duration: time.Minute},
	}})
	monitor.SetSLOAuth(SLOAuthEnforceNamespace, "team-a")

	record := elapsedRecord("team-a")
	record.ResizedAt = time.Now().Add(-30 * time.Second)
	verdict, err := monitor.CheckPod(context.Background(), record, time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe)
	assert.False(t, verdict.SLODeferred)
	assert.Empty(t, querier.gotQuery)
	assert.Equal(t, []string{"x"}, monitor.SLOSkipNames())
}

func TestCheckPod_ScopedEmptyQueryIsReported(t *testing.T) {
	t.Parallel()
	pod := readyPod("web-0", "team-a")
	monitor := NewMonitor(fake.NewSimpleClientset(pod), logr.Discard())
	querier := &mockSLOQuerier{err: rsmetrics.ErrEmptyInstantQuery}
	monitor.WithSLOChecker(querier, []attunev1alpha1.SLOGuardrail{{
		Name:       "ingress",
		Query:      "sum(nginx_ingress_controller_requests)",
		Threshold:  "1",
		Comparison: "above",
	}})
	monitor.SetSLOAuth(SLOAuthEnforceNamespace, "team-a")

	verdict, err := monitor.CheckPod(context.Background(), elapsedRecord("team-a"), time.Now())
	require.NoError(t, err)
	assert.True(t, verdict.Safe, "an empty scoped query does not revert")
	assert.Contains(t, querier.gotQuery, `namespace="team-a"`)
	assert.Equal(t, []string{"ingress"}, monitor.SLOEmptyNames())
	assert.Empty(t, monitor.SLOSkipNames())
}

func readyPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app"},
			},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func elapsedRecord(namespace string) ResizeRecord {
	return ResizeRecord{
		PodName:      "web-0",
		Namespace:    namespace,
		Container:    "app",
		ResizedAt:    time.Now().Add(-6 * time.Minute),
		WorkloadName: "my-app",
	}
}
