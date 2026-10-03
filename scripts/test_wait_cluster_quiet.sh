#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/wait-cluster-quiet.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/wait-cluster-quiet.sh"

echo "PLAN: classify wait-cluster-quiet.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"
[[ -x "${SCRIPT}" ]] || fail "${SCRIPT} is not executable"

echo "DO: CI and the Makefile must call the helper and cap Go E2E parallelism"
for f in \
  "${ROOT}/.github/workflows/e2e-nightly.yaml" \
  "${ROOT}/.github/workflows/ci.yaml" \
  "${ROOT}/Makefile"; do
  grep -F -q 'hack/wait-cluster-quiet.sh' "${f}" || fail "${f} does not call the helper"
  grep -F -q -- '-parallel=2' "${f}" || fail "${f} does not set -parallel=2"
  grep -F -q -- '-timeout=30m' "${f}" || fail "${f} does not set -timeout=30m"
done
echo "OK: both E2E workflows and Makefile share the gate and the cap"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

cat > "${TMP}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${CLUSTER_QUIET_STATE:?}"
mode="$(cat "${state}")"
if [[ "$1" == "get" && "$2" == "--raw=/readyz" ]]; then
  if [[ "${mode}" == "noready" ]]; then
    exit 1
  fi
  exit 0
fi
if [[ "$1" == "get" && "$2" == "pods" ]]; then
  if [[ "${mode}" == "busy" ]]; then
    printf '%s\n' '{"items":[{"metadata":{"namespace":"chainsaw-left","name":"app","deletionTimestamp":"2026-10-03T00:00:00Z"},"status":{"phase":"Running"}}]}'
    exit 0
  fi
  if [[ "${mode}" == "noready" ]]; then
    printf '%s\n' '{"items":[]}'
    exit 0
  fi
  if [[ "${mode}" == "then-clear" ]]; then
    printf '%s\n' 'clear' > "${state}"
    printf '%s\n' '{"items":[{"metadata":{"namespace":"chainsaw-left","name":"app"},"status":{"phase":"Running"}}]}'
    exit 0
  fi
  printf '%s\n' '{"items":[{"metadata":{"namespace":"monitoring","name":"prom"},"status":{"phase":"Running"}}]}'
  exit 0
fi
echo "unexpected kubectl $*" >&2
exit 1
EOF
chmod +x "${TMP}/kubectl"

echo "DO: a terminating workload pod keeps the gate closed"
printf '%s\n' 'busy' > "${TMP}/state"
if CLUSTER_QUIET_DEADLINE_SEC=1 CLUSTER_QUIET_INTERVAL_SEC=1 \
  KUBECTL="${TMP}/kubectl" CLUSTER_QUIET_STATE="${TMP}/state" \
  bash "${SCRIPT}"; then
  fail "busy cluster returned success"
fi
echo "OK: busy cluster fails closed"

echo "DO: /readyz failure keeps the gate closed"
printf '%s\n' 'noready' > "${TMP}/state"
if CLUSTER_QUIET_DEADLINE_SEC=1 CLUSTER_QUIET_INTERVAL_SEC=1 \
  KUBECTL="${TMP}/kubectl" CLUSTER_QUIET_STATE="${TMP}/state" \
  bash "${SCRIPT}"; then
  fail "unready API returned success"
fi
echo "OK: unready API fails closed"

echo "DO: one leftover pod then a quiet cluster succeeds"
printf '%s\n' 'then-clear' > "${TMP}/state"
CLUSTER_QUIET_DEADLINE_SEC=10 CLUSTER_QUIET_INTERVAL_SEC=1 \
  KUBECTL="${TMP}/kubectl" CLUSTER_QUIET_STATE="${TMP}/state" \
  bash "${SCRIPT}" >/dev/null
echo "OK: gate opens after pods leave"

echo "DONE: ok=true"
