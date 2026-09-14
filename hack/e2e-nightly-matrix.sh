#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Print the E2E Nightly k3s matrix JSON for one dispatch input.
# Usage: hack/e2e-nightly-matrix.sh <all|v1.32|v1.33|v1.34|v1.35|v1.36|v1.37>
#
# 1.32 stays (alpha InPlacePodVerticalScaling). 1.37 is experimental
# until k3s marks v1.37.0+k3s1 GA. 1.38 is omitted until an image exists.

set -euo pipefail

echo "PLAN: emit nightly k3s matrix JSON" >&2

selected="${1:-}"
if [[ -z "${selected}" ]]; then
  echo "FAIL: usage: $0 <all|v1.32|v1.33|v1.34|v1.35|v1.36|v1.37>" >&2
  echo "DONE: ok=false" >&2
  exit 2
fi

echo "DO: select ${selected}" >&2

case "${selected}" in
  v1.32)
    matrix='{"include":[{"k8s-version":"v1.32","k3s-image":"v1.32.13-k3s1","experimental":false}]}'
    ;;
  v1.33)
    matrix='{"include":[{"k8s-version":"v1.33","k3s-image":"v1.33.13-k3s2","experimental":false}]}'
    ;;
  v1.34)
    matrix='{"include":[{"k8s-version":"v1.34","k3s-image":"v1.34.11-k3s1","experimental":false}]}'
    ;;
  v1.35)
    matrix='{"include":[{"k8s-version":"v1.35","k3s-image":"v1.35.8-k3s1","experimental":false}]}'
    ;;
  v1.36)
    matrix='{"include":[{"k8s-version":"v1.36","k3s-image":"v1.36.4-k3s1","experimental":false}]}'
    ;;
  v1.37)
    matrix='{"include":[{"k8s-version":"v1.37","k3s-image":"v1.37.0-k3s1","experimental":true}]}'
    ;;
  all)
    matrix='{"include":[{"k8s-version":"v1.32","k3s-image":"v1.32.13-k3s1","experimental":false},{"k8s-version":"v1.33","k3s-image":"v1.33.13-k3s2","experimental":false},{"k8s-version":"v1.34","k3s-image":"v1.34.11-k3s1","experimental":false},{"k8s-version":"v1.35","k3s-image":"v1.35.8-k3s1","experimental":false},{"k8s-version":"v1.36","k3s-image":"v1.36.4-k3s1","experimental":false},{"k8s-version":"v1.37","k3s-image":"v1.37.0-k3s1","experimental":true}]}'
    ;;
  *)
    echo "FAIL: unknown k8s-version input: ${selected}" >&2
    echo "DONE: ok=false" >&2
    exit 1
    ;;
esac

echo "OK: ${selected}" >&2
echo "DONE: ok=true" >&2
printf '%s\n' "${matrix}"
