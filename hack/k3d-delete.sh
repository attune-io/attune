#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Bounded k3d cluster delete. A hung docker/k3d must not consume the
# remaining E2E job timeout (seen after setup-e2e-cluster failed on
# main: k3d cluster delete ran until the 30m job cap). Always exits 0.
#
# Usage: k3d-delete.sh CLUSTER_NAME [KUBECONFIG]
# Overrides for tests: K3D, TIMEOUT_BIN, K3D_DELETE_TIMEOUT_SEC.

set -euo pipefail

if [[ $# -lt 1 || -z "${1}" ]]; then
  echo "usage: k3d-delete.sh CLUSTER_NAME [KUBECONFIG]" >&2
  exit 2
fi

NAME="$1"
KUBECONFIG_PATH="${2:-}"
TIMEOUT_SEC="${K3D_DELETE_TIMEOUT_SEC:-60}"
K3D="${K3D:-k3d}"
TIMEOUT_BIN="${TIMEOUT_BIN:-timeout}"

echo "PLAN: delete k3d cluster ${NAME} (timeout ${TIMEOUT_SEC}s)"

if command -v "${TIMEOUT_BIN}" >/dev/null 2>&1; then
  "${TIMEOUT_BIN}" "${TIMEOUT_SEC}" "${K3D}" cluster delete "${NAME}" || true
else
  echo "::warning::timeout not found; deleting ${NAME} without a bound"
  "${K3D}" cluster delete "${NAME}" || true
fi

if [[ -n "${KUBECONFIG_PATH}" ]]; then
  rm -f "${KUBECONFIG_PATH}"
fi

echo "DONE: ok=true cluster=${NAME}"
echo "NEXT: none"
