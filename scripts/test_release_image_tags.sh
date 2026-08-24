#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/release-image-tags.sh: v-prefixed tags must emit
# both vX.Y.Z and X.Y.Z; already-bare tags must not emit vv or a
# duplicate v alias.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/release-image-tags.sh"

echo "PLAN: classify release-image-tags.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

expect_lines() {
  local desc="$1"
  shift
  local got
  got="$(bash "${SCRIPT}" "$@")"
  echo "DO: ${desc}"
  echo "${got}" | sed 's/^/  /'
  local want
  for want in "${expected[@]}"; do
    echo "${got}" | grep -Fxq "${want}" || fail "${desc}: missing ${want}"
  done
  local extra
  extra="$(echo "${got}" | grep -v '^$' | wc -l | tr -d ' ')"
  if [[ "${extra}" -ne "${#expected[@]}" ]]; then
    fail "${desc}: got ${extra} tags, want ${#expected[@]}"
  fi
  echo "OK: ${desc}"
}

expected=(
  "ghcr.io/attune-io/attune:v0.1.23"
  "docker.io/attuneio/attune:v0.1.23"
  "ghcr.io/attune-io/attune:0.1.23"
  "docker.io/attuneio/attune:0.1.23"
)
expect_lines "v-prefixed tag emits v and bare aliases" --tag v0.1.23

expected=(
  "ghcr.io/attune-io/attune:v0.1.23"
  "docker.io/attuneio/attune:v0.1.23"
  "ghcr.io/attune-io/attune:0.1.23"
  "docker.io/attuneio/attune:0.1.23"
  "ghcr.io/attune-io/attune:latest"
  "docker.io/attuneio/attune:latest"
)
expect_lines "v-prefixed tag with --latest" --tag v0.1.23 --latest

expected=(
  "ghcr.io/attune-io/attune:0.1.23"
  "docker.io/attuneio/attune:0.1.23"
)
expect_lines "already-bare tag does not add a v prefix" --tag 0.1.23

if bash "${SCRIPT}" --tag v0.1.23 | grep -q ':vv'; then
  fail "v-prefixed tag emitted vv"
fi
echo "OK: no vv prefix"

echo "DONE: ok=true"
echo "NEXT: none"
