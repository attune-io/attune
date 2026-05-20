#!/usr/bin/env bash
# Copyright 2026 SebTardifLabs
# SPDX-License-Identifier: Apache-2.0
#
# Verify that all metric names referenced in the PrometheusRule Helm template
# exist in the operator's metrics registry (internal/operatormetrics/metrics.go).
set -euo pipefail

TEMPLATE="charts/kube-rightsize/templates/prometheusrule.yaml"
METRICS_SRC="internal/operatormetrics/metrics.go"

if [[ ! -f "$TEMPLATE" ]]; then
  echo "SKIP: $TEMPLATE not found" >&2
  exit 0
fi

# Extract metric names from the template (kube_rightsize_*).
# grep for metric name patterns, strip Helm template syntax.
metrics_in_template=$(grep -oE 'kube_rightsize_[a-z_]+' "$TEMPLATE" | sort -u)

if [[ -z "$metrics_in_template" ]]; then
  echo "SKIP: no kube_rightsize_* metrics found in $TEMPLATE" >&2
  exit 0
fi

# Extract registered metric names from Go source.
metrics_in_source=$(grep -oE '"kube_rightsize_[a-z_]+"' "$METRICS_SRC" | tr -d '"' | sort -u)

rc=0
for m in $metrics_in_template; do
  # The template may reference _count or _sum suffixes of histograms.
  base="${m%_count}"
  base="${base%_sum}"
  base="${base%_bucket}"
  if ! echo "$metrics_in_source" | grep -qF "$base"; then
    echo "ERROR: metric '$m' (base '$base') in $TEMPLATE not found in $METRICS_SRC" >&2
    rc=1
  fi
done

if [[ $rc -eq 0 ]]; then
  echo "OK: all PrometheusRule metrics match registered operator metrics"
fi
exit $rc
