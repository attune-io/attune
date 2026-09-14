#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/e2e-nightly-matrix.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-nightly-matrix.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
CI="${ROOT}/.github/workflows/ci.yaml"
MAKEFILE="${ROOT}/Makefile"

echo "PLAN: classify e2e-nightly-matrix.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"
[[ -x "${SCRIPT}" ]] || fail "${SCRIPT} is not executable"

echo "DO: nightly must call the helper"
grep -F -q 'hack/e2e-nightly-matrix.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/e2e-nightly-matrix.sh"
# prepare-matrix used to be checkout-free (inline case). The helper
# lives in the repo, so that job must check out before calling it.
if ! awk '/prepare-matrix:/,/test-e2e:/' "${NIGHTLY}" | grep -q 'actions/checkout@'; then
  fail "prepare-matrix must check out the repo before calling the helper"
fi
if grep -n 'k3s-image":"v1.35.4-k3s1"' "${NIGHTLY}"; then
  fail "stale v1.35.4-k3s1 pin still inline in e2e-nightly.yaml"
fi
grep -F -q 'test_e2e_nightly_matrix.sh' "${MAKEFILE}" \
  || fail "Makefile python-test does not run this classifier"
echo "OK: nightly and Makefile wire the helper"

echo "DO: PR CI pin is newest required k3s"
grep -F -q 'K3S_IMAGE: "rancher/k3s:v1.36.4-k3s1"' "${CI}" \
  || fail "ci.yaml K3S_IMAGE is not rancher/k3s:v1.36.4-k3s1"
grep -F -q 'hack/e2e-nightly-matrix.sh' "${CI}" \
  || fail "ci.yaml scripts filter must include hack/e2e-nightly-matrix.sh"

echo "DO: all includes 1.32 through 1.37 and keeps 1.32"
all_json="$("${SCRIPT}" all)"
echo "${all_json}" | grep -q '"k8s-version":"v1.32"' || fail "all dropped v1.32"
echo "${all_json}" | grep -q '"k8s-version":"v1.36"' || fail "all missing v1.36"
echo "${all_json}" | grep -q '"k8s-version":"v1.37"' || fail "all missing v1.37"
echo "${all_json}" | grep -q '"k8s-version":"v1.38"' && fail "all must not include v1.38 until an image exists"
echo "${all_json}" | grep -q '"k3s-image":"v1.36.4-k3s1"' || fail "all missing v1.36.4-k3s1"
echo "${all_json}" | grep -q '"k3s-image":"v1.37.0-k3s1"' || fail "all missing v1.37.0-k3s1"
echo "${all_json}" | grep -q '"experimental":true' || fail "1.37 must be experimental"

echo "DO: single-version cells"
v36="$("${SCRIPT}" v1.36)"
echo "${v36}" | grep -q '"k8s-version":"v1.36"' || fail "v1.36 cell wrong"
echo "${v36}" | grep -q '"experimental":false' || fail "v1.36 must set experimental false"
echo "${v36}" | grep -q '"experimental":true' && fail "v1.36 must not be experimental"

echo "DO: unknown version fails"
if "${SCRIPT}" v1.38 >/dev/null 2>&1; then
  fail "v1.38 must fail until an image exists"
fi
if "${SCRIPT}" >/dev/null 2>&1; then
  fail "missing arg must fail"
fi

echo "OK: matrix helper"
echo "DONE: ok=true"
