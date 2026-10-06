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

// Command sync-olm-rbac copies config/rbac/role.yaml into the
// ClusterServiceVersion clusterPermissions block.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/attune-io/attune/internal/manifests"
)

func main() {
	if err := run(); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	rolePath := flag.String("role", "", "path to config/rbac/role.yaml")
	csvPath := flag.String("csv", "", "path to the ClusterServiceVersion YAML to update")
	flag.Parse()
	if *rolePath == "" || *csvPath == "" {
		return fmt.Errorf("both -role and -csv are required")
	}
	roleYAML, err := os.ReadFile(*rolePath) //nolint:gosec // CLI paths are the operator's own manifests
	if err != nil {
		return fmt.Errorf("read role: %w", err)
	}
	csvYAML, err := os.ReadFile(*csvPath) //nolint:gosec // CLI paths are the operator's own manifests
	if err != nil {
		return fmt.Errorf("read csv: %w", err)
	}
	updated, err := manifests.SyncClusterPermissions(csvYAML, roleYAML)
	if err != nil {
		return err
	}
	info, err := os.Stat(*csvPath)
	if err != nil {
		return fmt.Errorf("stat csv: %w", err)
	}
	if err := os.WriteFile(*csvPath, updated, info.Mode()); err != nil { //nolint:gosec // CLI path is the operator manifest the caller named
		return fmt.Errorf("write csv: %w", err)
	}
	return nil
}
