#!/usr/bin/env bash
# Copyright 2026 kube-rightsize Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that critical user-facing defaults are consistent across docs,
# CRDs, Go code, and chart values. Exits non-zero on the first mismatch.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rc=0

check_default() {
  local label="$1" pattern="$2"
  shift 2
  local files=("$@")

  for f in "${files[@]}"; do
    if [ ! -f "$REPO_ROOT/$f" ]; then
      echo "WARN: $f does not exist, skipping"
      continue
    fi
    if ! grep -q "$pattern" "$REPO_ROOT/$f"; then
      echo "FAIL: $label: pattern '$pattern' not found in $f"
      rc=1
    fi
  done
}

# --- minimumDataPoints default = 48 ---
# Go code (canonical source of truth)
check_default "minimumDataPoints (Go)" \
  "defaultMinimumDataPoints.*= 48" \
  "internal/controller/rightsizepolicy_controller.go"

# CRD schemas (generated, should say default: 48)
check_default "minimumDataPoints (CRD policy)" \
  "default: 48" \
  "config/crd/bases/rightsize.io_rightsizepolicies.yaml"
check_default "minimumDataPoints (CRD defaults)" \
  "default: 48" \
  "config/crd/bases/rightsize.io_rightsizedefaults.yaml"

# Docs that state the default
check_default "minimumDataPoints (API ref)" \
  "minimumDataPoints: 48" \
  "docs/reference/api.md"
check_default "minimumDataPoints (quickstart)" \
  "minimumDataPoints: 48" \
  "docs/getting-started/quickstart.md"
check_default "minimumDataPoints (README)" \
  "minimumDataPoints: 48" \
  "README.md"

# --- networkPolicy.prometheusPort default = 9090 ---
check_default "prometheusPort (values.yaml)" \
  "prometheusPort: 9090" \
  "charts/kube-rightsize/values.yaml"
check_default "prometheusPort (README)" \
  'prometheusPort.*9090' \
  "README.md"

if [ $rc -ne 0 ]; then
  echo ""
  echo "ERROR: Documentation defaults are inconsistent. Fix the files above."
  exit 1
fi

echo "OK: All documented defaults are consistent."