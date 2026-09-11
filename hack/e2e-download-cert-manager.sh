#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Fetch cert-manager.yaml for E2E. GitHub Releases HTTP 500 (#726) must
# not fail the job when a vendored copy of the pinned version exists.
# A 500 error body at DEST is not treated as a cache hit.
#
# Used by e2e-nightly.yaml and setup-e2e-cluster so the two paths cannot
# drift. Override CURL / CERT_MANAGER_* / DOWNLOAD_* / MANIFEST_MIN_BYTES
# in unit tests.

set -euo pipefail

CURL="${CURL:-curl}"
VER="${CERT_MANAGER_VERSION:-v1.21.1}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUNDLE="${CERT_MANAGER_BUNDLE:-${ROOT}/hack/testdata/cert-manager-${VER}.yaml}"
DOWNLOAD_ATTEMPTS="${DOWNLOAD_ATTEMPTS:-3}"
DOWNLOAD_RETRY_SLEEP="${DOWNLOAD_RETRY_SLEEP:-10}"
MANIFEST_MIN_BYTES="${MANIFEST_MIN_BYTES:-10000}"

if [[ -z "${1:-}" ]]; then
  echo "usage: e2e-download-cert-manager.sh DEST" >&2
  exit 2
fi
DEST="$1"
TMP="${DEST}.tmp.$$"

cleanup() { rm -f "${TMP}"; }
trap cleanup EXIT

manifest_ok() {
  local path="$1" size
  [[ -f "${path}" ]] || return 1
  size="$(wc -c <"${path}" | tr -d ' ')"
  (( size >= MANIFEST_MIN_BYTES )) || return 1
  grep -q 'cert-manager.io' "${path}" || return 1
  grep -q 'kind: CustomResourceDefinition' "${path}" || return 1
}

install_from() {
  local src="$1"
  cp "${src}" "${DEST}"
  chmod 644 "${DEST}"
}

fetch_url() {
  local url="$1"
  rm -f "${TMP}"
  if ! "${CURL}" -fsSL -o "${TMP}" "${url}"; then
    rm -f "${TMP}"
    return 1
  fi
  if ! manifest_ok "${TMP}"; then
    echo "::warning::download from ${url} was not a cert-manager manifest"
    rm -f "${TMP}"
    return 1
  fi
  install_from "${TMP}"
}

if manifest_ok "${DEST}"; then
  echo "Cached: ${DEST}"
  exit 0
fi
if [[ -e "${DEST}" ]]; then
  echo "::warning::discarding invalid ${DEST} (not a cert-manager manifest)"
  rm -f "${DEST}"
fi

URLS=(
  "https://github.com/cert-manager/cert-manager/releases/download/${VER}/cert-manager.yaml"
)

attempt=1
while (( attempt <= DOWNLOAD_ATTEMPTS )); do
  echo "DO: download cert-manager ${VER} attempt ${attempt}/${DOWNLOAD_ATTEMPTS}"
  for url in "${URLS[@]}"; do
    if fetch_url "${url}"; then
      echo "OK: downloaded ${url} -> ${DEST}"
      exit 0
    fi
    echo "::warning::cert-manager.yaml download from ${url} failed"
  done
  # Prefer the pinned bundle over more GitHub retries. Nightly #726
  # burned 30s on three HTTP 500s and then failed; the vendor copy
  # is the same bytes as the release asset.
  if manifest_ok "${BUNDLE}"; then
    echo "::warning::using vendored cert-manager manifest ${BUNDLE}"
    install_from "${BUNDLE}"
    echo "OK: installed vendored ${BUNDLE} -> ${DEST}"
    exit 0
  fi
  if (( attempt < DOWNLOAD_ATTEMPTS )); then
    sleep "${DOWNLOAD_RETRY_SLEEP}"
  fi
  attempt=$((attempt + 1))
done

echo "::error::Failed to download cert-manager.yaml for ${VER} after ${DOWNLOAD_ATTEMPTS} attempts"
exit 1
