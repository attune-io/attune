/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations under
the License.
*/

package gitops

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitopsDialBlocked(t *testing.T) {
	t.Parallel()
	private := net.ParseIP("10.1.2.3")
	loopback := net.ParseIP("127.0.0.1")
	imds := net.ParseIP("169.254.169.254")
	public := net.ParseIP("1.1.1.1")
	require.NotNil(t, private)
	require.NotNil(t, loopback)
	require.NotNil(t, imds)
	require.NotNil(t, public)

	assert.True(t, gitopsDialBlocked(private, false))
	assert.False(t, gitopsDialBlocked(private, true))
	assert.True(t, gitopsDialBlocked(loopback, true), "loopback stays blocked")
	assert.True(t, gitopsDialBlocked(imds, true), "IMDS stays blocked")
	assert.False(t, gitopsDialBlocked(public, false))
	assert.False(t, gitopsDialBlocked(public, true))
}

func TestGitopsDialBlocked_AWSIMDSv6(t *testing.T) {
	t.Parallel()
	imds := net.ParseIP("fd00:ec2::254")
	require.NotNil(t, imds)
	assert.True(t, gitopsDialBlocked(imds, false))
	assert.True(t, gitopsDialBlocked(imds, true), "IPv6 IMDS stays blocked when allowPrivate is set")
}
