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
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitOpsAPIURL_Valid(t *testing.T) {
	t.Parallel()
	valid := []string{
		"https://api.github.com",
		"https://ghe.example.com/api/v3",
		"https://gitlab.example.com/api/v4",
		"https://8.8.8.8/api",
	}
	for _, addr := range valid {
		assert.NoError(t, GitOpsAPIURL(addr), "expected valid: %s", addr)
	}
}

func TestGitOpsAPIURL_RejectsHTTP(t *testing.T) {
	t.Parallel()
	err := GitOpsAPIURL("http://ghe.example.com/api/v3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestGitOpsAPIURL_RejectsIMDS(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"https://169.254.169.254/",
		"https://169.254.169.254/latest/meta-data/",
		"https://[fd00:ec2::254]/",
		"https://[fd00:ec2::254]/latest/meta-data/",
		"https://metadata.google.internal/computeMetadata",
		"https://METADATA.GOOGLE.INTERNAL/foo",
		"https://instance-data.ec2.internal",
		"https://metadata.internal",
	}
	for _, addr := range blocked {
		err := GitOpsAPIURL(addr)
		assert.Error(t, err, "expected blocked: %s", addr)
	}
}

func TestGitOpsAPIURL_RejectsPrivateAndLoopback(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"https://127.0.0.1:443",
		"https://[::1]/",
		"https://localhost/",
		"https://10.96.0.1/",
		"https://192.168.1.1/",
		"https://172.16.0.1/",
		"https://[fe80::1]/",
		"https://0.0.0.0/",
		"https://[::]/",
		"https://[fd00::1]/",
	}
	for _, addr := range blocked {
		assert.Error(t, GitOpsAPIURL(addr), "expected blocked: %s", addr)
	}
}

func TestGitOpsAPIURL_RejectsUserinfo(t *testing.T) {
	t.Parallel()
	assert.Error(t, GitOpsAPIURL("https://user:token@ghe.example.com"))
	assert.Error(t, GitOpsAPIURL("https://user@ghe.example.com"))
}

func TestGitOpsAPIURL_RejectsBadSchemeAndHost(t *testing.T) {
	t.Parallel()
	assert.Error(t, GitOpsAPIURL("ftp://ghe.example.com"))
	assert.Error(t, GitOpsAPIURL("https://"))
	assert.Error(t, GitOpsAPIURL("not a url"))
}

func TestGitOpsBlockedIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"0.0.0.0", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"10.96.0.1", true},
		{"fd00:ec2::254", true},
		{"fd00::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"::ffff:169.254.169.254", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:8.8.8.8", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip)
			assert.Equal(t, tt.blocked, GitOpsBlockedIP(ip), tt.ip)
		})
	}
}

func TestGitOpsAPIURL_AllowPrivateRFC1918(t *testing.T) {
	t.Parallel()
	assert.NoError(t, GitOpsAPIURLAllowingPrivate("https://10.96.0.1/", true))
	assert.NoError(t, GitOpsAPIURLAllowingPrivate("https://192.168.1.10:8443/", true))
	assert.NoError(t, GitOpsAPIURLAllowingPrivate("https://[fd00::1]/", true))
	assert.Error(t, GitOpsAPIURLAllowingPrivate("https://127.0.0.1/", true))
	assert.Error(t, GitOpsAPIURLAllowingPrivate("https://169.254.169.254/", true))
	assert.Error(t, GitOpsAPIURLAllowingPrivate("https://[fd00:ec2::254]/", true), "IPv6 IMDS stays blocked")
	assert.Error(t, GitOpsAPIURLAllowingPrivate("https://localhost/", true))
	assert.Error(t, GitOpsAPIURL("https://10.96.0.1/"), "default still blocks RFC1918")
}

func TestGitOpsBlockedHost(t *testing.T) {
	t.Parallel()
	assert.True(t, GitOpsBlockedHost("metadata.google.internal"))
	assert.True(t, GitOpsBlockedHost("METADATA.GOOGLE.INTERNAL"))
	assert.True(t, GitOpsBlockedHost("metadata.google.internal."))
	assert.True(t, GitOpsBlockedHost("169.254.169.254"))
	assert.True(t, GitOpsBlockedHost("fd00:ec2::254"))
	assert.True(t, GitOpsBlockedHost("localhost"))
	assert.False(t, GitOpsBlockedHost("api.github.com"))
	assert.False(t, GitOpsBlockedHost("ghe.example.com"))
}

func TestGitOpsAlwaysBlockedIP(t *testing.T) {
	t.Parallel()
	assert.True(t, GitOpsAlwaysBlockedIP(net.ParseIP("127.0.0.1")))
	assert.True(t, GitOpsAlwaysBlockedIP(net.ParseIP("169.254.169.254")))
	assert.True(t, GitOpsAlwaysBlockedIP(net.ParseIP("::1")))
	assert.True(t, GitOpsAlwaysBlockedIP(net.ParseIP("fd00:ec2::254")), "AWS IPv6 IMDS")
	assert.False(t, GitOpsAlwaysBlockedIP(net.ParseIP("10.0.0.1")))
	assert.False(t, GitOpsAlwaysBlockedIP(net.ParseIP("8.8.8.8")))
}

func TestPrometheusAddress_StillAllowsPrivate(t *testing.T) {
	t.Parallel()
	// GitOps hardening must not change in-cluster Prometheus validation.
	assert.NoError(t, PrometheusAddress("http://10.96.0.1:9090"))
	assert.NoError(t, PrometheusAddress("http://192.168.1.1:9090"))
	assert.Error(t, GitOpsAPIURL("https://10.96.0.1:9090"))
}
