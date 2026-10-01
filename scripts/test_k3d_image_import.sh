#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/k3d-image-import.sh: keep the tools node, bound
# each attempt, and retry once after removing a hung tools container.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/k3d-image-import.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"

echo "PLAN: classify k3d-image-import.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"

echo "DO: nightly and setup-e2e-cluster must call the helper"
grep -F -q 'hack/k3d-image-import.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/k3d-image-import.sh"
grep -F -q 'hack/k3d-image-import.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/k3d-image-import.sh"
if grep -nE '^[[:space:]]*k3d image import' "${NIGHTLY}" "${COMPOSITE}"; then
  fail "raw k3d image import remains in the CI setup paths"
fi
echo "OK: both import paths call the helper"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
BIN="${TMP}/bin"
mkdir -p "${BIN}"
LOG="${TMP}/timeout.log"
DOCKER_LOG="${TMP}/docker.log"
: >"${LOG}"
: >"${DOCKER_LOG}"

cat >"${BIN}/timeout" <<EOF
#!/usr/bin/env bash
set -euo pipefail
while [[ \$# -gt 0 && "\$1" != "k3d" ]]; do
  shift
done
printf '%s\n' "\$*" >> "${LOG}"
mode=\$(cat "${TMP}/mode")
count=\$(wc -l < "${LOG}" | tr -d ' ')
if [[ "\${mode}" == "hang-once" && "\${count}" == "1" ]]; then
  exit 124
fi
if [[ "\${mode}" == "always-fail" ]]; then
  exit 124
fi
exit 0
EOF
cat >"${BIN}/docker" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${DOCKER_LOG}"
exit 0
EOF
cat >"${BIN}/k3d" <<'EOF'
#!/usr/bin/env bash
echo "k3d should be invoked by the timeout stub" >&2
exit 99
EOF
chmod +x "${BIN}/timeout" "${BIN}/docker" "${BIN}/k3d"

run_import() {
  PATH="${BIN}:${PATH}" "$SCRIPT" "$@"
}

echo "DO: a successful import keeps the tools node and does not remove it"
echo success >"${TMP}/mode"
: >"${LOG}"
: >"${DOCKER_LOG}"
run_import e2e /tmp/prometheus.tar /tmp/busybox.tar
grep -F -q 'k3d image import --keep-tools /tmp/prometheus.tar /tmp/busybox.tar -c e2e' "${LOG}" \
  || fail "successful import did not pass --keep-tools"
[[ ! -s "${DOCKER_LOG}" ]] || fail "successful import removed the tools container"
echo "OK: success path keeps the tools node"

echo "DO: a timed-out start removes the tools container and retries once"
echo hang-once >"${TMP}/mode"
: >"${LOG}"
: >"${DOCKER_LOG}"
warn=$(run_import e2e /tmp/prometheus.tar)
lines=$(wc -l < "${LOG}" | tr -d ' ')
[[ "${lines}" == "2" ]] || fail "expected 2 import attempts, got ${lines}"
printf '%s\n' "${warn}" | grep -F -q 'status 124' \
  || fail "timeout warning lost the exit status: ${warn}"
grep -F -q 'rm -f k3d-e2e-tools' "${DOCKER_LOG}" \
  || fail "timeout did not remove k3d-e2e-tools"
echo "OK: timeout retries once after removing the tools container"

echo "DO: two timeouts fail and name the tarball"
echo always-fail >"${TMP}/mode"
: >"${LOG}"
: >"${DOCKER_LOG}"
set +e
err=$(run_import e2e /tmp/prometheus.tar 2>&1)
status=$?
set -e
[[ "${status}" -ne 0 ]] || fail "two timeouts exited 0"
printf '%s\n' "${err}" | grep -F -q '/tmp/prometheus.tar' \
  || fail "failure did not name the tarball"
echo "OK: repeated timeout fails closed"

echo "DONE: ok=true"
