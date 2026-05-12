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
)

func TestPrometheusAddress_Valid(t *testing.T) {
	valid := []string{
		"http://prometheus-server.monitoring:80",
		"https://prometheus.monitoring:9090",
		"http://10.96.0.1:9090",
	}
	for _, addr := range valid {
		assert.NoError(t, PrometheusAddress(addr), "expected valid: %s", addr)
	}
}

func TestPrometheusAddress_BlockedMetadata(t *testing.T) {
	blocked := []string{
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata",
		"http://METADATA.GOOGLE.INTERNAL/foo",
		"http://instance-data.ec2.internal",
		"http://metadata.internal",
		"http://[fd00:ec2::254]/latest/meta-data/",
	}
	for _, addr := range blocked {
		err := PrometheusAddress(addr)
		assert.Error(t, err, "expected blocked: %s", addr)
	}
}

func TestPrometheusAddress_BlockedLoopback(t *testing.T) {
	assert.Error(t, PrometheusAddress("http://127.0.0.1:9090"))
	assert.Error(t, PrometheusAddress("http://[::1]:9090"))
}

func TestPrometheusAddress_BlockedLinkLocal(t *testing.T) {
	assert.Error(t, PrometheusAddress("http://[fe80::1]:9090"))
}

func TestPrometheusAddress_BadScheme(t *testing.T) {
	assert.Error(t, PrometheusAddress("ftp://prometheus:9090"))
}

func TestPrometheusAddress_NoHost(t *testing.T) {
	assert.Error(t, PrometheusAddress("http://"))
}
