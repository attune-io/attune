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

package weeklyreport

import (
	"fmt"
	"strings"
)

// Endpoint is one chat destination. The URL lives in a Secret.
type Endpoint struct {
	Name       string
	SecretName string
	SecretKey  string
}

// ParseEndpoints checks repeatable name/secretName/secretKey flags.
// An empty list is valid. The process may still start with none configured.
func ParseEndpoints(raw []string) ([]Endpoint, error) {
	if len(raw) > 2 {
		return nil, fmt.Errorf("at most two weekly report endpoints")
	}
	out := make([]Endpoint, 0, len(raw))
	seenName := map[string]struct{}{}
	seenSecret := map[string]struct{}{}
	for _, item := range raw {
		parts := strings.Split(item, "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("weekly report endpoint must be name/secretName/secretKey")
		}
		ep := Endpoint{Name: parts[0], SecretName: parts[1], SecretKey: parts[2]}
		if !dnsLabel(ep.Name) {
			return nil, fmt.Errorf("weekly report endpoint name must be a DNS label")
		}
		if _, ok := seenName[ep.Name]; ok {
			return nil, fmt.Errorf("duplicate weekly report endpoint name")
		}
		key := ep.SecretName + "\x00" + ep.SecretKey
		if _, ok := seenSecret[key]; ok {
			return nil, fmt.Errorf("duplicate weekly report secret")
		}
		seenName[ep.Name] = struct{}{}
		seenSecret[key] = struct{}{}
		out = append(out, ep)
	}
	return out, nil
}

func dnsLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
