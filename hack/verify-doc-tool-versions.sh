#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that supported tool version references stay consistent across
# user-facing documentation entry points.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rc=0

check_literal() {
  local label="$1" expected="$2"
  shift 2
  local files=("$@")

  for f in "${files[@]}"; do
    if ! grep -Fq "$expected" "$REPO_ROOT/$f"; then
      echo "FAIL: $label: literal '$expected' not found in $f"
      rc=1
    fi
  done
}

helm_files=(
  "README.md"
  "CONTRIBUTING.md"
  "docs/getting-started/installation.md"
  "docs/contributing/development.md"
  "charts/attune/README.md.gotmpl"
  "charts/attune/README.md"
)

cluster_tool_files=(
  "CONTRIBUTING.md"
  "docs/contributing/development.md"
  "docs/contributing/testing.md"
)

check_literal "Helm support range" '3.16+ or 4.x' "${helm_files[@]}"
check_literal "k3d/Kind support range" 'k3d 5.8+ / Kind 0.24+' "${cluster_tool_files[@]}"

if [ $rc -ne 0 ]; then
  echo
  echo "ERROR: Supported tool version references are inconsistent."
  exit 1
fi

echo "OK: Supported tool version references are consistent."
