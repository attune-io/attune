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

func TestMatchGlob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{pattern: "abc", value: "abc", want: true},
		{pattern: "abc", value: "abcd", want: false},
		{pattern: "*", value: "arn:aws:iam::1:role/a/b", want: true},
		{pattern: "arn:aws:iam::123:role/*", value: "arn:aws:iam::123:role/prod-amp-reader", want: true},
		{pattern: "arn:aws:iam::123:role/*", value: "arn:aws:iam::999:role/prod-amp-reader", want: false},
		{pattern: "*.amazonaws.com", value: "aps-workspaces.us-east-1.amazonaws.com", want: true},
		{pattern: "aps-*.amazonaws.com", value: "aps-workspaces.us-east-1.amazonaws.com", want: true},
		{pattern: "", value: "", want: true},
		{pattern: "", value: "x", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+" "+tt.value, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, MatchGlob(tt.pattern, tt.value))
		})
	}
}

func TestVPANamespace(t *testing.T) {
	t.Parallel()
	require.NoError(t, VPANamespace("team-a", "", false))
	require.NoError(t, VPANamespace("team-a", "team-a", false))
	require.NoError(t, VPANamespace("team-a", "other", true))
	err := VPANamespace("team-a", "other", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "other")
	assert.Contains(t, err.Error(), "team-a")
}

func TestSigV4PolicyAllowed(t *testing.T) {
	t.Parallel()
	allow := SigV4Allowlist{
		RoleARNs: []string{"arn:aws:iam::123456789012:role/attune-*"},
		Hosts:    []string{"aps-workspaces.us-east-1.amazonaws.com"},
	}
	role := "arn:aws:iam::123456789012:role/attune-amp"
	address := "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws"
	require.NoError(t, SigV4PolicyAllowed(address, role, allow))
	require.NoError(t, SigV4PolicyAllowed(address, "", allow))

	err := SigV4PolicyAllowed(address, role, SigV4Allowlist{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sigv4-allowed-role-arns")

	err = SigV4PolicyAllowed(address, "", SigV4Allowlist{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sigv4-allowed-workspace-hosts")

	err = SigV4PolicyAllowed("https://example.com/workspaces/ws", "", allow)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "example.com")
}

func TestCloudWatchRoleAllowed(t *testing.T) {
	t.Parallel()
	role := "arn:aws:iam::123456789012:role/attune-cw"
	require.NoError(t, CloudWatchRoleAllowed("", SigV4Allowlist{}))
	err := CloudWatchRoleAllowed(role, SigV4Allowlist{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sigv4-allowed-role-arns")
	require.NoError(t, CloudWatchRoleAllowed(role, SigV4Allowlist{RoleARNs: []string{role}}))
}
