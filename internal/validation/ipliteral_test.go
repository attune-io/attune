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

func TestParseIPv4Literal_RejectsOverflow(t *testing.T) {
	t.Parallel()
	// 2^32 + 127.0.0.1. A 32-bit mask turns this into 127.0.0.1.
	assert.Nil(t, parseIPv4Literal("6425673729"))
	assert.Nil(t, parseIPv4Literal("4294967296"))
	assert.Nil(t, parseIPv4Literal("0x100000001"))
	assert.Nil(t, parseIPv4Literal("040000000000"))
	assert.Nil(t, hostIP("6425673729"))
}

func TestParseIPv4Literal_AcceptsInRangeInetAton(t *testing.T) {
	t.Parallel()
	got := parseIPv4Literal("2130706433")
	require.NotNil(t, got)
	assert.True(t, got.Equal(net.ParseIP("127.0.0.1")))

	got = parseIPv4Literal("4294967295")
	require.NotNil(t, got)
	assert.True(t, got.Equal(net.ParseIP("255.255.255.255")))

	got = parseIPv4Literal("0xffffffff")
	require.NotNil(t, got)
	assert.True(t, got.Equal(net.ParseIP("255.255.255.255")))

	got = parseIPv4Literal("127.1")
	require.NotNil(t, got)
	assert.True(t, got.Equal(net.ParseIP("127.0.0.1")))
}
