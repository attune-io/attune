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
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
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

func yamlDocs(t *testing.T, rel string) [][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	require.NoError(t, err)
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(body)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs
		}
		require.NoError(t, err)
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
}

func decodeNamed[T any](t *testing.T, rel, kind, name string) T {
	t.Helper()
	var zero T
	for _, doc := range yamlDocs(t, rel) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		require.NoError(t, yaml.Unmarshal(doc, &meta))
		if meta.Kind != kind || meta.Metadata.Name != name {
			continue
		}
		var out T
		require.NoError(t, yaml.Unmarshal(doc, &out))
		return out
	}
	t.Fatalf("no %s %q in %s", kind, name, rel)
	return zero
}

func assertQueryTokenRole(t *testing.T, role rbacv1.Role, saName string) {
	t.Helper()
	require.Len(t, role.Rules, 1)
	rule := role.Rules[0]
	assert.Equal(t, []string{""}, rule.APIGroups)
	assert.Equal(t, []string{"serviceaccounts/token"}, rule.Resources)
	assert.Equal(t, []string{saName}, rule.ResourceNames)
	assert.Equal(t, []string{"create"}, rule.Verbs)
}

func assertQuerySABinding(t *testing.T, binding rbacv1.RoleBinding, roleName, subjectName string) {
	t.Helper()
	assert.Equal(t, "Role", binding.RoleRef.Kind)
	assert.Equal(t, roleName, binding.RoleRef.Name)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "ServiceAccount", binding.Subjects[0].Kind)
	assert.Equal(t, subjectName, binding.Subjects[0].Name)
}

func assertManagerQueryArgs(t *testing.T, dep appsv1.Deployment) {
	t.Helper()
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)
	container := dep.Spec.Template.Spec.Containers[0]
	assert.Contains(t, container.Args, "--prometheus-use-service-account-token")
	assert.Contains(t, container.Args, "--prometheus-query-service-account=attune-prometheus-query")
	var fieldPath string
	for _, env := range container.Env {
		if env.Name == "POD_NAMESPACE" && env.ValueFrom != nil && env.ValueFrom.FieldRef != nil {
			fieldPath = env.ValueFrom.FieldRef.FieldPath
		}
	}
	assert.Equal(t, "metadata.namespace", fieldPath)
}

func TestQueryServiceAccountManifests(t *testing.T) {
	sa := decodeNamed[corev1.ServiceAccount](t, "config/rbac/prometheus_query_service_account.yaml", "ServiceAccount", "prometheus-query")
	assert.Equal(t, "prometheus-query", sa.Name)

	role := decodeNamed[rbacv1.Role](t, "config/rbac/prometheus_query_token_role.yaml", "Role", "prometheus-query-token")
	assertQueryTokenRole(t, role, "attune-prometheus-query")

	binding := decodeNamed[rbacv1.RoleBinding](t, "config/rbac/prometheus_query_token_role_binding.yaml", "RoleBinding", "prometheus-query-token")
	assertQuerySABinding(t, binding, "prometheus-query-token", "controller-manager")

	kust := readRepo(t, "config/rbac/kustomization.yaml")
	require.Contains(t, kust, "prometheus_query_service_account.yaml")
	require.Contains(t, kust, "prometheus_query_token_role.yaml")
	require.Contains(t, kust, "prometheus_query_token_role_binding.yaml")

	def := readRepo(t, "config/default/kustomization.yaml")
	require.Contains(t, def, "namePrefix: attune-")

	manager := decodeNamed[appsv1.Deployment](t, "config/manager/manager.yaml", "Deployment", "controller-manager")
	assertManagerQueryArgs(t, manager)

	csvBody, err := os.ReadFile(filepath.Join(repoRoot(t), "config/olm/template/manifests/attune.clusterserviceversion.yaml"))
	require.NoError(t, err)
	var csv struct {
		Spec struct {
			Install struct {
				Spec struct {
					Permissions []struct {
						ServiceAccountName string              `json:"serviceAccountName"`
						Rules              []rbacv1.PolicyRule `json:"rules"`
					} `json:"permissions"`
					Deployments []struct {
						Name string                `json:"name"`
						Spec appsv1.DeploymentSpec `json:"spec"`
					} `json:"deployments"`
				} `json:"spec"`
			} `json:"install"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(csvBody, &csv))

	var tokenRule rbacv1.PolicyRule
	var sawToken, sawQuerySA bool
	for _, perm := range csv.Spec.Install.Spec.Permissions {
		if perm.ServiceAccountName == "attune-prometheus-query" {
			sawQuerySA = true
			assert.Empty(t, perm.Rules)
		}
		for _, rule := range perm.Rules {
			for _, resource := range rule.Resources {
				if resource == "serviceaccounts/token" {
					tokenRule = rule
					sawToken = true
				}
			}
		}
	}
	require.True(t, sawToken, "OLM permission must create tokens for the query ServiceAccount")
	require.True(t, sawQuerySA)
	assert.Equal(t, []string{""}, tokenRule.APIGroups)
	assert.Equal(t, []string{"serviceaccounts/token"}, tokenRule.Resources)
	assert.Equal(t, []string{"attune-prometheus-query"}, tokenRule.ResourceNames)
	assert.Equal(t, []string{"create"}, tokenRule.Verbs)

	var olmDep *appsv1.DeploymentSpec
	for i := range csv.Spec.Install.Spec.Deployments {
		if csv.Spec.Install.Spec.Deployments[i].Name == "attune-controller-manager" {
			olmDep = &csv.Spec.Install.Spec.Deployments[i].Spec
		}
	}
	require.NotNil(t, olmDep)
	assertManagerQueryArgs(t, appsv1.Deployment{Spec: *olmDep})

	renderedSA := decodeNamed[corev1.ServiceAccount](t, "dist/install.yaml", "ServiceAccount", "attune-prometheus-query")
	assert.Equal(t, "attune-prometheus-query", renderedSA.Name)
	renderedRole := decodeNamed[rbacv1.Role](t, "dist/install.yaml", "Role", "attune-prometheus-query-token")
	assertQueryTokenRole(t, renderedRole, "attune-prometheus-query")
	renderedBinding := decodeNamed[rbacv1.RoleBinding](t, "dist/install.yaml", "RoleBinding", "attune-prometheus-query-token")
	assertQuerySABinding(t, renderedBinding, "attune-prometheus-query-token", "attune-controller-manager")
	renderedDep := decodeNamed[appsv1.Deployment](t, "dist/install.yaml", "Deployment", "attune-controller-manager")
	assertManagerQueryArgs(t, renderedDep)
}
