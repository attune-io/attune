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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudWatchClusterName_Valid(t *testing.T) {
	t.Parallel()
	valid := []string{
		"c",
		"prod",
		"my-eks-cluster",
		"My_Cluster-1",
		"0start-with-digit",
		strings.Repeat("a", 100),
	}
	for _, name := range valid {
		assert.NoError(t, CloudWatchClusterName(name), "expected valid: %q", name)
	}
}

func TestCloudWatchClusterName_RejectsSEARCHInjection(t *testing.T) {
	t.Parallel()
	// Quote-break that would close ClusterName="..." and inject extra SEARCH terms.
	hostile := `x" Namespace="kube-system" MetricName="container_memory_working_set`
	require.Error(t, CloudWatchClusterName(hostile))
}

func TestCloudWatchClusterName_RejectsHostile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"double quote", `evil"`},
		{"single quote", "evil'"},
		{"backslash", `evil\`},
		{"space", "my cluster"},
		{"tab", "my\tcluster"},
		{"newline", "my\ncluster"},
		{"SEARCH wildcard", "prod*"},
		{"dot", "prod.cluster"},
		{"slash", "prod/cluster"},
		{"leading hyphen", "-prod"},
		{"leading underscore", "_prod"},
		{"too long", strings.Repeat("a", 101)},
		{"unicode quote", "prod\u201c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, CloudWatchClusterName(tt.value), "expected rejected: %q", tt.value)
		})
	}
}

func TestCloudWatchRoleARN_Valid(t *testing.T) {
	t.Parallel()
	valid := []string{
		"",
		"arn:aws:iam::123456789012:role/CloudWatchReadOnly",
		"arn:aws:iam::123456789012:role/path/to/role",
		"arn:aws-us-gov:iam::123456789012:role/Role",
		"arn:aws-cn:iam::123456789012:role/role.with.dots",
	}
	for _, arn := range valid {
		assert.NoError(t, CloudWatchRoleARN(arn), "expected valid: %q", arn)
	}
}

func TestCloudWatchRoleARN_RejectsHostile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"quotes", `arn:aws:iam::123456789012:role/x" extra`},
		{"spaces", "arn:aws:iam::123456789012:role/my role"},
		{"backslash", `arn:aws:iam::123456789012:role/foo\`},
		{"not an arn", "CloudWatchReadOnly"},
		{"sts assumed-role", "arn:aws:sts::123456789012:assumed-role/foo/session"},
		{"user arn", "arn:aws:iam::123456789012:user/alice"},
		{"short account", "arn:aws:iam::123:role/foo"},
		{"s3 arn", "arn:aws:s3:::bucket/key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, CloudWatchRoleARN(tt.value), "expected rejected: %q", tt.value)
		})
	}
}

func TestCloudWatchSEARCHToken_RejectsDelimiters(t *testing.T) {
	t.Parallel()
	assert.NoError(t, CloudWatchSEARCHToken("default"))
	assert.NoError(t, CloudWatchSEARCHToken("kube-system"))
	assert.Error(t, CloudWatchSEARCHToken(`ns"`))
	assert.Error(t, CloudWatchSEARCHToken("ns'"))
	assert.Error(t, CloudWatchSEARCHToken(`ns\`))
	assert.Error(t, CloudWatchSEARCHToken("ns name"))
}
