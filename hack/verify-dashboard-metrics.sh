#!/usr/bin/env bash
# Copyright 2026 attune-io
# SPDX-License-Identifier: Apache-2.0
#
# The standalone dashboard JSON is the source of truth. The Helm chart uses a
# generated derivative with datasource-specific fields removed so Grafana
# sidecar provisioning can consume the same dashboard structure.
set -euo pipefail

MODE="${1:-check}"
STANDALONE="deploy/grafana/dashboard.json"
HELM_TEMPLATE="charts/attune/templates/grafana-dashboard.yaml"
HELM_DASHBOARD="charts/attune/files/grafana-dashboard.json"
EXPECTED="$(mktemp)"
trap 'rm -f "$EXPECTED"' EXIT

if [[ "$MODE" != "check" && "$MODE" != "--write" ]]; then
  echo "Usage: bash hack/verify-dashboard-metrics.sh [--write]" >&2
  exit 2
fi

python3 - "$STANDALONE" "$EXPECTED" <<'PY'
import json
import sys
from pathlib import Path

source = Path(sys.argv[1])
target = Path(sys.argv[2])
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
dashboard["uid"] = "attune"
target.write_text(json.dumps(dashboard, indent=2) + "\n")
PY

if ! grep -Fq '.Files.Get "files/grafana-dashboard.json"' "$HELM_TEMPLATE"; then
  echo "ERROR: $HELM_TEMPLATE must load files/grafana-dashboard.json as the chart dashboard source." >&2
  exit 1
fi

if [[ "$MODE" == "--write" ]]; then
  cp "$EXPECTED" "$HELM_DASHBOARD"
  echo "Wrote $HELM_DASHBOARD from $STANDALONE."
  exit 0
fi

if ! diff -u "$HELM_DASHBOARD" "$EXPECTED"; then
  echo ""
  echo "ERROR: Helm dashboard JSON is stale." >&2
  echo "Refresh it with: bash hack/verify-dashboard-metrics.sh --write" >&2
  exit 1
fi

panel_count=$(python3 - "$STANDALONE" <<'PY'
import json
import sys
from pathlib import Path

dashboard = json.loads(Path(sys.argv[1]).read_text())
print(len(dashboard.get("panels", [])))
PY
)

echo "OK: Helm dashboard JSON matches $STANDALONE ($panel_count panels)."

# Verify that all attune_* metric names in the dashboard exist in the
# operator's metrics registry. This catches typos and stale metric names
# that would cause Grafana panels to show "No data".
METRICS_SRC="internal/operatormetrics/metrics.go"
metrics_in_dashboard=$(grep -oE 'attune_[a-z_]+' "$STANDALONE" | sort -u)
metrics_in_source=$(grep -oE '"attune_[a-z_]+"' "$METRICS_SRC" | tr -d '"' | sort -u)

rc=0
for m in $metrics_in_dashboard; do
  # Strip histogram suffixes (_count, _sum, _bucket) and _total for
  # counter base-name matching.
  base="${m%_count}"
  base="${base%_sum}"
  base="${base%_bucket}"
  if ! echo "$metrics_in_source" | grep -qF "$base"; then
    echo "ERROR: metric '$m' (base '$base') in $STANDALONE not found in $METRICS_SRC" >&2
    rc=1
  fi
done

if [[ $rc -ne 0 ]]; then
  exit 1
fi
echo "OK: all dashboard metrics match registered operator metrics."
