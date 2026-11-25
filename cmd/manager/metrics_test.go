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

package main

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

func TestMetricsServerOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		bind       string
		secure     bool
		wantSecure bool
		wantFilter bool
	}{
		{name: "secure default", bind: ":8080", secure: true, wantSecure: true, wantFilter: true},
		{name: "legacy http", bind: ":8080", secure: false, wantSecure: false, wantFilter: false},
		{name: "disabled ignores secure", bind: "0", secure: true, wantSecure: false, wantFilter: false},
		{name: "disabled and insecure", bind: "0", secure: false, wantSecure: false, wantFilter: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := metricsServerOptions(tt.bind, tt.secure)
			assert.Equal(t, tt.bind, got.BindAddress)
			assert.Equal(t, tt.wantSecure, got.SecureServing)
			if !tt.wantFilter {
				assert.Nil(t, got.FilterProvider)
				return
			}
			require.NotNil(t, got.FilterProvider)
			assert.Equal(t,
				reflect.ValueOf(filters.WithAuthenticationAndAuthorization).Pointer(),
				reflect.ValueOf(got.FilterProvider).Pointer())
		})
	}
}
