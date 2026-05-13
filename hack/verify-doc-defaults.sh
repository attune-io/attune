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

# --- CPU percentile default = 95, memory percentile default = 99 ---
check_default "cpuPercentile (Go)" \
  "DefaultCPUPercentile.*= 95" \
  "api/v1alpha1/defaults.go"
check_default "memPercentile (Go)" \
  "DefaultMemoryPercentile.*= 99" \
  "api/v1alpha1/defaults.go"
check_default "cpuPercentile (README)" \
  "percentile: 95" \
  "README.md"
check_default "memPercentile (README)" \
  "percentile: 99" \
  "README.md"

# --- CPU safetyMargin default = 1.2, memory safetyMargin default = 1.3 ---
check_default "cpuSafetyMargin (Go)" \
  'DefaultCPUSafetyMargin.*"1.2"' \
  "api/v1alpha1/defaults.go"
check_default "memSafetyMargin (Go)" \
  'DefaultMemorySafetyMargin.*"1.3"' \
  "api/v1alpha1/defaults.go"
check_default "cpuSafetyMargin (README)" \
  'safetyMargin.*1.2' \
  "README.md"
check_default "memSafetyMargin (README)" \
  'safetyMargin.*1.3' \
  "README.md"

# --- Resource bounds defaults ---
check_default "cpuBoundsMin (Go)" \
  'DefaultCPUBoundsMin.*"50m"' \
  "api/v1alpha1/defaults.go"
check_default "cpuBoundsMax (Go)" \
  'DefaultCPUBoundsMax.*"4000m"' \
  "api/v1alpha1/defaults.go"
check_default "memBoundsMin (Go)" \
  'DefaultMemoryBoundsMin.*"64Mi"' \
  "api/v1alpha1/defaults.go"
check_default "memBoundsMax (Go)" \
  'DefaultMemoryBoundsMax.*"8Gi"' \
  "api/v1alpha1/defaults.go"
check_default "cpuBounds (README)" \
  'min.*50m' \
  "README.md"
check_default "memBounds (README)" \
  'min.*64Mi' \
  "README.md"

# --- cooldown default = 1h ---
check_default "cooldown (Go)" \
  'DefaultCooldown.*"1h"' \
  "api/v1alpha1/defaults.go"
check_default "cooldown (README)" \
  '[Cc]ooldown' \
  "README.md"

# --- collectorTTL default = 10m ---
check_default "collectorTTL (Go)" \
  'collectorTTL = 10 \* time.Minute' \
  "internal/controller/rightsizepolicy_controller.go"
check_default "collectorTTL (Helm values)" \
  'collectorTTL: "10m"' \
  "charts/kube-rightsize/values.yaml"

# --- networkPolicy.prometheusPort default = 9090 ---
check_default "prometheusPort (values.yaml)" \
  "prometheusPort: 9090" \
  "charts/kube-rightsize/values.yaml"
check_default "prometheusPort (README)" \
  'prometheusPort.*9090' \
  "README.md"

# --- why-kube-rightsize.md: pricing, safety margins ---
check_default "cpuPricing (why page)" \
  '0\.031' \
  "docs/why-kube-rightsize.md"
check_default "memPricing (why page)" \
  '0\.004' \
  "docs/why-kube-rightsize.md"
check_default "cpuSafetyMargin (why page)" \
  'x 1.2' \
  "docs/why-kube-rightsize.md"
check_default "memSafetyMargin (why page)" \
  'x 1.3' \
  "docs/why-kube-rightsize.md"
check_default "cpuPercentile (why page)" \
  'P95' \
  "docs/why-kube-rightsize.md"

# --- savings-calculator.md: pricing input defaults ---
check_default "cpuPricing (calculator)" \
  'value="0.031"' \
  "docs/savings-calculator.md"
check_default "memPricing (calculator)" \
  'value="0.004"' \
  "docs/savings-calculator.md"

if [ $rc -ne 0 ]; then
  echo ""
  echo "ERROR: Documentation defaults are inconsistent. Fix the files above."
  exit 1
fi

echo "OK: All documented defaults are consistent."