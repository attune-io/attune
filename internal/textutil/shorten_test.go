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

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/validate/content"
)

func TestShortenWithHash_FitsUnchanged(t *testing.T) {
	assert.Equal(t, "", ShortenWithHash("", 63))
	assert.Equal(t, "my-app", ShortenWithHash("my-app", 63))
	exact := strings.Repeat("a", 63)
	assert.Equal(t, exact, ShortenWithHash(exact, 63))
}

func TestShortenWithHash_TooLong(t *testing.T) {
	// Kubernetes names are DNS-1123 subdomains: up to 253 characters,
	// lowercase alphanumerics, '-' and '.'. Cuts that land on '-' or '.'
	// must still produce valid label values and qualified names.
	inputs := []string{
		strings.Repeat("a", 64),
		strings.Repeat("a", 253),
		strings.Repeat("a", 51) + "-" + strings.Repeat("b", 20),
		strings.Repeat("a", 51) + "." + strings.Repeat("b", 20),
		strings.Repeat("ab-", 30),
		strings.Repeat("x.y", 40),
	}
	seen := map[string]string{}
	for _, in := range inputs {
		got := ShortenWithHash(in, content.LabelValueMaxLength)
		assert.LessOrEqual(t, len(got), content.LabelValueMaxLength, in)
		assert.Empty(t, content.IsLabelValue(got), "label value %q", got)
		assert.Empty(t, content.IsLabelKey(got), "qualified name %q", got)
		assert.Equal(t, got, ShortenWithHash(in, content.LabelValueMaxLength), "deterministic")
		if prev, dup := seen[got]; dup {
			t.Fatalf("collision: %q and %q both map to %q", prev, in, got)
		}
		seen[got] = in

		key := "attune.io/last-resize-time." + ShortenWithHash(in, 46)
		assert.Empty(t, content.IsLabelKey(key), "annotation key %q", key)
	}
}

func TestShortenWithHash_SharedPrefixDistinct(t *testing.T) {
	base := strings.Repeat("a", 70)
	a := ShortenWithHash(base+"-one", 63)
	b := ShortenWithHash(base+"-two", 63)
	assert.NotEqual(t, a, b)
	assert.Len(t, a, 63)
	assert.Equal(t, a[:52], b[:52], "readable prefix kept")
}

func TestShortenWithHash_TinyLimit(t *testing.T) {
	assert.Len(t, ShortenWithHash("abcdefghijkl", 5), 5)
	assert.Empty(t, ShortenWithHash("abc", 0))
}
