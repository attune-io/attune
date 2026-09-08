#!/usr/bin/env bash
# Copyright 2026 attune-io
# SPDX-License-Identifier: Apache-2.0
#
# Standalone Grafana JSON is the source of truth. The Helm chart uses
# generated derivatives with datasource-specific fields removed so Grafana
# sidecar provisioning can consume the same dashboard structure.
#
# Pairs:
#   deploy/grafana/dashboard.json       -> charts/attune/files/grafana-dashboard.json
#   deploy/grafana/fleet-dashboard.json -> charts/attune/files/grafana-fleet-dashboard.json
set -euo pipefail

MODE="${1:-check}"

if [[ "$MODE" != "check" && "$MODE" != "--write" ]]; then
  echo "Usage: bash hack/verify-dashboard-metrics.sh [--write]" >&2
  exit 2
fi

EXPECTED_DIR="$(mktemp -d)"
trap 'rm -rf "$EXPECTED_DIR"' EXIT

# Transform standalone JSON into the Helm sidecar form (strip __inputs and
# datasource, pin the dashboard uid) and write the result to $3.
transform_dashboard() {
  local standalone="$1"
  local uid="$2"
  local target="$3"
  python3 - "$standalone" "$uid" "$target" <<'PY'
import json
import sys
from pathlib import Path

source = Path(sys.argv[1])
uid = sys.argv[2]
target = Path(sys.argv[3])
dashboard = json.loads(source.read_text())


def transform(value):
    if isinstance(value, dict):
        result = {}
        for key, item in value.items():
            if key in {"__inputs", "datasource"}:
                continue
            result[key] = transform(item)
        return result
    if isinstance(value, list):
        return [transform(item) for item in value]
    return value


dashboard = transform(dashboard)
dashboard["uid"] = uid
target.write_text(json.dumps(dashboard, indent=2) + "\n")
PY
}

# Verify one standalone -> Helm pair. On --write, refresh the Helm file.
verify_dashboard() {
  local standalone="$1"
  local helm_template="$2"
  local helm_dashboard="$3"
  local uid="$4"
  local files_get="$5"
  local expected="${EXPECTED_DIR}/${uid}.json"

  transform_dashboard "$standalone" "$uid" "$expected"

  if ! grep -Fq ".Files.Get \"${files_get}\"" "$helm_template"; then
    echo "ERROR: $helm_template must load ${files_get} as the chart dashboard source." >&2
    exit 1
  fi

  if [[ "$MODE" == "--write" ]]; then
    cp "$expected" "$helm_dashboard"
    echo "Wrote $helm_dashboard from $standalone."
    return 0
  fi

  if ! diff -u "$helm_dashboard" "$expected"; then
    echo ""
    echo "ERROR: Helm dashboard JSON is stale: $helm_dashboard" >&2
    echo "Refresh it with: bash hack/verify-dashboard-metrics.sh --write" >&2
    exit 1
  fi

  local panel_count
  panel_count=$(python3 - "$standalone" <<'PY'
import json
import sys
from pathlib import Path

dashboard = json.loads(Path(sys.argv[1]).read_text())
print(len(dashboard.get("panels", [])))
PY
)

  echo "OK: Helm dashboard JSON matches $standalone ($panel_count panels)."
}

# Verify that all attune_* metric names in a dashboard exist in the
# operator's metrics registry. This catches typos and stale metric names
# that would cause Grafana panels to show "No data".
check_metrics() {
  local standalone="$1"
  local metrics_src="$2"
  local metrics_in_dashboard
  local metrics_in_source
  local rc=0
  local m base

  metrics_in_dashboard=$(grep -oE 'attune_[a-z_]+' "$standalone" | sort -u)
  metrics_in_source=$(grep -oE '"attune_[a-z_]+"' "$metrics_src" | tr -d '"' | sort -u)

  for m in $metrics_in_dashboard; do
    # Strip histogram suffixes (_count, _sum, _bucket) for base-name matching.
    base="${m%_count}"
    base="${base%_sum}"
    base="${base%_bucket}"
    if ! echo "$metrics_in_source" | grep -qF "$base"; then
      echo "ERROR: metric '$m' (base '$base') in $standalone not found in $metrics_src" >&2
      rc=1
    fi
  done

  if [[ $rc -ne 0 ]]; then
    exit 1
  fi
  echo "OK: all metrics in $standalone match registered operator metrics."
}

verify_dashboard \
  "deploy/grafana/dashboard.json" \
  "charts/attune/templates/grafana-dashboard.yaml" \
  "charts/attune/files/grafana-dashboard.json" \
  "attune" \
  "files/grafana-dashboard.json"

verify_dashboard \
  "deploy/grafana/fleet-dashboard.json" \
  "charts/attune/templates/grafana-fleet-dashboard.yaml" \
  "charts/attune/files/grafana-fleet-dashboard.json" \
  "attune-fleet" \
  "files/grafana-fleet-dashboard.json"

if [[ "$MODE" == "--write" ]]; then
  exit 0
fi

METRICS_SRC="internal/operatormetrics/metrics.go"
check_metrics "deploy/grafana/dashboard.json" "$METRICS_SRC"
check_metrics "deploy/grafana/fleet-dashboard.json" "$METRICS_SRC"
echo "OK: all dashboard metrics match registered operator metrics."
