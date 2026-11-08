#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pull the pinned prometheus-community Helm chart to DEST.
# actions/cache restores DEST before this runs. A valid tarball skips
# helm repo add, helm repo update, and helm pull. A cold cache still
# retries the pull: several jobs can miss at once (nightly #1125,
# GitHub Releases HTTP 504). Do not vendor the .tgz.
#
# DEST must be named prometheus-<version>.tgz. Override HELM,
# PROMETHEUS_CHART_VERSION, DOWNLOAD_ATTEMPTS, and DOWNLOAD_RETRY_SLEEP
# in unit tests.

set -euo pipefail

HELM="${HELM:-helm}"
VER="${PROMETHEUS_CHART_VERSION:-27.22.0}"
DOWNLOAD_ATTEMPTS="${DOWNLOAD_ATTEMPTS:-3}"
DOWNLOAD_RETRY_SLEEP="${DOWNLOAD_RETRY_SLEEP:-10}"
REPO_NAME="${PROMETHEUS_HELM_REPO_NAME:-prometheus-community}"
REPO_URL="${PROMETHEUS_HELM_REPO_URL:-https://prometheus-community.github.io/helm-charts}"

if [[ -z "${1:-}" ]]; then
  echo "usage: e2e-pull-prometheus-chart.sh DEST" >&2
  exit 2
fi
DEST="$1"

if [[ "$(basename "${DEST}")" != "prometheus-${VER}.tgz" ]]; then
  echo "::error::DEST must be named prometheus-${VER}.tgz" >&2
  exit 2
fi

# Root Chart.yaml only. A packaged chart also nests dependency
# Chart.yaml files under charts/, and those must not count as a hit.
chart_ok() {
  local path="$1" listing member tmp meta name_got ver_got
  [[ -s "${path}" ]] || return 1
  listing="$(tar -tzf "${path}" 2>/dev/null)" || return 1
  # First root Chart.yaml. awk exits after one line; `|| true` covers
  # grep exiting 1 and the SIGPIPE from that early exit.
  member="$(printf '%s\n' "${listing}" | grep -E '^([^/]+/)?Chart\.yaml$' | awk 'NR==1 { print; exit }')" || true
  [[ -n "${member}" ]] || return 1
  tmp="$(mktemp -d)"
  if ! tar -xzf "${path}" -C "${tmp}" "${member}"; then
    rm -rf "${tmp}"
    return 1
  fi
  meta="${tmp}/${member}"
  if [[ ! -f "${meta}" ]]; then
    rm -rf "${tmp}"
    return 1
  fi
  # Exact field match. A regex would treat dots in the version as wildcards.
  name_got="$(sed -n 's/^name:[[:space:]]*"\{0,1\}\([^"]*\)"\{0,1\}[[:space:]]*$/\1/p' "${meta}")"
  ver_got="$(sed -n 's/^version:[[:space:]]*"\{0,1\}\([^"]*\)"\{0,1\}[[:space:]]*$/\1/p' "${meta}")"
  rm -rf "${tmp}"
  [[ "${name_got}" == "prometheus" && "${ver_got}" == "${VER}" ]]
}

if chart_ok "${DEST}"; then
  echo "Cached: ${DEST}"
  exit 0
fi
if [[ -e "${DEST}" ]]; then
  echo "::warning::discarding invalid ${DEST}"
  rm -f "${DEST}"
fi

dest_dir="$(cd "$(dirname "${DEST}")" && pwd)"

echo "DO: helm repo add ${REPO_NAME}"
"${HELM}" repo add "${REPO_NAME}" "${REPO_URL}"
echo "DO: helm repo update"
"${HELM}" repo update

attempt=1
while (( attempt <= DOWNLOAD_ATTEMPTS )); do
  echo "DO: helm pull prometheus ${VER} attempt ${attempt}/${DOWNLOAD_ATTEMPTS}"
  rm -f "${DEST}"
  if "${HELM}" pull "${REPO_NAME}/prometheus" \
    --version "${VER}" \
    --destination "${dest_dir}"; then
    if chart_ok "${DEST}"; then
      echo "OK: pulled ${VER} -> ${DEST}"
      exit 0
    fi
    echo "::warning::helm pull wrote an unexpected chart at ${DEST}"
  fi
  rm -f "${DEST}"
  if (( attempt < DOWNLOAD_ATTEMPTS )); then
    echo "::warning::helm pull attempt ${attempt} failed, retrying in ${DOWNLOAD_RETRY_SLEEP}s..."
    sleep "${DOWNLOAD_RETRY_SLEEP}"
  fi
  attempt=$((attempt + 1))
done

echo "::error::Failed to pull Prometheus chart ${VER} after ${DOWNLOAD_ATTEMPTS} attempts"
exit 1
