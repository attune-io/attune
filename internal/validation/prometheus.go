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
	"net/url"
	"strings"
)

var reservedPrometheusQueryParameters = map[string]struct{}{
	"query":   {},
	"time":    {},
	"start":   {},
	"end":     {},
	"step":    {},
	"timeout": {},
}

// PrometheusAddress validates that the Prometheus address is a valid URL
// with an allowed scheme and blocks loopback, link-local, and cloud
// metadata targets. Cluster-private addresses stay allowed.
//
// DNS names are not resolved here. The operator dialer checks the
// resolved address with GitOpsAlwaysBlockedIP, the shared dial
// blocklist. localhost stays allowed so kubectl attune doctor can
// reach a port-forward.
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

	// Auth belongs on bearerTokenSecret / headers, not the URL.
	if parsed.User != nil {
		return fmt.Errorf("must not include userinfo")
	}

	hostname := strings.TrimSuffix(parsed.Hostname(), ".")

	// Metadata hostnames come from GitOpsBlockedHost. localhost stays
	// allowed so kubectl attune doctor can reach a port-forward.
	// Literal and resolved IPs use GitOpsAlwaysBlockedIP, which the
	// Prometheus dialer calls after DNS. Private ranges stay allowed.
	if !strings.EqualFold(hostname, "localhost") && GitOpsBlockedHost(hostname) {
		return fmt.Errorf("address must not target cloud metadata endpoint %q", hostname)
	}

	// hostIP accepts inet_aton forms that net.ParseIP misses.
	if ip := hostIP(hostname); ip != nil && GitOpsAlwaysBlockedIP(ip) {
		return fmt.Errorf("address must not target loopback/metadata IP %q", hostname)
	}

	return nil
}

// PrometheusQueryParameters rejects parameters that would override
// operator-controlled Prometheus API request fields.
func PrometheusQueryParameters(params map[string]string) error {
	for key := range params {
		lowerKey := strings.ToLower(key)
		if _, reserved := reservedPrometheusQueryParameters[lowerKey]; reserved {
			return fmt.Errorf("query parameter %q is reserved by the Prometheus API and cannot be overridden", key)
		}
	}
	return nil
}
