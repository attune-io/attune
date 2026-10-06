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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestOLMTemplateClusterRulesMatchRole(t *testing.T) {
	root := repoRoot(t)
	roleYAML, err := os.ReadFile(filepath.Join(root, "config/rbac/role.yaml"))
	require.NoError(t, err)
	csvYAML, err := os.ReadFile(filepath.Join(root, "config/olm/template/manifests/attune.clusterserviceversion.yaml"))
	require.NoError(t, err)

	roleRules, err := parseClusterRoleRules(roleYAML)
	require.NoError(t, err)
	csvRules, nPerms, err := parseCSVClusterRules(csvYAML)
	require.NoError(t, err)
	require.Equal(t, 1, nPerms)
	assert.True(t, grantsEqual(roleRules, csvRules), "CSV cluster rules differ from config/rbac/role.yaml; run make sync-olm-rbac")

	synced, err := SyncClusterPermissions(csvYAML, roleYAML)
	require.NoError(t, err)
	assert.Equal(t, string(csvYAML), string(synced), "template is not the generated clusterPermissions; run make sync-olm-rbac")

	syncedRules, _, err := parseCSVClusterRules(synced)
	require.NoError(t, err)
	require.NotEmpty(t, syncedRules)
	assert.True(t, isLeaseRule(syncedRules[len(syncedRules)-1]))
}

func TestFallenBehindCSVGainsRoleRules(t *testing.T) {
	roleYAML, err := os.ReadFile(filepath.Join(repoRoot(t), "config/rbac/role.yaml"))
	require.NoError(t, err)
	roleRules, err := parseClusterRoleRules(roleYAML)
	require.NoError(t, err)

	stale := []byte(staleLeaseOnlyCSV)
	staleRules, _, err := parseCSVClusterRules(stale)
	require.NoError(t, err)
	assert.False(t, grantsEqual(roleRules, staleRules), "a leases-only CSV must not match role.yaml")

	synced, err := SyncClusterPermissions(stale, roleYAML)
	require.NoError(t, err)
	syncedRules, _, err := parseCSVClusterRules(synced)
	require.NoError(t, err)
	assert.True(t, grantsEqual(roleRules, syncedRules))
	require.True(t, isLeaseRule(syncedRules[len(syncedRules)-1]))
	assert.Equal(t, []string{"create", "delete", "get", "list", "update", "watch"}, syncedRules[len(syncedRules)-1].Verbs)

	again, err := SyncClusterPermissions(synced, roleYAML)
	require.NoError(t, err)
	assert.Equal(t, string(synced), string(again))

	require.Contains(t, string(synced), "serviceaccounts/token")
	require.Contains(t, string(synced), "attune-prometheus-query")
	var doc struct {
		Spec struct {
			Install struct {
				Spec struct {
					Permissions []struct {
						ServiceAccountName string              `json:"serviceAccountName"`
						Rules              []rbacv1.PolicyRule `json:"rules"`
					} `json:"permissions"`
				} `json:"spec"`
			} `json:"install"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(synced, &doc))
	require.Len(t, doc.Spec.Install.Spec.Permissions, 2)
	assert.Equal(t, "attune-controller-manager", doc.Spec.Install.Spec.Permissions[0].ServiceAccountName)
	require.Len(t, doc.Spec.Install.Spec.Permissions[0].Rules, 1)
	assert.Equal(t, []string{"serviceaccounts/token"}, doc.Spec.Install.Spec.Permissions[0].Rules[0].Resources)
	assert.Empty(t, doc.Spec.Install.Spec.Permissions[1].Rules)
}

func TestSyncKeepsNamespacedPermissionsWhenLeaseIsMissing(t *testing.T) {
	roleYAML := []byte(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch"]
`)
	csv := []byte(strings.Replace(staleLeaseOnlyCSV, leaseRuleYAML, "            - apiGroups: [\"\"]\n              resources: [\"pods\"]\n              verbs: [\"get\"]\n", 1))
	synced, err := SyncClusterPermissions(csv, roleYAML)
	require.NoError(t, err)
	rules, _, err := parseCSVClusterRules(synced)
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, []string{"namespaces"}, rules[0].Resources)
	assert.True(t, isLeaseRule(rules[1]))
	assert.Contains(t, string(synced), "serviceaccounts/token")
}

func TestSyncClusterPermissionsRejectsBadInput(t *testing.T) {
	_, err := SyncClusterPermissions([]byte("kind: ClusterServiceVersion\n"), []byte("kind: ClusterRole\nrules: []\n"))
	require.Error(t, err)

	_, err = SyncClusterPermissions([]byte("kind: ClusterServiceVersion\n"), []byte("kind: ClusterRole\nmetadata:\n  name: manager-role\nrules:\n- apiGroups: [\"\"]\n  resources: [\"pods\"]\n  verbs: [\"get\"]\n"))
	require.Error(t, err)
}

const leaseRuleYAML = `            - apiGroups:
                - coordination.k8s.io
              resources:
                - leases
              verbs:
                - create
                - delete
                - get
                - list
                - update
                - watch
`

const staleLeaseOnlyCSV = `apiVersion: operators.coreos.com/v1alpha1
kind: ClusterServiceVersion
metadata:
  name: attune.v__VERSION__
spec:
  install:
    strategy: deployment
    spec:
      permissions:
        - serviceAccountName: attune-controller-manager
          rules:
            - apiGroups:
                - ""
              resources:
                - serviceaccounts/token
              resourceNames:
                - attune-prometheus-query
              verbs:
                - create
        - serviceAccountName: attune-prometheus-query
          rules: []
      clusterPermissions:
        - serviceAccountName: attune-controller-manager
          rules:
` + leaseRuleYAML + `      deployments: []
`
