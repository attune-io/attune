#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Print newline-separated container tags for a release.
# Always emit the git tag as-is. When the tag starts with v, also emit
# the bare SemVer alias so older Helm charts that used Chart.appVersion
# as the image tag (issue #546) can still pull.

set -euo pipefail

TAG=""
REGISTRY="ghcr.io"
IMAGE="attune-io/attune"
DOCKER="docker.io/attuneio/attune"
INCLUDE_LATEST=0

usage() {
  cat <<'EOF'
Usage: release-image-tags.sh --tag TAG [--registry REG] [--image NAME] [--docker REF] [--latest]

Print one image reference per line (GHCR then Docker Hub).
--tag       Git tag or SemVer (v0.1.23 or 0.1.23). Required.
--registry  GHCR registry host (default: ghcr.io).
--image     GHCR repository path (default: attune-io/attune).
--docker    Docker Hub repository (default: docker.io/attuneio/attune).
--latest    Also emit :latest on both registries.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      [[ $# -ge 2 ]] || { echo "FAIL: --tag requires a value" >&2; exit 1; }
      TAG="$2"
      shift 2
      ;;
    --registry)
      [[ $# -ge 2 ]] || { echo "FAIL: --registry requires a value" >&2; exit 1; }
      REGISTRY="$2"
      shift 2
      ;;
    --image)
      [[ $# -ge 2 ]] || { echo "FAIL: --image requires a value" >&2; exit 1; }
      IMAGE="$2"
      shift 2
      ;;
    --docker)
      [[ $# -ge 2 ]] || { echo "FAIL: --docker requires a value" >&2; exit 1; }
      DOCKER="$2"
      shift 2
      ;;
    --latest)
      INCLUDE_LATEST=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "FAIL: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "${TAG}" ]]; then
  echo "FAIL: --tag is required" >&2
  usage >&2
  exit 1
fi

echo "PLAN: compute release image tags for ${TAG}" >&2

GHCR="${REGISTRY}/${IMAGE}"
BARE="${TAG#v}"

echo "DO: emit ${TAG} on GHCR and Docker Hub" >&2
printf '%s\n' "${GHCR}:${TAG}"
printf '%s\n' "${DOCKER}:${TAG}"

if [[ "${BARE}" != "${TAG}" ]]; then
  echo "DO: emit bare SemVer alias ${BARE}" >&2
  printf '%s\n' "${GHCR}:${BARE}"
  printf '%s\n' "${DOCKER}:${BARE}"
fi

if [[ "${INCLUDE_LATEST}" -eq 1 ]]; then
  echo "DO: emit latest" >&2
  printf '%s\n' "${GHCR}:latest"
  printf '%s\n' "${DOCKER}:latest"
fi

echo "DONE: ok=true tag=${TAG} bare=${BARE} latest=${INCLUDE_LATEST}" >&2
echo "NEXT: none" >&2
