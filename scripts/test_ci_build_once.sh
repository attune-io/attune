#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Lock Recipe A: product test/security compile workflows must not
# retrigger on push to main. Promote/release workflows may.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CI="${ROOT}/.github/workflows/ci.yaml"
SECURITY="${ROOT}/.github/workflows/security.yaml"
RELEASE="${ROOT}/.github/workflows/release.yaml"
DOCS="${ROOT}/.github/workflows/docs.yaml"
BENCH="${ROOT}/.github/workflows/bench-baseline.yaml"

echo "PLAN: lock CI/Security triggers off push to main"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

extract_on() {
  awk '
    /^on:/ {p=1; next}
    p && /^[A-Za-z]/ {exit}
    p {print}
  ' "$1"
}

[[ -f "${CI}" ]] || fail "missing ${CI}"
[[ -f "${SECURITY}" ]] || fail "missing ${SECURITY}"
[[ -f "${RELEASE}" ]] || fail "missing ${RELEASE}"
[[ -f "${DOCS}" ]] || fail "missing ${DOCS}"
[[ -f "${BENCH}" ]] || fail "missing ${BENCH}"

echo "DO: ci.yaml and security.yaml keep PR + dispatch, no push"
for f in "${CI}" "${SECURITY}"; do
  on_block="$(extract_on "${f}")"
  grep -q '^  pull_request:' <<<"${on_block}" || fail "${f}: missing pull_request"
  grep -q '^  workflow_dispatch:' <<<"${on_block}" || fail "${f}: missing workflow_dispatch"
  if grep -q '^  push:' <<<"${on_block}"; then
    fail "${f}: push trigger would recompile the same tree on main"
  fi
  if grep -q '^  tags:' <<<"${on_block}"; then
    fail "${f}: tag trigger would compile again on release"
  fi
  echo "OK: $(basename "${f}") on-block has no push/tags"
done

echo "DO: security.yaml keeps a schedule (weekly scan, not a third compile)"
sec_on="$(extract_on "${SECURITY}")"
grep -q '^  schedule:' <<<"${sec_on}" || fail "security.yaml: missing schedule"

echo "DO: release.yaml and docs.yaml still run on push to main (promote)"
rel_on="$(extract_on "${RELEASE}")"
docs_on="$(extract_on "${DOCS}")"
grep -q '^  push:' <<<"${rel_on}" || fail "release.yaml: missing push (release-please)"
grep -q '^  push:' <<<"${docs_on}" || fail "docs.yaml: missing push (Pages deploy)"

echo "DO: release.yaml must not run the test matrix"
if grep -nE 'go test|make test|cargo test|cargo nextest' "${RELEASE}"; then
  fail "release.yaml contains a test-matrix command"
fi
echo "OK: release.yaml has no test matrix"

echo "DO: PR CI restores bench baselines; main bench-baseline.yaml writes them"
if grep -n 'actions/cache/save' "${CI}"; then
  fail "ci.yaml must not cache/save (PR cache is not visible to other PRs)"
fi
grep -q 'actions/cache/restore' "${CI}" || fail "ci.yaml must restore bench baselines"
bench_on="$(extract_on "${BENCH}")"
grep -q '^  push:' <<<"${bench_on}" || fail "bench-baseline.yaml: missing push (shared cache scope)"
grep -q 'actions/cache/save' "${BENCH}" || fail "bench-baseline.yaml must cache/save on main"
echo "OK: bench restore on PR, save on main"

echo "DO: lock predicate fails closed when push: is injected"
tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT
awk '
  /^on:/ {print; print "  push:"; print "    branches: [main]"; next}
  {print}
' "${CI}" > "${tmpdir}/ci-with-push.yaml"
mut_on="$(extract_on "${tmpdir}/ci-with-push.yaml")"
grep -q '^  push:' <<<"${mut_on}" || fail "mutation did not add push: to on-block"
echo "OK: extract_on detects an injected push trigger"

echo "DONE: ok=true"
