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
	"crypto/sha256"
	"encoding/hex"
)

// shortHashLen is the number of hex characters of the SHA-256 digest that
// ShortenWithHash appends (40 bits).
const shortHashLen = 10

// ShortenWithHash returns s unchanged when it is at most maxLen bytes.
// A longer s is cut and suffixed with "-" and a short hash of the whole
// of s, so the result is at most maxLen bytes, deterministic, distinct
// for distinct inputs (up to hash collisions) and ends with a lowercase
// hex character. Used to fit Kubernetes names into the 63-character
// limit of label values and annotation key names. If maxLen is too small
// to hold a prefix, the result is the hash cut to maxLen.
func ShortenWithHash(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])[:shortHashLen]
	keep := maxLen - len(h) - 1
	if keep <= 0 {
		return CutUTF8Bytes(h, maxLen)
	}
	return CutUTF8Bytes(s, keep) + "-" + h
}
