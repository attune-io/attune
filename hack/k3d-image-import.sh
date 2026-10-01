#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Import image tarballs into one k3d cluster.
#
# k3d v5.8.3 deletes its tools container after every import. The next
# import creates a container with the same name, and docker.ContainerStart
# can block with no further log until the job timeout (nightly #907).
# --keep-tools leaves that container running. Cluster delete removes it.
#
# k3d does not time out ContainerStart. Bound each attempt to 5 minutes.
# On failure, remove the tools container and try once more.

set -euo pipefail

usage() {
  echo "usage: k3d-image-import.sh CLUSTER ARCHIVE [ARCHIVE...]" >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

CLUSTER="${1:-}"
if [[ -n "${1:-}" ]]; then
  shift
fi
if [[ -z "${CLUSTER}" || $# -lt 1 ]]; then
  usage
  exit 2
fi

TOOLS="k3d-${CLUSTER}-tools"
IMPORT_TIMEOUT="${IMPORT_TIMEOUT:-5m}"

import_once() {
  timeout --signal=TERM --kill-after=15s "${IMPORT_TIMEOUT}" \
    k3d image import --keep-tools "$@" -c "${CLUSTER}"
}

status=0
import_once "$@" || status=$?
if [[ "${status}" -eq 0 ]]; then
  exit 0
fi
echo "::warning::k3d image import for ${CLUSTER} failed (status ${status}); removing ${TOOLS} and retrying once"
docker rm -f "${TOOLS}" >/dev/null 2>&1 || true
status=0
import_once "$@" || status=$?
if [[ "${status}" -eq 0 ]]; then
  exit 0
fi
echo "::error::k3d image import failed for ${CLUSTER} after one retry: $*" >&2
exit 1
