#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verify the Helm chart default image tag is Chart.appVersion (bare
# SemVer). Releases publish both vX.Y.Z and X.Y.Z. Empty image.tag
# must not add a v prefix. artifacthub.io/images must name that same
# image. A :latest pin makes Artifact Hub scan a moving tag for every
# published chart version.
#
# This is the gate that would have caught attune-io/attune#546 (and
# a later v-prefix regression): helm lint and helm-unittest did not
# assert the default image, and E2E always --set image.tag= from a
# locally built image.

set -euo pipefail

ROOT=""

usage() {
  cat <<'EOF'
Usage: verify-helm-image-tag.sh [--root DIR]

Fail if helm template default image is not Chart.appVersion (bare SemVer).
--root   Repository root (default: parent of this script's directory).
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --root)
      if [[ $# -lt 2 ]]; then
        echo "FAIL: --root requires a directory argument" >&2
        exit 1
      fi
      ROOT="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "FAIL: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "${ROOT}" ]]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

echo "PLAN: assert default Helm image tag is Chart.appVersion (bare SemVer)"

CHART="${ROOT}/charts/attune"
CHART_YAML="${CHART}/Chart.yaml"

if [[ ! -f "${CHART_YAML}" ]]; then
  echo "FAIL: Chart.yaml not found at ${CHART_YAML}"
  echo "DONE: ok=false reason=missing-chart"
  exit 1
fi

if ! command -v helm >/dev/null 2>&1; then
  echo "FAIL: helm is required"
  echo "DONE: ok=false reason=missing-helm"
  exit 1
fi

echo "DO: read appVersion from ${CHART_YAML}"
APP_VERSION="$(awk '/^appVersion:/{print $2; exit}' "${CHART_YAML}" | tr -d '"')"
if [[ -z "${APP_VERSION}" ]]; then
  echo "FAIL: no appVersion in ${CHART_YAML}"
  echo "DONE: ok=false reason=unparseable-appversion"
  exit 1
fi

BARE="${APP_VERSION#v}"
EXPECTED_TAG="${BARE}"
echo "OK: appVersion=${APP_VERSION} expected_tag=${EXPECTED_TAG}"

echo "DO: helm template default image"
RENDERED="$(helm template attune "${CHART}" --set webhooks.enabled=false)"
DEFAULT_IMAGE="$(printf '%s\n' "${RENDERED}" | awk '/image:/{gsub(/"/, "", $2); print $2; exit}')"
if [[ -z "${DEFAULT_IMAGE}" ]]; then
  echo "FAIL: helm template produced no image:"
  echo "DONE: ok=false reason=no-image"
  exit 1
fi
echo "OK: default_image=${DEFAULT_IMAGE}"

DEFAULT_TAG="${DEFAULT_IMAGE##*:}"
REPO="${DEFAULT_IMAGE%:*}"
if [[ "${DEFAULT_TAG}" != "${EXPECTED_TAG}" ]]; then
  echo "FAIL: default image tag ${DEFAULT_TAG} != ${EXPECTED_TAG} (repo=${REPO})"
  echo "HINT: attune.imageTag must use Chart.appVersion (no v prefix) when image.tag is empty."
  echo "DONE: ok=false reason=wrong-default-tag got=${DEFAULT_TAG} want=${EXPECTED_TAG}"
  echo "NEXT: fix charts/attune/templates/_helpers.tpl attune.imageTag"
  exit 1
fi

echo "DO: helm template with explicit image.tag=e2e"
OVERRIDE_IMAGE="$(helm template attune "${CHART}" --set webhooks.enabled=false --set image.tag=e2e \
  | awk '/image:/{gsub(/"/, "", $2); print $2; exit}')"
if [[ "${OVERRIDE_IMAGE}" != "${REPO}:e2e" ]]; then
  echo "FAIL: explicit image.tag=e2e rendered ${OVERRIDE_IMAGE}, want ${REPO}:e2e"
  echo "DONE: ok=false reason=override-mutated"
  exit 1
fi
echo "OK: explicit tag e2e left unchanged"

echo "DO: artifacthub.io/images matches the default install image"
AH_RAW="$(awk '
  /artifacthub.io\/images:/ {
    if (match($0, /"[^"]*"/)) {
      s = substr($0, RSTART + 1, RLENGTH - 2)
      gsub(/\\n/, "\n", s)
      printf "%s\n", s
      exit
    }
    in_block = 1
    next
  }
  in_block {
    if ($0 ~ /^[^ \t#]/) exit
    print
  }
' "${CHART_YAML}")"
AH_IMAGE="$(printf '%s\n' "${AH_RAW}" | awk '/image:/{print $2; exit}')"
AH_IMAGE="${AH_IMAGE%\"}"
AH_IMAGE="${AH_IMAGE%%#*}"
AH_IMAGE="${AH_IMAGE//[[:space:]]/}"
EXPECTED_IMAGE="${REPO}:${EXPECTED_TAG}"
if [[ -z "${AH_IMAGE}" ]]; then
  echo "FAIL: artifacthub.io/images has no image"
  echo "DONE: ok=false reason=missing-artifacthub-image"
  echo "NEXT: set charts/attune/Chart.yaml artifacthub.io/images to ${EXPECTED_IMAGE}"
  exit 1
fi
if [[ "${AH_IMAGE}" != "${EXPECTED_IMAGE}" ]]; then
  echo "FAIL: artifacthub.io/images ${AH_IMAGE} != default install ${EXPECTED_IMAGE}"
  echo "HINT: Artifact Hub scans this annotation. :latest scans a moving tag for every chart version."
  echo "DONE: ok=false reason=artifacthub-image-drift got=${AH_IMAGE} want=${EXPECTED_IMAGE}"
  echo "NEXT: set the annotation image to ${EXPECTED_IMAGE}"
  exit 1
fi
echo "OK: artifacthub_image=${AH_IMAGE}"

echo "DONE: ok=true default=${DEFAULT_IMAGE}"
echo "NEXT: none"
exit 0
