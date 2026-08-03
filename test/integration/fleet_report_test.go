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
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/attune-io/attune/internal/fleetreport"
)

func TestFleetReportExport_WritesConfigMap(t *testing.T) {
	ctx := context.Background()
	const ns = "integration-test"

	deploy := newTestDeployment("fleet-demo", ns)
	require.NoError(t, k8sClient.Create(ctx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), deploy) })

	polName := "fleet-pol-" + time.Now().Format("150405")
	policy := newTestPolicy(polName, ns, deploy.Name)
	require.NoError(t, k8sClient.Create(ctx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), policy) })

	cmName := "attune-fleet-report-" + polName
	exp := &fleetreport.Exporter{
		Client:    k8sClient,
		Log:       logr.Discard(),
		Namespace: ns,
		Name:      cmName,
		ClusterID: "integration-test",
		Interval:  time.Hour,
		Now:       func() time.Time { return time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC) },
	}
	exp.ExportOnce(ctx)

	var got corev1.ConfigMap
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &got))
	assert.Equal(t, "true", got.Labels[fleetreport.ConfigMapLabel])
	assert.Equal(t, fleetreport.SchemaVersion, got.Data["schema-version"])
	var decoded fleetreport.Report
	require.NoError(t, json.Unmarshal([]byte(got.Data["report.json"]), &decoded))
	assert.GreaterOrEqual(t, decoded.PolicyCount, 1)
	assert.Equal(t, "integration-test", decoded.ClusterID)
	assert.Equal(t, fleetreport.SchemaVersion, decoded.SchemaVersion)

	exp.ExportOnce(ctx)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &got))
	assert.NotEmpty(t, got.Data["report.json"])
}
