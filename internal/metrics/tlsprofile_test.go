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

package metrics

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTLSMinVersionForProfile(t *testing.T) {
	tests := []struct {
		profile string
		want    uint16
	}{
		{"Modern", tls.VersionTLS13},
		{"Intermediate", tls.VersionTLS12},
		{"Old", tls.VersionTLS10},
		{"Custom", 0},
		{"", tls.VersionTLS12},
		{"Unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			assert.Equal(t, tt.want, TLSMinVersionForProfile(tt.profile))
		})
	}
}
