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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatadogSite(t *testing.T) {
	t.Parallel()
	valid := []string{
		"",
		"datadoghq.com",
		"datadoghq.eu",
		"us3.datadoghq.com",
		"us5.datadoghq.com",
		"ap1.datadoghq.com",
		"ddog-gov.com",
	}
	for _, site := range valid {
		assert.NoError(t, DatadogSite(site), "expected valid: %q", site)
	}

	err := DatadogSite("evil.example")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a recognized Datadog site")

	err = DatadogSite("datadoghq.com.evil.example")
	require.Error(t, err)
}
