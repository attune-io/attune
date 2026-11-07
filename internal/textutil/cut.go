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

// Package textutil cuts strings without producing invalid UTF-8.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// CutUTF8Bytes returns a valid UTF-8 prefix of msg of at most n bytes.
// Invalid byte sequences are dropped. A limit that lands inside a
// character stops before that character. n <= 0 returns an empty string.
func CutUTF8Bytes(msg string, n int) string {
	if n <= 0 {
		return ""
	}
	msg = strings.ToValidUTF8(msg, "")
	if len(msg) <= n {
		return msg
	}
	i := n
	for i > 0 && !utf8.RuneStart(msg[i]) {
		i--
	}
	return msg[:i]
}
