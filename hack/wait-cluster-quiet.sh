#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Wait until Chainsaw namespace deletion has finished and the API server
# answers. Go E2E starts t.Parallel immediately. Leftover terminating
# pods plus a wedged apiserver fill the one k3d node (nightly #921).
#
# Overrides for tests: KUBECTL, CLUSTER_QUIET_DEADLINE_SEC,
# CLUSTER_QUIET_INTERVAL_SEC.

set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
DEADLINE_SEC="${CLUSTER_QUIET_DEADLINE_SEC:-180}"
INTERVAL_SEC="${CLUSTER_QUIET_INTERVAL_SEC:-5}"

# Platform namespaces that stay for the Go suite.
KEEP_NS='^(kube-system|kube-public|kube-node-lease|cert-manager|attune-system|monitoring)$'

echo "PLAN: wait up to ${DEADLINE_SEC}s for non-system pods to leave and /readyz to answer"

deadline=$((SECONDS + DEADLINE_SEC))
last_busy="api not checked"

while (( SECONDS < deadline )); do
  ready=0
  if "${KUBECTL}" get --raw='/readyz' >/dev/null 2>&1; then
    ready=1
  fi

  # Terminating pods still report Running until they disappear.
  # Succeeded and Failed do not hold CPU requests.
  busy=""
  if pod_json="$("${KUBECTL}" get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json 2>/dev/null)"; then
    busy="$(printf '%s' "${pod_json}" | jq -r --arg re "${KEEP_NS}" '
      .items[]
      | select((.metadata.namespace | test($re)) | not)
      | "\(.metadata.namespace)/\(.metadata.name) phase=\(.status.phase) deleting=\(.metadata.deletionTimestamp // "no")"
    ')"
  else
    busy="kubectl get pods failed"
    ready=0
  fi

  if [[ "${ready}" -eq 1 && -z "${busy}" ]]; then
    echo "OK: cluster quiet, /readyz answered"
    echo "DONE: ok=true"
    echo "NEXT: go e2e"
    exit 0
  fi

  if [[ -n "${busy}" ]]; then
    last_busy="${busy}"
  else
    last_busy="/readyz not ready"
  fi
  echo "WAIT: ${last_busy}"
  sleep "${INTERVAL_SEC}"
done

echo "FAIL: cluster still busy after ${DEADLINE_SEC}s"
echo "${last_busy}"
echo "DONE: ok=false"
exit 1
