#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier + unit tests for hack/e2e-download-cert-manager.sh.
# Nightly #726 died on a GitHub Releases HTTP 500 after a cache miss.
# Both E2E paths must share the helper, reject a 500 error body as
# "cached", and fall back to the vendored manifest.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-download-cert-manager.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"
CI_YAML="${ROOT}/.github/workflows/ci.yaml"

echo "PLAN: classify e2e-download-cert-manager.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"

echo "DO: nightly and setup-e2e-cluster must call the helper"
grep -F -q 'hack/e2e-download-cert-manager.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/e2e-download-cert-manager.sh"
grep -F -q 'hack/e2e-download-cert-manager.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/e2e-download-cert-manager.sh"
if grep -n 'curl -fsSL -o /tmp/cert-manager.yaml' "${NIGHTLY}" "${COMPOSITE}"; then
  fail "inline cert-manager.yaml curl still present; use the helper"
fi
echo "OK: both download paths call the helper"

echo "DO: yaml cache key must not include k3s-image"
if grep -n 'path: /tmp/cert-manager.yaml' -A6 "${NIGHTLY}" "${COMPOSITE}" \
  | grep -E 'key:.*k3s'; then
  fail "cert-manager.yaml is still cached under a per-k3s image key"
fi
grep -F -q 'e2e-cm-manifest-' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml missing dedicated e2e-cm-manifest- cache key"
grep -F -q 'e2e-cm-manifest-' "${COMPOSITE}" \
  || fail "setup-e2e-cluster missing dedicated e2e-cm-manifest- cache key"
echo "OK: yaml cache is version-scoped, not per k3s image"

CM_VER="$(grep 'CERT_MANAGER_VERSION:' "${CI_YAML}" | head -1 | sed 's/.*"\(.*\)".*/\1/')"
[[ -n "${CM_VER}" ]] || fail "could not read CERT_MANAGER_VERSION from ci.yaml"
BUNDLE="${ROOT}/hack/testdata/cert-manager-${CM_VER}.yaml"
[[ -f "${BUNDLE}" ]] || fail "missing vendored manifest ${BUNDLE}"
echo "OK: vendored bundle matches ${CM_VER}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

write_stub() {
  local dest="$1"
  {
    printf '%s\n' 'apiVersion: apiextensions.k8s.io/v1' 'kind: CustomResourceDefinition'
    printf '%s\n' 'metadata:' '  name: certificates.cert-manager.io'
    printf '%s\n' 'spec:' '  group: cert-manager.io'
    # Pad so a tiny error body cannot pass the default size floor in production.
    dd if=/dev/zero bs=1024 count=12 2>/dev/null | tr '\0' '#'
    printf '\n'
  } >"${dest}"
}

write_curl() {
  local mode="$1"
  cat >"${TMP}/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
LOG="${TMP}/curl.log"
out=""
url=""
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    -o|--output) out="\$2"; shift 2 ;;
    -*) shift ;;
    *) url="\$1"; shift ;;
  esac
done
echo "\${url}" >>"\${LOG}"
case "${mode}:\${url}" in
  fail:*github.com/cert-manager/cert-manager/releases/download*)
    printf '%s\n' 'Internal Server Error' >"\${out}"
    exit 22
    ;;
  fail:*)
    printf '%s\n' 'not found' >"\${out}"
    exit 22
    ;;
  ok:*github.com/cert-manager/cert-manager/releases/download*)
    cat "${TMP}/upstream.yaml" >"\${out}"
    exit 0
    ;;
  *)
    echo "unexpected curl \${url}" >&2
    exit 2
    ;;
esac
EOF
  chmod +x "${TMP}/curl"
}

run_helper() {
  CURL="${TMP}/curl" \
    CERT_MANAGER_VERSION="${CM_VER}" \
    CERT_MANAGER_BUNDLE="${TMP}/bundle.yaml" \
    DOWNLOAD_RETRY_SLEEP=0 \
    MANIFEST_MIN_BYTES=64 \
    bash "${SCRIPT}" "${TMP}/dest.yaml"
}

write_stub "${TMP}/upstream.yaml"
write_stub "${TMP}/bundle.yaml"

echo "DO: valid dest is reused without curling"
write_stub "${TMP}/dest.yaml"
write_curl fail
if ! run_helper; then
  fail "valid dest should be treated as cached"
fi
if [[ -f "${TMP}/curl.log" ]]; then
  fail "cached dest must not invoke curl"
fi
echo "OK: cached valid dest"

echo "DO: GitHub 500 must not leave dest treated as cached; use bundle"
rm -f "${TMP}/dest.yaml" "${TMP}/curl.log"
write_curl fail
if ! run_helper; then
  fail "bundle fallback should succeed after remote 500"
fi
grep -q 'kind: CustomResourceDefinition' "${TMP}/dest.yaml" \
  || fail "dest after fallback is not a manifest"
if grep -q 'Internal Server Error' "${TMP}/dest.yaml"; then
  fail "500 error body was left as dest"
fi
[[ -s "${TMP}/curl.log" ]] || fail "expected curl to be attempted before bundle"
echo "OK: 500 falls back to vendored bundle"

echo "DO: garbage dest is discarded and replaced"
printf '%s\n' 'Internal Server Error' >"${TMP}/dest.yaml"
rm -f "${TMP}/curl.log"
write_curl fail
if ! run_helper; then
  fail "garbage dest should be replaced from bundle"
fi
if grep -q 'Internal Server Error' "${TMP}/dest.yaml"; then
  fail "garbage dest was kept"
fi
echo "OK: discarded 500-body dest"

echo "DO: remote success is used without the bundle"
rm -f "${TMP}/dest.yaml" "${TMP}/curl.log"
write_curl ok
CERT_MANAGER_BUNDLE="${TMP}/missing-bundle.yaml" \
  CURL="${TMP}/curl" \
  CERT_MANAGER_VERSION="${CM_VER}" \
  DOWNLOAD_RETRY_SLEEP=0 \
  MANIFEST_MIN_BYTES=64 \
  bash "${SCRIPT}" "${TMP}/dest.yaml" \
  || fail "remote success should not need the bundle"
grep -q 'kind: CustomResourceDefinition' "${TMP}/dest.yaml" \
  || fail "remote dest is not a manifest"
echo "OK: remote success"

echo "DO: all remotes fail and no bundle must fail closed"
rm -f "${TMP}/dest.yaml" "${TMP}/curl.log"
write_curl fail
if CURL="${TMP}/curl" \
    CERT_MANAGER_VERSION="${CM_VER}" \
    CERT_MANAGER_BUNDLE="${TMP}/missing-bundle.yaml" \
    DOWNLOAD_RETRY_SLEEP=0 \
    DOWNLOAD_ATTEMPTS=2 \
    MANIFEST_MIN_BYTES=64 \
    bash "${SCRIPT}" "${TMP}/dest.yaml"; then
  fail "missing remotes and missing bundle should exit non-zero"
fi
if [[ -f "${TMP}/dest.yaml" ]] && grep -q 'Internal Server Error' "${TMP}/dest.yaml"; then
  fail "failed download left a 500 body at dest"
fi
echo "OK: fail-closed without remotes or bundle"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
