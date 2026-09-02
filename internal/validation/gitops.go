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
	"net"
	"net/url"
	"strings"
)

// GitOpsAPIURL validates an optional GitOps forge API base URL.
// Unlike PrometheusAddress, this requires https and rejects RFC1918 /
// ULA / loopback / link-local / metadata targets. GitOps calls carry a
// bearer token and must not aim at in-cluster or cloud-metadata endpoints.
func GitOpsAPIURL(address string) error {
	return GitOpsAPIURLAllowingPrivate(address, false)
}

// GitOpsAPIURLAllowingPrivate is GitOpsAPIURL with an opt-in for RFC1918/ULA
// IP literals (self-hosted forges). Loopback, link-local, unspecified, and
// metadata hostnames stay blocked even when allowPrivate is true.
func GitOpsAPIURLAllowingPrivate(address string, allowPrivate bool) error {
	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}
	if parsed.User != nil {
		return fmt.Errorf("must not include userinfo")
	}

	hostname := strings.TrimSuffix(parsed.Hostname(), ".")
	if hostname == "" {
		return fmt.Errorf("host is required")
	}

	blockedHosts := []string{
		"metadata.google.internal",
		"metadata.internal",
		"instance-data.ec2.internal",
		"169.254.169.254",
		"fd00:ec2::254",
		"localhost",
	}
	lowerHost := strings.ToLower(hostname)
	for _, blocked := range blockedHosts {
		if lowerHost == blocked {
			return fmt.Errorf("address must not target cloud metadata endpoint %q", hostname)
		}
	}

	if ip := net.ParseIP(hostname); ip != nil {
		if allowPrivate {
			if GitOpsAlwaysBlockedIP(ip) {
				return fmt.Errorf("address must not target loopback, link-local, or metadata IP %q", hostname)
			}
		} else if GitOpsBlockedIP(ip) {
			return fmt.Errorf("address must not target loopback, link-local, private, or metadata IP %q", hostname)
		}
	}
	return nil
}

// awsIMDSv6 is the AWS EC2 Instance Metadata Service v2 IPv6 endpoint.
// It is ULA (fd00::/8), not link-local, so IsPrivate is true but
// IsLinkLocalUnicast is false. Must stay blocked when allowPrivate is set.
var awsIMDSv6 = net.ParseIP("fd00:ec2::254")

// GitOpsAlwaysBlockedIP reports addresses that stay blocked even when
// allowPrivateEndpoints is set: loopback, link-local (IMDS), unspecified,
// and AWS IPv6 IMDS.
func GitOpsAlwaysBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.Equal(awsIMDSv6)
}

// GitOpsBlockedIP reports whether an IP must not be dialed for GitOps HTTP.
// Stricter than Prometheus: RFC1918 and ULA are blocked because GitOps is
// not an in-cluster metrics scrape.
func GitOpsBlockedIP(ip net.IP) bool {
	if GitOpsAlwaysBlockedIP(ip) {
		return true
	}
	return ip.IsPrivate()
}
