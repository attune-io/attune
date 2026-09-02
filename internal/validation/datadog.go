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

import "fmt"

// validDatadogSites is the allowlist of Datadog site values used to
// construct https://api.<site>. Empty site means the caller default.
var validDatadogSites = map[string]bool{
	"datadoghq.com":     true,
	"datadoghq.eu":      true,
	"us3.datadoghq.com": true,
	"us5.datadoghq.com": true,
	"ap1.datadoghq.com": true,
	"ddog-gov.com":      true,
}

// DatadogSite reports whether site is empty (caller default) or an
// allowlisted Datadog site. Invalid values must not be interpolated
// into https://api.<site>.
func DatadogSite(site string) error {
	if site == "" {
		return nil
	}
	if !validDatadogSites[site] {
		return fmt.Errorf("site %q is not a recognized Datadog site", site)
	}
	return nil
}
