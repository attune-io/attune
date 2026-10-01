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

package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const quantityMinMaxRule = "quantity(string(self.minAllowed))"

func TestPolicyCRDQuantityRuleStaysOffContainerPolicies(t *testing.T) {
	t.Parallel()
	text := crdText(t, "attune.io_attunepolicies.yaml")
	if got := strings.Count(text, quantityMinMaxRule); got != 2 {
		t.Fatalf("policy CRD has %d quantity min/max rules, want 2 (spec.cpu and spec.memory)", got)
	}
	start := strings.Index(text, "\n              containerPolicies:\n")
	cpu := strings.Index(text, "\n              cpu:\n")
	if start < 0 || cpu <= start {
		t.Fatal("policy CRD is missing containerPolicies before spec.cpu")
	}
	if strings.Contains(text[start:cpu], quantityMinMaxRule) {
		t.Fatal("containerPolicies still carries the quantity min/max rule")
	}
}

func TestDefaultsCRDKeepsQuantityMinMaxRule(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"attune.io_attunedefaults.yaml",
		"attune.io_attunenamespacedefaults.yaml",
	} {
		text := crdText(t, name)
		if got := strings.Count(text, quantityMinMaxRule); got != 2 {
			t.Fatalf("%s has %d quantity min/max rules, want 2", name, got)
		}
	}
}

func crdText(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), "config", "crd", "bases", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
