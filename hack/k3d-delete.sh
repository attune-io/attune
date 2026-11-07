#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Bounded k3d cluster delete. A hung docker/k3d must not consume the
# remaining E2E job (seen after setup-e2e-cluster failed on main:
# k3d cluster delete ran until the 30m job cap). Always exits 0.
#
# hack/bounded-run.sh returns when the child ignores SIGKILL. GNU
# timeout does not. This script does not call timeout, and it does
# not delete without a bound when timeout is missing.
#
# Usage: k3d-delete.sh CLUSTER_NAME [KUBECONFIG]
# Overrides for tests: K3D, K3D_DELETE_TIMEOUT_SEC, K3D_DELETE_KILL_AFTER_SEC.

set -euo pipefail

if [[ $# -lt 1 || -z "${1}" ]]; then
  echo "usage: k3d-delete.sh CLUSTER_NAME [KUBECONFIG]" >&2
  exit 2
fi

NAME="$1"
KUBECONFIG_PATH="${2:-}"
TIMEOUT_SEC="${K3D_DELETE_TIMEOUT_SEC:-60}"
K3D="${K3D:-k3d}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "PLAN: delete k3d cluster ${NAME} (timeout ${TIMEOUT_SEC}s)"

KILL_AFTER="${K3D_DELETE_KILL_AFTER_SEC:-5}"
del_status=0
BOUNDED_KILL_AFTER_SEC="${KILL_AFTER}" \
  bash "${SCRIPT_DIR}/bounded-run.sh" "${TIMEOUT_SEC}" -- \
  "${K3D}" cluster delete "${NAME}" || del_status=$?
# 124 is our bound. 137 is a child that died on SIGKILL before we returned.
if [[ "${del_status}" -eq 124 || "${del_status}" -eq 137 ]]; then
  echo "::warning::k3d delete timed out for ${NAME}; cluster may still exist"
fi

if [[ -n "${KUBECONFIG_PATH}" ]]; then
  rm -f "${KUBECONFIG_PATH}"
fi

echo "DONE: ok=true cluster=${NAME}"
echo "NEXT: none"
