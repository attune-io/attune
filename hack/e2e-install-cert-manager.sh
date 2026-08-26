#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Apply cert-manager after k3s may still be bouncing (kubelet restart
# after k3d image import). Poll /readyz, retry kubectl apply, then wait
# for the three cert-manager Deployments.
#
# Used by e2e-nightly.yaml and setup-e2e-cluster so the two paths cannot
# drift. Override KUBECTL / READYZ_* / APPLY_* in unit tests.

set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
READYZ_ATTEMPTS="${READYZ_ATTEMPTS:-60}"
READYZ_SLEEP="${READYZ_SLEEP:-2}"
APPLY_ATTEMPTS="${APPLY_ATTEMPTS:-3}"
APPLY_RETRY_SLEEP="${APPLY_RETRY_SLEEP:-15}"
MODE="apply"
MANIFEST=""

if [[ "${1:-}" == "--wait-only" ]]; then
  MODE="wait"
elif [[ -n "${1:-}" ]]; then
  MANIFEST="$1"
else
  echo "usage: e2e-install-cert-manager.sh MANIFEST | --wait-only" >&2
  exit 2
fi

wait_readyz() {
  local attempt
  for attempt in $(seq 1 "${READYZ_ATTEMPTS}"); do
    if "${KUBECTL}" get --raw='/readyz' >/dev/null 2>&1; then
      echo "API server ready after ${attempt} attempt(s)"
      return 0
    fi
    if (( attempt == READYZ_ATTEMPTS )); then
      echo "::error::API server never became ready (readyz)"
      return 1
    fi
    sleep "${READYZ_SLEEP}"
  done
}

wait_readyz
if [[ "${MODE}" == "wait" ]]; then
  exit 0
fi

attempt=1
while (( attempt <= APPLY_ATTEMPTS )); do
  if "${KUBECTL}" apply -f "${MANIFEST}"; then
    break
  fi
  if (( attempt == APPLY_ATTEMPTS )); then
    echo "::error::cert-manager install failed after ${APPLY_ATTEMPTS} attempts"
    exit 1
  fi
  echo "::warning::cert-manager install attempt ${attempt} failed, retrying in ${APPLY_RETRY_SLEEP}s..."
  sleep "${APPLY_RETRY_SLEEP}"
  wait_readyz
  attempt=$((attempt + 1))
done

"${KUBECTL}" wait --for=condition=Available deployment/cert-manager -n cert-manager --timeout=120s
"${KUBECTL}" wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
"${KUBECTL}" wait --for=condition=Available deployment/cert-manager-cainjector -n cert-manager --timeout=120s
