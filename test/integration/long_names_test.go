//go:build integration

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

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/controller"
)

// TestMarkResizeTime_LongWorkloadNamePersists stamps the cooldown for a
// workload whose name is too long for a raw attune.io/last-resize-time.<name>
// key (#1135). The real apiserver validates annotation keys, so before the
// fix the whole merge patch was rejected and no workload got a stamp.
func TestMarkResizeTime_LongWorkloadNamePersists(t *testing.T) {
	long := "payments-reconciliation-ledger-worker-eu-west-primary-blue"
	require.Len(t, long, 58)

	policy := newTestPolicy("long-name-cooldown", "integration-test", long)
	require.NoError(t, k8sClient.Create(ctx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, policy) })

	r := controller.NewAttunePolicyReconciler()
	r.Client = k8sClient
	r.Scheme = k8sClient.Scheme()
	now := time.Now().UTC()
	r.SetNowFunc(func() time.Time { return now })

	var p attunev1alpha1.AttunePolicy
	key := types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}
	require.NoError(t, k8sClient.Get(ctx, key, &p))
	require.NoError(t, r.MarkResizeTimeForTest(ctx, &p, long, "short-app"),
		"apiserver must accept every per-workload cooldown key")

	var stored attunev1alpha1.AttunePolicy
	require.NoError(t, k8sClient.Get(ctx, key, &stored))
	ts := now.Format(time.RFC3339)
	assert.Equal(t, ts, stored.Annotations["attune.io/last-resize-time"])
	assert.Equal(t, ts, stored.Annotations["attune.io/last-resize-time.short-app"],
		"short names keep today's key")
	longKeys := 0
	for k, v := range stored.Annotations {
		name := strings.TrimPrefix(k, "attune.io/")
		if strings.HasPrefix(name, "last-resize-time.payments-") {
			longKeys++
			assert.LessOrEqual(t, len(name), 63, k)
			assert.Equal(t, ts, v)
		}
	}
	assert.Equal(t, 1, longKeys, "one bounded key for the long workload")
	assert.True(t, r.WorkloadCooldownActiveForTest(&stored, long),
		"cooldown is read back from the persisted stamp")
	assert.True(t, r.WorkloadCooldownActiveForTest(&stored, "short-app"))
}

// TestExportRecommendationConfigMaps_LongWorkloadName exports a
// recommendation for a workload whose name is longer than a label value
// may be (#1136). The real apiserver validates labels, so before the fix
// Create was rejected and no ConfigMap was written.
func TestExportRecommendationConfigMaps_LongWorkloadName(t *testing.T) {
	long := "workload-" + strings.Repeat("w", 64)
	require.Greater(t, len(long), 63)

	policy := newTestPolicy("long-name-export", "integration-test", long)
	require.NoError(t, k8sClient.Create(ctx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, policy) })

	r := controller.NewAttunePolicyReconciler()
	r.Client = k8sClient
	r.Scheme = k8sClient.Scheme()

	cpu, err := resource.ParseQuantity("200m")
	require.NoError(t, err)
	mem, err := resource.ParseQuantity("256Mi")
	require.NoError(t, err)
	recs := []attunev1alpha1.WorkloadRecommendation{{
		Workload: long,
		Kind:     "Deployment",
		Containers: []attunev1alpha1.ContainerRecommendation{{
			Name:        "main",
			Confidence:  0.9,
			Recommended: attunev1alpha1.ResourceValues{CPURequest: cpu, MemoryRequest: mem},
		}},
	}}

	// Twice: the second pass takes the Patch path and runs orphan cleanup
	// against the stored, shortened label.
	r.ExportRecommendationConfigMapsForTest(ctx, policy, recs)
	r.ExportRecommendationConfigMapsForTest(ctx, policy, recs)

	cmName := policy.Name + "-" + long + "-recommendations"
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: cmName}, &cm),
		"apiserver must accept the export ConfigMap labels")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &cm) })
	assert.Equal(t, long, cm.Data["workload"], "full name stays in Data")
	assert.Equal(t, policy.Name, cm.Labels["attune.io/policy"], "short policy name keeps its label")
	assert.LessOrEqual(t, len(cm.Labels["attune.io/workload"]), 63)
	assert.NotEqual(t, long, cm.Labels["attune.io/workload"])
}
