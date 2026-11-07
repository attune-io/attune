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

echo "DO: CI, nightly, and setup-e2e-cluster must invoke the helper"
ci_calls=$(grep -cE 'bash hack/k3d-delete\.sh' "${CI}" || true)
nightly_calls=$(grep -cE 'bash hack/k3d-delete\.sh' "${NIGHTLY}" || true)
composite_calls=$(grep -cE 'bash hack/k3d-delete\.sh' "${COMPOSITE}" || true)
(( ci_calls >= 1 )) || fail "ci.yaml run-line calls=${ci_calls} want >=1"
(( nightly_calls >= 3 )) || fail "e2e-nightly.yaml run-line calls=${nightly_calls} want >=3"
(( composite_calls >= 2 )) || fail "setup-e2e-cluster run-line calls=${composite_calls} want >=2"
echo "OK: invoke counts ci=${ci_calls} nightly=${nightly_calls} composite=${composite_calls}"

echo "DO: no unbounded k3d cluster delete remains in those paths"
if grep -nE 'k3d[[:space:]]+cluster[[:space:]]+delete' "${CI}" "${NIGHTLY}" "${COMPOSITE}"; then
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
  trap '' TERM
  sleep 30
fi
if [[ "${K3D_FAIL:-0}" == "1" ]]; then
  echo "k3d failed" >&2
  exit 1
fi
exit 0
EOF
chmod +x "${TMP}/k3d"

export PATH="${TMP}:${PATH}"
export K3D="${TMP}/k3d"
export K3D_DELETE_TIMEOUT_SEC=1
export K3D_LOG="${TMP}/k3d.log"

echo "DO: successful delete exits 0 and invokes k3d"
: >"${K3D_LOG}"
K3D_HANG=0 K3D_FAIL=0 bash "${SCRIPT}" e2e-test
grep -Fq 'cluster delete e2e-test' "${K3D_LOG}" \
  || fail "successful delete did not call k3d cluster delete"
echo "OK: successful delete"

echo "DO: hung delete ignores TERM and is still bounded by SIGKILL"
: >"${K3D_LOG}"
export K3D_DELETE_KILL_AFTER_SEC=1
start=$(date +%s)
K3D_HANG=1 bash "${SCRIPT}" hung-cluster >"${TMP}/hung.out" 2>&1 || true
elapsed=$(( $(date +%s) - start ))
if (( elapsed > 8 )); then
  fail "hung delete took ${elapsed}s; expected TERM at 1s then KILL at +1s"
fi
echo "OK: hung delete bounded (${elapsed}s)"
unset K3D_DELETE_KILL_AFTER_SEC

echo "DO: k3d failure still exits 0"
: >"${K3D_LOG}"
K3D_FAIL=1 bash "${SCRIPT}" fail-cluster
echo "OK: failed delete exits 0"

echo "DO: empty name exits 2 and does not call k3d"
: >"${K3D_LOG}"
if bash "${SCRIPT}" ""; then
  fail "empty name should exit 2"
fi
[[ ! -s "${K3D_LOG}" ]] || fail "empty name still invoked k3d"
echo "OK: empty name rejected"

echo "DO: delete does not shell out to timeout or delete without a bound"
grep -F -q 'bounded-run.sh' "${SCRIPT}" || fail "delete does not use bounded-run.sh"
if grep -n 'TIMEOUT_BIN' "${SCRIPT}"; then
  fail "delete still depends on TIMEOUT_BIN"
fi
echo "OK: delete is always bounded"

echo "DO: a child that ignores TERM and KILL still returns inside the bound"
# shellcheck disable=SC2317
kill() {
  printf '%s\n' "$*" >>"${TMP}/kill.log"
  return 0
}
# shellcheck source=../hack/bounded-run.sh
source "${ROOT}/hack/bounded-run.sh"
: >"${TMP}/kill.log"
export BOUNDED_KILL_AFTER_SEC=1
export BOUNDED_ABANDON_SEC=1
start=$(date +%s)
set +e
bounded_run 1 -- sleep 30
ignore_status=$?
set -e
elapsed=$(( $(date +%s) - start ))
builtin kill -KILL "$(jobs -p)" 2>/dev/null || true
wait 2>/dev/null || true
[[ "${ignore_status}" -eq 124 ]] || fail "ignore-KILL status ${ignore_status}, want 124"
if (( elapsed > 8 )); then
  fail "ignore-KILL took ${elapsed}s; the helper waited for the child"
fi
grep -F -q -- '-KILL' "${TMP}/kill.log" || fail "ignore-KILL path did not send KILL"
echo "OK: ignore-KILL returned 124 in ${elapsed}s"
unset BOUNDED_KILL_AFTER_SEC BOUNDED_ABANDON_SEC

echo "DO: kubeconfig path is removed"
kc="${TMP}/kubeconfig"
echo leftover >"${kc}"
K3D_HANG=0 K3D_FAIL=0 bash "${SCRIPT}" e2e-test "${kc}"
[[ ! -e "${kc}" ]] || fail "kubeconfig ${kc} was not removed"
echo "OK: kubeconfig removed"

echo "DONE: ok=true"
echo "NEXT: none"
