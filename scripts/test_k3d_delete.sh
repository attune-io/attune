#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/k3d-delete.sh: cleanup must bound k3d cluster
# delete so a hung docker/k3d cannot eat the E2E job timeout.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/k3d-delete.sh"
CI="${ROOT}/.github/workflows/ci.yaml"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"

echo "PLAN: classify k3d-delete.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"
[[ -x "${SCRIPT}" ]] || fail "${SCRIPT} is not executable"

echo "DO: CI, nightly, and setup-e2e-cluster must call the helper"
grep -F -q 'hack/k3d-delete.sh' "${CI}" \
  || fail "ci.yaml does not invoke hack/k3d-delete.sh"
grep -F -q 'hack/k3d-delete.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/k3d-delete.sh"
grep -F -q 'hack/k3d-delete.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/k3d-delete.sh"
echo "OK: all three paths call the helper"

echo "DO: no unbounded k3d cluster delete remains in those paths"
if grep -nE 'k3d cluster delete' "${CI}" "${NIGHTLY}" "${COMPOSITE}" \
  | grep -v 'k3d-delete.sh' | grep -v '#'; then
  fail "bare k3d cluster delete still present; use hack/k3d-delete.sh"
fi
echo "OK: no bare k3d cluster delete in CI paths"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

cat >"${TMP}/k3d" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "k3d $*" >>"${K3D_LOG}"
if [[ "${K3D_HANG:-0}" == "1" ]]; then
  sleep 30
fi
if [[ "${K3D_FAIL:-0}" == "1" ]]; then
  echo "k3d failed" >&2
  exit 1
fi
exit 0
EOF
chmod +x "${TMP}/k3d"

cat >"${TMP}/timeout" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# usage: timeout SEC CMD...
secs="$1"
shift
"$@" &
pid=$!
(
  sleep "${secs}"
  kill "${pid}" 2>/dev/null || true
) &
waiter=$!
if wait "${pid}"; then
  kill "${waiter}" 2>/dev/null || true
  wait "${waiter}" 2>/dev/null || true
  exit 0
fi
status=$?
kill "${waiter}" 2>/dev/null || true
wait "${waiter}" 2>/dev/null || true
exit "${status}"
EOF
chmod +x "${TMP}/timeout"

export PATH="${TMP}:${PATH}"
export K3D="${TMP}/k3d"
export TIMEOUT_BIN="${TMP}/timeout"
export K3D_DELETE_TIMEOUT_SEC=1
export K3D_LOG="${TMP}/k3d.log"

echo "DO: successful delete exits 0 and invokes k3d"
: >"${K3D_LOG}"
K3D_HANG=0 K3D_FAIL=0 bash "${SCRIPT}" e2e-test
grep -Fq 'cluster delete e2e-test' "${K3D_LOG}" \
  || fail "successful delete did not call k3d cluster delete"
echo "OK: successful delete"

echo "DO: hung delete is bounded and still exits 0"
: >"${K3D_LOG}"
start=$(date +%s)
K3D_HANG=1 bash "${SCRIPT}" hung-cluster
elapsed=$(( $(date +%s) - start ))
if (( elapsed > 8 )); then
  fail "hung delete took ${elapsed}s; expected timeout around 1s"
fi
echo "OK: hung delete bounded (${elapsed}s)"

echo "DO: k3d failure still exits 0"
: >"${K3D_LOG}"
K3D_FAIL=1 bash "${SCRIPT}" fail-cluster
echo "OK: failed delete exits 0"

echo "DO: kubeconfig path is removed"
kc="${TMP}/kubeconfig"
echo leftover >"${kc}"
K3D_HANG=0 K3D_FAIL=0 bash "${SCRIPT}" e2e-test "${kc}"
[[ ! -e "${kc}" ]] || fail "kubeconfig ${kc} was not removed"
echo "OK: kubeconfig removed"

echo "DONE: ok=true"
echo "NEXT: none"
