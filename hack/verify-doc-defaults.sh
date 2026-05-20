#!/usr/bin/env bash
# Copyright 2026 kube-rightsize Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that critical user-facing defaults are consistent across docs,
# CRDs, Go code, and chart values. Exits non-zero on the first mismatch.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rc=0

resolve_path() {
  local path="$1"
  if [[ "$path" = /* ]]; then
    printf '%s\n' "$path"
    return
  fi
  printf '%s/%s\n' "$REPO_ROOT" "$path"
}

check_default() {
  local label="$1" pattern="$2"
  shift 2
  local files=("$@")

  for f in "${files[@]}"; do
    local path
    path=$(resolve_path "$f")
    if [ ! -f "$path" ]; then
      echo "WARN: $f does not exist, skipping"
      continue
    fi
    if ! grep -q "$pattern" "$path"; then
      echo "FAIL: $label: pattern '$pattern' not found in $f"
      rc=1
    fi
  done
}

check_absent() {
  local label="$1" pattern="$2"
  shift 2
  local files=("$@")

  for f in "${files[@]}"; do
    local path
    path=$(resolve_path "$f")
    if [ ! -f "$path" ]; then
      echo "WARN: $f does not exist, skipping"
      continue
    fi
    if grep -q "$pattern" "$path"; then
      echo "FAIL: $label: unexpected pattern '$pattern' found in $f"
      rc=1
    fi
  done
}

# --- minimumDataPoints default = 48 ---
# Go code (canonical source of truth)
check_default "minimumDataPoints (Go)" \
  "defaultMinimumDataPoints.*= 48" \
  "internal/controller/rightsizepolicy_controller.go"

# Go defaults.go (canonical source; CRD no longer has +kubebuilder:default
# because defaulting moved to controller for RightSizeDefaults compatibility)
check_default "minimumDataPoints (defaults.go)" \
  "DefaultMinimumDataPoints.*= 48" \
  "api/v1alpha1/defaults.go"

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

minimum_data_points_crd_files=(
  "config/crd/bases/rightsize.io_rightsizepolicies.yaml"
  "config/crd/bases/rightsize.io_rightsizedefaults.yaml"
  "config/crd/bases/rightsize.io_rightsizenamespacedefaults.yaml"
  "charts/kube-rightsize/crds/rightsize.io_rightsizepolicies.yaml"
  "charts/kube-rightsize/crds/rightsize.io_rightsizedefaults.yaml"
  "charts/kube-rightsize/crds/rightsize.io_rightsizenamespacedefaults.yaml"
  "$@"
)

check_default "minimumDataPoints timing (Go godoc)" \
  "48 samples" \
  "api/v1alpha1/rightsizepolicy_types.go"
check_default "minimumDataPoints timing (Go godoc)" \
  "4 hours of data" \
  "api/v1alpha1/rightsizepolicy_types.go"
check_default "minimumDataPoints timing (README)" \
  "4 hours of data" \
  "README.md"
check_default "minimumDataPoints timing (Prometheus guide)" \
  "4 hours of data" \
  "docs/guides/prometheus-setup.md"
check_default "minimumDataPoints timing (CRDs)" \
  "48 samples" \
  "${minimum_data_points_crd_files[@]}"
check_default "minimumDataPoints timing (CRDs)" \
  "4 hours of data" \
  "${minimum_data_points_crd_files[@]}"
check_absent "minimumDataPoints stale timing (Go godoc)" \
  "2 days" \
  "api/v1alpha1/rightsizepolicy_types.go"
check_absent "minimumDataPoints stale timing (README)" \
  "2 days" \
  "README.md"
check_absent "minimumDataPoints stale timing (Prometheus guide)" \
  "2 days" \
  "docs/guides/prometheus-setup.md"
check_absent "minimumDataPoints stale timing (CRDs)" \
  "2 days" \
  "${minimum_data_points_crd_files[@]}"

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

# --- queryStep default = 5m ---
check_default "queryStep (Go)" \
  "defaultPrometheusStep = 5 \* time.Minute" \
  "internal/controller/rightsizepolicy_controller.go"
check_default "queryStep (API ref)" \
  "queryStep: 5m" \
  "docs/reference/api.md"

# --- collectorTTL default = 10m ---
check_default "collectorTTL (Go)" \
  'collectorTTL = 10 \* time.Minute' \
  "internal/controller/prometheus.go"
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

# --- prometheusTimeout: Go code, values.yaml, configuration.md ---
check_default "prometheusTimeout (Go default)" \
  'promTimeout = 5 \* time.Minute' \
  "internal/controller/rightsizepolicy_controller.go"
check_default "prometheusTimeout (values.yaml)" \
  'prometheusTimeout: "5m"' \
  "charts/kube-rightsize/values.yaml"
check_default "prometheusTimeout (configuration.md)" \
  '"5m"' \
  "docs/reference/configuration.md"

# --- Ready condition docs consistency ---
ready_reason_reference_files=(
  "docs/reference/api.md"
  "docs/SPEC.md"
  "docs/reference/cli.md"
)

for f in "${ready_reason_reference_files[@]}"; do
  check_default "Ready reasons ($f)" "Monitoring" "$f"
  check_default "Ready reasons ($f)" "InsufficientData" "$f"
  check_default "Ready reasons ($f)" "PrometheusUnavailable" "$f"
  check_default "Ready reasons ($f)" "InvalidConfig" "$f"
  check_default "Ready reasons ($f)" "WorkloadDiscoveryFailed" "$f"
done

check_default "Ready troubleshooting section" "^### PrometheusUnavailable" "docs/guides/troubleshooting.md"
check_default "Ready troubleshooting section" "^### InsufficientData" "docs/guides/troubleshooting.md"
check_default "Ready troubleshooting section" "^### InvalidConfig" "docs/guides/troubleshooting.md"
check_default "Ready troubleshooting section" "^### WorkloadDiscoveryFailed" "docs/guides/troubleshooting.md"
check_default "Prometheus setup condition table" "Ready: False, Reason: InsufficientData" "docs/guides/prometheus-setup.md"
check_default "Prometheus setup condition meaning" "Prometheus could not be used for this reconcile" "docs/guides/prometheus-setup.md"
check_absent "Prometheus setup stale condition meaning" "No Prometheus address found" "docs/guides/prometheus-setup.md"

# --- controller-applied defaults explanation ---
check_default "controller-applied defaults (README)" \
  "controller at reconcile time" \
  "README.md"
check_default "effective values guidance (README)" \
  "kubectl rightsize explain <policy>" \
  "README.md"
check_default "controller-applied defaults (quickstart)" \
  "controller at reconcile time" \
  "docs/getting-started/quickstart.md"
check_default "effective values guidance (quickstart)" \
  "kubectl rightsize explain <policy>" \
  "docs/getting-started/quickstart.md"

if [ $rc -ne 0 ]; then
  echo ""
  echo "ERROR: Documentation defaults are inconsistent. Fix the files above."
  exit 1
fi

echo "OK: All documented defaults are consistent."