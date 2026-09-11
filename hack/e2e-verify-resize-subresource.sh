#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Confirm the pods/resize subresource is registered. On k3s v1.32 the
# embedded apiserver can miss InPlacePodVerticalScaling at start
# (k3s-io/k3s#14286, attune #352). Waiting longer does not help: the
# gate is applied at process start. Poll briefly, then recreate.
#
# Env:
#   CLUSTER_NAME, KUBECONFIG, K3S_IMAGE (required for recreate)
#   KUBECTL, K3D, DOCKER, K3D_DELETE (overrides for tests)
#   RESIZE_VERIFY_ATTEMPTS (default 6), RESIZE_VERIFY_SLEEP (default 5)
#   RESIZE_RECREATE_ATTEMPTS (default 3)
#   SKIP_RECREATE=1 (verify only)

set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
K3D="${K3D:-k3d}"
DOCKER="${DOCKER:-docker}"
CLUSTER_NAME="${CLUSTER_NAME:-}"
KUBECONFIG="${KUBECONFIG:-}"
K3S_IMAGE="${K3S_IMAGE:-}"
VERIFY_ATTEMPTS="${RESIZE_VERIFY_ATTEMPTS:-6}"
VERIFY_SLEEP="${RESIZE_VERIFY_SLEEP:-5}"
RECREATE_ATTEMPTS="${RESIZE_RECREATE_ATTEMPTS:-3}"
READYZ_ATTEMPTS="${RESIZE_READYZ_ATTEMPTS:-60}"
READYZ_SLEEP="${RESIZE_READYZ_SLEEP:-2}"
SKIP_RECREATE="${SKIP_RECREATE:-0}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K3D_DELETE="${K3D_DELETE:-${SCRIPT_DIR}/k3d-delete.sh}"

echo "PLAN: verify pods/resize (attempts=${VERIFY_ATTEMPTS} recreate=${RECREATE_ATTEMPTS})"

if [[ -z "${CLUSTER_NAME}" ]]; then
  echo "FAIL: CLUSTER_NAME is required"
  echo "DONE: ok=false"
  exit 1
fi

verify_resize() {
  local attempt
  for attempt in $(seq 1 "${VERIFY_ATTEMPTS}"); do
    if "${KUBECTL}" get --raw /api/v1 2>/dev/null \
      | jq -e '.resources[] | select(.name == "pods/resize")' >/dev/null 2>&1; then
      echo "OK: pods/resize registered (attempt ${attempt})"
      return 0
    fi
    echo "WAIT: pods/resize missing (attempt ${attempt}/${VERIFY_ATTEMPTS})"
    if (( attempt < VERIFY_ATTEMPTS )); then
      sleep "${VERIFY_SLEEP}"
    fi
  done
  return 1
}

write_k3s_config() {
  local path="$1"
  cat >"${path}" <<'K3SEOF'
kube-apiserver-arg:
  - "feature-gates=InPlacePodVerticalScaling=true"
kube-controller-manager-arg:
  - "feature-gates=InPlacePodVerticalScaling=true"
kube-scheduler-arg:
  - "feature-gates=InPlacePodVerticalScaling=true"
kubelet-arg:
  - "feature-gates=InPlacePodVerticalScaling=true"
K3SEOF
}

recreate_cluster() {
  if [[ -z "${K3S_IMAGE}" ]]; then
    echo "FAIL: K3S_IMAGE is required to recreate"
    return 1
  fi
  echo "DO: recreate k3d cluster ${CLUSTER_NAME}"
  bash "${K3D_DELETE}" "${CLUSTER_NAME}"
  local cfg
  cfg="$(mktemp)"
  write_k3s_config "${cfg}"
  if ! "${K3D}" cluster create "${CLUSTER_NAME}" \
    --image "${K3S_IMAGE}" \
    --k3s-arg "--disable=traefik,servicelb@server:*" \
    --volume "${cfg}:/etc/rancher/k3s/config.yaml@server:*" \
    --wait --timeout 120s \
    --kubeconfig-update-default=false \
    --kubeconfig-switch-context=false; then
    rm -f "${cfg}"
    echo "FAIL: k3d cluster create failed"
    return 1
  fi
  rm -f "${cfg}"
  if [[ -n "${KUBECONFIG}" ]]; then
    "${K3D}" kubeconfig merge "${CLUSTER_NAME}" --output "${KUBECONFIG}" --overwrite
  fi
  local attempt
  for attempt in $(seq 1 "${READYZ_ATTEMPTS}"); do
    if "${KUBECTL}" get --raw /readyz >/dev/null 2>&1; then
      break
    fi
    if (( attempt == READYZ_ATTEMPTS )); then
      echo "FAIL: API server never became ready after recreate"
      return 1
    fi
    sleep "${READYZ_SLEEP}"
  done
  "${KUBECTL}" wait --for=condition=Ready nodes --all --timeout=360s
}

dump_diagnostics() {
  echo "FAIL: pods/resize not registered after recreate"
  echo "Diagnostic: k3s config file:"
  "${DOCKER}" exec "k3d-${CLUSTER_NAME}-server-0" cat /etc/rancher/k3s/config.yaml || true
  echo "Diagnostic: k3s process:"
  "${DOCKER}" exec "k3d-${CLUSTER_NAME}-server-0" sh -c "ps aux | grep k3s" || true
  echo "Diagnostic: API resources containing resize:"
  "${KUBECTL}" get --raw /api/v1 2>/dev/null \
    | jq '[.resources[] | select(.name | contains("resize"))]' || true
}

echo "DO: k3s config inside container"
"${DOCKER}" exec "k3d-${CLUSTER_NAME}-server-0" cat /etc/rancher/k3s/config.yaml || true

if verify_resize; then
  echo "DONE: ok=true recreated=0"
  echo "NEXT: none"
  exit 0
fi

if [[ "${SKIP_RECREATE}" == "1" ]]; then
  echo "FAIL: pods/resize missing and SKIP_RECREATE=1"
  echo "DONE: ok=false"
  exit 1
fi

recreated=0
while (( recreated < RECREATE_ATTEMPTS )); do
  recreated=$((recreated + 1))
  echo "WAIT: known k3s feature-gate race; recreate ${recreated}/${RECREATE_ATTEMPTS}"
  if ! recreate_cluster; then
    echo "DONE: ok=false"
    exit 1
  fi
  echo "DO: k3s config inside recreated container"
  "${DOCKER}" exec "k3d-${CLUSTER_NAME}-server-0" cat /etc/rancher/k3s/config.yaml || true
  if verify_resize; then
    echo "DONE: ok=true recreated=${recreated}"
    echo "NEXT: none"
    exit 0
  fi
done

dump_diagnostics
echo "DONE: ok=false recreated=${recreated}"
echo "NEXT: inspect k3s logs"
exit 1
