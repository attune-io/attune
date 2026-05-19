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

// Package validation provides shared validation functions used by both the
// admission webhooks and the controller for defense-in-depth.
package validation

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// PrometheusAddress validates that the Prometheus address is a valid URL
// with an allowed scheme and blocks SSRF against private/metadata endpoints.
func PrometheusAddress(address string) error {
	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", parsed.Scheme)
	}

	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}

	hostname := parsed.Hostname()

	// Block cloud metadata endpoints (hostnames and IPs).
	blockedHosts := []string{
		"metadata.google.internal",
		"metadata.internal",
		"instance-data.ec2.internal",
		"169.254.169.254",
	}
	lowerHost := strings.ToLower(hostname)
	for _, blocked := range blockedHosts {
		if lowerHost == blocked {
			return fmt.Errorf("address must not target cloud metadata endpoint %q", hostname)
		}
	}

	// Block loopback and link-local IPs (cloud metadata lives at 169.254.169.254).
	// Private IPs (10.x, 172.16.x, 192.168.x) are NOT blocked because Prometheus
	// typically runs on a ClusterIP service inside the cluster.
	if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
			ip.IsLinkLocalMulticast() || ip.Equal(net.ParseIP("fd00:ec2::254")) {
			return fmt.Errorf("address must not target loopback/metadata IP %q", hostname)
		}
	}

	return nil
}
