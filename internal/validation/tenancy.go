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

package validation

import (
	"fmt"
	"net/url"
	"strings"
)

// SigV4Allowlist is the operator's set of IAM roles and workspace hosts
// a namespace author may name. Empty lists match nothing.
type SigV4Allowlist struct {
	RoleARNs []string
	Hosts    []string
}

// RoleAllowed reports whether roleARN matches a configured glob.
func (a SigV4Allowlist) RoleAllowed(roleARN string) bool {
	roleARN = strings.TrimSpace(roleARN)
	if roleARN == "" {
		return false
	}
	for _, pattern := range a.RoleARNs {
		if MatchGlob(strings.TrimSpace(pattern), roleARN) {
			return true
		}
	}
	return false
}

// HostAllowed reports whether host matches a configured glob.
func (a SigV4Allowlist) HostAllowed(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	for _, pattern := range a.Hosts {
		if MatchGlob(strings.TrimSpace(pattern), host) {
			return true
		}
	}
	return false
}

// VPANamespace rejects a VerticalPodAutoscaler namespace other than the
// object's own namespace. allowCross is for cluster AttuneDefaults.
// An empty vpaNamespace means the policy namespace.
func VPANamespace(objectNamespace, vpaNamespace string, allowCross bool) error {
	vpaNamespace = strings.TrimSpace(vpaNamespace)
	if allowCross || vpaNamespace == "" || vpaNamespace == objectNamespace {
		return nil
	}
	if objectNamespace == "" {
		return fmt.Errorf("metricsSource.vpa.namespace %q must be empty or the object's namespace", vpaNamespace)
	}
	return fmt.Errorf("metricsSource.vpa.namespace %q must be %q or empty", vpaNamespace, objectNamespace)
}

// SigV4PolicyAllowed checks a namespace-authored Prometheus sigv4 block.
// A roleArn must match the role allowlist. An empty roleArn signs with
// the operator identity and the address host must match the host allowlist.
func SigV4PolicyAllowed(address, roleARN string, allow SigV4Allowlist) error {
	roleARN = strings.TrimSpace(roleARN)
	if roleARN != "" {
		if allow.RoleAllowed(roleARN) {
			return nil
		}
		return fmt.Errorf("metricsSource.prometheus.sigv4.roleArn %q is not in --sigv4-allowed-role-arns", roleARN)
	}
	host := prometheusHost(address)
	if allow.HostAllowed(host) {
		return nil
	}
	return fmt.Errorf("metricsSource.prometheus.sigv4 without roleArn uses the operator identity and host %q is not in --sigv4-allowed-workspace-hosts", host)
}

// CloudWatchRoleAllowed checks a namespace-authored CloudWatch roleArn.
// An empty role uses the operator identity for the constrained Container
// Insights query and is left unchanged. A set role must match the role allowlist.
func CloudWatchRoleAllowed(roleARN string, allow SigV4Allowlist) error {
	roleARN = strings.TrimSpace(roleARN)
	if roleARN == "" || allow.RoleAllowed(roleARN) {
		return nil
	}
	return fmt.Errorf("metricsSource.cloudwatch.roleArn %q is not in --sigv4-allowed-role-arns", roleARN)
}

func prometheusHost(address string) string {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
