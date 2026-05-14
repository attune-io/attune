#!/usr/bin/env bash
# Copyright 2026 SebTardifLabs
# SPDX-License-Identifier: Apache-2.0
#
# Verify that the standalone Grafana dashboard and the Helm chart dashboard
# cover the same set of kube_rightsize_* metrics. Prevents silent drift
# between the two copies.
set -euo pipefail

STANDALONE="deploy/grafana/dashboard.json"
HELM_TEMPLATE="charts/kube-rightsize/templates/grafana-dashboard.yaml"

standalone_metrics=$(grep -oP 'kube_rightsize_\w+' "$STANDALONE" | sort -u)
helm_metrics=$(grep -oP 'kube_rightsize_\w+' "$HELM_TEMPLATE" | sort -u)

diff_output=$(diff <(echo "$standalone_metrics") <(echo "$helm_metrics") || true)

if [ -n "$diff_output" ]; then
  echo "ERROR: Dashboard metric coverage has drifted."
  echo ""
  echo "Metrics only in standalone ($STANDALONE):"
  diff <(echo "$standalone_metrics") <(echo "$helm_metrics") | grep '^< ' | sed 's/^< /  /' || true
  echo ""
  echo "Metrics only in Helm chart ($HELM_TEMPLATE):"
  diff <(echo "$standalone_metrics") <(echo "$helm_metrics") | grep '^> ' | sed 's/^> /  /' || true
  echo ""
  echo "Both dashboards must cover the same kube_rightsize_* metrics."
  exit 1
fi

echo "OK: Both dashboards cover the same $(echo "$standalone_metrics" | wc -l) metrics."
