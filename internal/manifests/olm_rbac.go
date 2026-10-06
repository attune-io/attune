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

// Package manifests checks install manifests that are kept beside the Go
// module, including the OperatorHub ClusterServiceVersion.
package manifests

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// SyncClusterPermissions copies ClusterRole rules into the CSV
// clusterPermissions block. Leader election is not in role.yaml, so the
// leases rule already on the CSV stays, and the namespaced permissions
// block is left as it is.
func SyncClusterPermissions(csvYAML, roleYAML []byte) ([]byte, error) {
	roleRules, err := parseClusterRoleRules(roleYAML)
	if err != nil {
		return nil, err
	}
	csvRules, nPerms, err := parseCSVClusterRules(csvYAML)
	if err != nil {
		return nil, err
	}
	if nPerms != 1 {
		return nil, fmt.Errorf("csv has %d clusterPermissions entries, want 1", nPerms)
	}
	rewritten, err := spliceClusterRules(csvYAML, mergeRoleAndLease(roleRules, csvRules))
	if err != nil {
		return nil, err
	}
	return rewritten, nil
}

func parseClusterRoleRules(roleYAML []byte) ([]rbacv1.PolicyRule, error) {
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(roleYAML, &role); err != nil {
		return nil, fmt.Errorf("parse cluster role: %w", err)
	}
	if len(role.Rules) == 0 {
		return nil, errors.New("cluster role has no rules")
	}
	return role.Rules, nil
}

type csvInstall struct {
	Spec struct {
		Install struct {
			Spec struct {
				ClusterPermissions []struct {
					Rules []rbacv1.PolicyRule `json:"rules"`
				} `json:"clusterPermissions"`
			} `json:"spec"`
		} `json:"install"`
	} `json:"spec"`
}

func parseCSVClusterRules(csvYAML []byte) ([]rbacv1.PolicyRule, int, error) {
	var doc csvInstall
	if err := yaml.Unmarshal(csvYAML, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse csv: %w", err)
	}
	perms := doc.Spec.Install.Spec.ClusterPermissions
	if len(perms) == 0 {
		return nil, 0, errors.New("csv has no clusterPermissions")
	}
	return perms[0].Rules, len(perms), nil
}

func isLeaseRule(rule rbacv1.PolicyRule) bool {
	return len(rule.APIGroups) == 1 && rule.APIGroups[0] == "coordination.k8s.io" &&
		len(rule.Resources) == 1 && rule.Resources[0] == "leases" &&
		len(rule.ResourceNames) == 0 && len(rule.NonResourceURLs) == 0
}

func leaderElectionLeaseRule() rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups: []string{"coordination.k8s.io"},
		Resources: []string{"leases"},
		Verbs:     []string{"create", "delete", "get", "list", "update", "watch"},
	}
}

func mergeRoleAndLease(roleRules, csvRules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	out := append([]rbacv1.PolicyRule(nil), roleRules...)
	haveLease := false
	for _, rule := range out {
		if isLeaseRule(rule) {
			haveLease = true
			break
		}
	}
	if !haveLease {
		for _, rule := range csvRules {
			if isLeaseRule(rule) {
				out = append(out, rule)
				haveLease = true
				break
			}
		}
	}
	if !haveLease {
		out = append(out, leaderElectionLeaseRule())
	}
	return out
}

func spliceClusterRules(csvYAML []byte, rules []rbacv1.PolicyRule) ([]byte, error) {
	text := string(csvYAML)
	lines := strings.Split(text, "\n")
	clusterAt := -1
	clusterIndent := 0
	rulesAt := -1
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if clusterAt < 0 && strings.HasPrefix(trim, "clusterPermissions:") {
			clusterAt = i
			clusterIndent = leadingSpaces(line)
			continue
		}
		if clusterAt >= 0 && rulesAt < 0 && strings.HasPrefix(trim, "rules:") {
			rulesAt = i
			continue
		}
		if rulesAt >= 0 && leadingSpaces(line) <= clusterIndent {
			indent := leadingSpaces(lines[rulesAt])
			lines[rulesAt] = strings.Repeat(" ", indent) + "rules:"
			rendered := renderRules(rules, indent+2)
			var b strings.Builder
			for _, kept := range lines[:rulesAt+1] {
				b.WriteString(kept)
				b.WriteByte('\n')
			}
			b.WriteString(rendered)
			for j := i; j < len(lines); j++ {
				b.WriteString(lines[j])
				if j != len(lines)-1 {
					b.WriteByte('\n')
				}
			}
			return []byte(b.String()), nil
		}
	}
	return nil, errors.New("csv clusterPermissions rules block not found")
}

func renderRules(rules []rbacv1.PolicyRule, dashIndent int) string {
	keyIndent := strings.Repeat(" ", dashIndent+2)
	itemIndent := strings.Repeat(" ", dashIndent+4)
	dash := strings.Repeat(" ", dashIndent)
	var b strings.Builder
	for _, rule := range rules {
		first := true
		write := func(key string, vals []string) {
			if len(vals) == 0 {
				return
			}
			if first {
				b.WriteString(dash)
				b.WriteString("- ")
				b.WriteString(key)
				b.WriteString(":\n")
				first = false
			} else {
				b.WriteString(keyIndent)
				b.WriteString(key)
				b.WriteString(":\n")
			}
			for _, val := range vals {
				b.WriteString(itemIndent)
				b.WriteString("- ")
				b.WriteString(yamlScalar(val))
				b.WriteString("\n")
			}
		}
		write("apiGroups", rule.APIGroups)
		write("resources", rule.Resources)
		write("resourceNames", rule.ResourceNames)
		write("nonResourceURLs", rule.NonResourceURLs)
		write("verbs", rule.Verbs)
	}
	return b.String()
}

func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") || strings.Contains(s, " ") {
		return fmt.Sprintf("%q", s)
	}
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "true", "false", "on", "off", "null":
		return fmt.Sprintf("%q", s)
	}
	return s
}

func leadingSpaces(s string) int {
	n := 0
	for _, c := range s {
		if c != ' ' {
			break
		}
		n++
	}
	return n
}

// permissionTriples lists group, resource, resourceName, and verb.
// The leases rule is omitted. An empty resourceName means every name.
func permissionTriples(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, rule := range rules {
		if isLeaseRule(rule) {
			continue
		}
		groups := rule.APIGroups
		if len(groups) == 0 {
			groups = []string{""}
		}
		names := rule.ResourceNames
		if len(names) == 0 {
			names = []string{""}
		}
		resources := append([]string(nil), rule.Resources...)
		for _, u := range rule.NonResourceURLs {
			resources = append(resources, "url:"+u)
		}
		for _, group := range groups {
			for _, resource := range resources {
				for _, name := range names {
					for _, verb := range rule.Verbs {
						out = append(out, group+" "+resource+" "+name+" "+verb)
					}
				}
			}
		}
	}
	return out
}

func grantsEqual(roleRules, csvRules []rbacv1.PolicyRule) bool {
	return bytes.Equal(sortedJoin(permissionTriples(roleRules)), sortedJoin(permissionTriples(csvRules)))
}

func sortedJoin(items []string) []byte {
	cp := append([]string(nil), items...)
	slices.Sort(cp)
	return []byte(strings.Join(cp, "\n"))
}
