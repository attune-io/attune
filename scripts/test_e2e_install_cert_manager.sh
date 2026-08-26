#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/e2e-install-cert-manager.sh: first-apply OpenAPI
# failures must retry; a permanently down apiserver must fail closed.
# Also assert nightly and the PR-CI composite both call the helper.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-install-cert-manager.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"

echo "PLAN: classify e2e-install-cert-manager.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"

echo "DO: nightly and setup-e2e-cluster must call the helper"
grep -F -q 'hack/e2e-install-cert-manager.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/e2e-install-cert-manager.sh"
grep -F -q 'hack/e2e-install-cert-manager.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/e2e-install-cert-manager.sh"
if grep -n 'kubectl apply -f /tmp/cert-manager.yaml' "${NIGHTLY}" "${COMPOSITE}"; then
  fail "bare kubectl apply of cert-manager.yaml still present; use the helper"
fi
echo "OK: both install paths call the helper"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
MANIFEST="${TMP}/cert-manager.yaml"
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' >"${MANIFEST}"

write_kubectl() {
  local apply_fails="$1" readyz_fails="$2"
  cat >"${TMP}/kubectl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
STATE="${TMP}/state"
mkdir -p "\${STATE}"
cmd="\${1:-}"
shift || true
case "\${cmd}" in
  get)
    if [[ "\${*}" == *readyz* ]]; then
      n=\$(cat "\${STATE}/readyz" 2>/dev/null || echo 0)
      n=\$((n + 1))
      echo "\${n}" >"\${STATE}/readyz"
      if (( n <= ${readyz_fails} )); then
        echo "Error from server (ServiceUnavailable): apiserver not ready" >&2
        exit 1
      fi
      echo ok
      exit 0
    fi
    echo "unexpected kubectl get \$*" >&2
    exit 2
    ;;
  apply)
    n=\$(cat "\${STATE}/apply" 2>/dev/null || echo 0)
    n=\$((n + 1))
    echo "\${n}" >"\${STATE}/apply"
    if (( n <= ${apply_fails} )); then
      echo 'error: error validating "${MANIFEST}": error validating data: failed to download openapi: the server is currently unable to handle the request' >&2
      exit 1
    fi
    echo "configmap/ok created"
    exit 0
    ;;
  wait)
    echo "deployment.apps/cert-manager condition met"
    exit 0
    ;;
  *)
    echo "unexpected kubectl \${cmd} \$*" >&2
    exit 2
    ;;
esac
EOF
  chmod +x "${TMP}/kubectl"
}

run_helper() {
  KUBECTL="${TMP}/kubectl" \
    READYZ_ATTEMPTS=5 \
    READYZ_SLEEP=0 \
    APPLY_ATTEMPTS=3 \
    APPLY_RETRY_SLEEP=0 \
    bash "${SCRIPT}" "${MANIFEST}"
}

echo "DO: first apply OpenAPI failure then success must pass"
rm -rf "${TMP}/state"
write_kubectl 1 0
if ! run_helper; then
  fail "retry after one apply failure should succeed"
fi
apply_n="$(cat "${TMP}/state/apply")"
[[ "${apply_n}" == "2" ]] || fail "expected 2 apply calls, got ${apply_n}"
echo "OK: retried apply after OpenAPI failure"

echo "DO: first readyz miss then apply success must pass"
rm -rf "${TMP}/state"
write_kubectl 0 1
if ! run_helper; then
  fail "readyz miss then success should succeed"
fi
readyz_n="$(cat "${TMP}/state/readyz")"
[[ "${readyz_n}" == "2" ]] || fail "expected 2 readyz calls, got ${readyz_n}"
echo "OK: waited for readyz before apply"

echo "DO: three apply failures must fail closed"
rm -rf "${TMP}/state"
write_kubectl 99 0
if run_helper; then
  fail "permanent apply failure should exit non-zero"
fi
apply_n="$(cat "${TMP}/state/apply")"
[[ "${apply_n}" == "3" ]] || fail "expected 3 apply calls, got ${apply_n}"
echo "OK: exhausted apply retries"

echo "DO: never-ready apiserver must fail closed"
rm -rf "${TMP}/state"
write_kubectl 0 99
if run_helper; then
  fail "never-ready readyz should exit non-zero"
fi
if [[ -f "${TMP}/state/apply" ]]; then
  fail "apply must not run when readyz never succeeds"
fi
echo "OK: fail-closed when apiserver never becomes ready"

echo "DO: --wait-only must poll readyz and skip apply"
rm -rf "${TMP}/state"
write_kubectl 0 1
if ! KUBECTL="${TMP}/kubectl" READYZ_ATTEMPTS=5 READYZ_SLEEP=0 \
    bash "${SCRIPT}" --wait-only; then
  fail "--wait-only should succeed after one readyz miss"
fi
if [[ -f "${TMP}/state/apply" ]]; then
  fail "--wait-only must not call kubectl apply"
fi
readyz_n="$(cat "${TMP}/state/readyz")"
[[ "${readyz_n}" == "2" ]] || fail "--wait-only expected 2 readyz calls, got ${readyz_n}"
echo "OK: --wait-only"

echo "DO: both paths must re-wait after Prometheus image import"
grep -F -A6 'Load Prometheus and test images into cluster' "${NIGHTLY}" \
  | grep -F -q 'hack/e2e-install-cert-manager.sh --wait-only' \
  || fail "e2e-nightly.yaml missing --wait-only after Prometheus image import"
grep -F -A6 'Load Prometheus and test images into cluster' "${COMPOSITE}" \
  | grep -F -q 'hack/e2e-install-cert-manager.sh --wait-only' \
  || fail "setup-e2e-cluster missing --wait-only after Prometheus image import"
echo "OK: Prometheus import waits for readyz"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
