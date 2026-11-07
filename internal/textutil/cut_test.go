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

package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCutUTF8Bytes(t *testing.T) {
	assert.Equal(t, "", CutUTF8Bytes("abc", 0))
	assert.Equal(t, "abc", CutUTF8Bytes("abc", 10))

	exact := strings.Repeat("a", 158) + "é"
	assert.Equal(t, exact, CutUTF8Bytes(exact+"MORE", 160))

	split := strings.Repeat("a", 159) + "éMORE"
	got := CutUTF8Bytes(split, 160)
	assert.Equal(t, strings.Repeat("a", 159), got)
	assert.LessOrEqual(t, len(got), 160)

	bad := "hello\xffworld"
	got = CutUTF8Bytes(bad, 20)
	require.True(t, utf8.ValidString(got))
	assert.Equal(t, "helloworld", got)
	assert.NotContains(t, got, "\xff")
}
