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

package manifests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	require.NoError(t, err)
	return string(body)
}

func TestQueryServiceAccountManifests(t *testing.T) {
	sa := readRepo(t, "config/rbac/prometheus_query_service_account.yaml")
	require.Contains(t, sa, "kind: ServiceAccount")
	require.Contains(t, sa, "name: prometheus-query")

	role := readRepo(t, "config/rbac/prometheus_query_token_role.yaml")
	require.Contains(t, role, "serviceaccounts/token")
	require.Contains(t, role, "attune-prometheus-query")
	require.Contains(t, role, "create")

	binding := readRepo(t, "config/rbac/prometheus_query_token_role_binding.yaml")
	require.Contains(t, binding, "name: prometheus-query")
	require.Contains(t, binding, "kind: ServiceAccount")
	require.Contains(t, binding, "name: controller-manager")

	kust := readRepo(t, "config/rbac/kustomization.yaml")
	require.Contains(t, kust, "prometheus_query_service_account.yaml")
	require.Contains(t, kust, "prometheus_query_token_role.yaml")
	require.Contains(t, kust, "prometheus_query_token_role_binding.yaml")

	def := readRepo(t, "config/default/kustomization.yaml")
	require.Contains(t, def, "namePrefix: attune-")

	manager := readRepo(t, "config/manager/manager.yaml")
	require.Contains(t, manager, "--prometheus-use-service-account-token")
	require.Contains(t, manager, "--prometheus-query-service-account=attune-prometheus-query")

	csv := readRepo(t, "config/olm/template/manifests/attune.clusterserviceversion.yaml")
	require.Contains(t, csv, "serviceAccountName: attune-prometheus-query")
	require.Contains(t, csv, "serviceaccounts/token")
	require.Contains(t, csv, "attune-prometheus-query")
	require.Contains(t, csv, "--prometheus-use-service-account-token")
	require.Contains(t, csv, "--prometheus-query-service-account=attune-prometheus-query")

	rendered := readRepo(t, "dist/install.yaml")
	require.Contains(t, rendered, "name: attune-prometheus-query")
	require.Contains(t, rendered, "serviceaccounts/token")
	require.Contains(t, rendered, "--prometheus-query-service-account=attune-prometheus-query")
}
