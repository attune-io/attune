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
	"k8s.io/apimachinery/pkg/types"

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
