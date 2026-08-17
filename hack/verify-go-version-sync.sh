#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Keep go.mod `go X.Y.Z` in sync with the Dockerfile `golang:X.Y.Z` pin.
# setup-go and govulncheck read go.mod. Dependabot docker PRs only bump
# the image tag, so the two pins drift unless something copies the tag
# into go.mod (this script, or the Dependabot auto-merge workflow).

set -euo pipefail

MODE="check"
ROOT=""

usage() {
  cat <<'EOF'
Usage: verify-go-version-sync.sh [--check|--write] [--root DIR]

--check  Fail if go.mod and Dockerfile Go versions differ (default).
--write  Update go.mod `go` directive to match Dockerfile golang: tag.
--root   Repository root (default: parent of this script's directory).
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --check) MODE="check"; shift ;;
    --write) MODE="write"; shift ;;
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

if [[ -z "$ROOT" ]]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

echo "PLAN: compare go.mod go directive with Dockerfile golang tag (mode=${MODE})"

GOMOD="${ROOT}/go.mod"
DOCKERFILE="${ROOT}/Dockerfile"

if [[ ! -f "$GOMOD" ]]; then
  echo "FAIL: go.mod not found at ${GOMOD}"
  echo "DONE: ok=false reason=missing-go-mod"
  exit 1
fi
if [[ ! -f "$DOCKERFILE" ]]; then
  echo "FAIL: Dockerfile not found at ${DOCKERFILE}"
  echo "DONE: ok=false reason=missing-dockerfile"
  exit 1
fi

extract_go_mod_version() {
  # First `go X.Y` or `go X.Y.Z` directive (not toolchain).
  local line
  line="$(grep -E '^go [0-9]+\.[0-9]+(\.[0-9]+)?[[:space:]]*$' "$1" | head -n 1 || true)"
  if [[ -z "$line" ]]; then
    echo ""
    return 0
  fi
  echo "${line#go }"
}

extract_dockerfile_version() {
  # First official golang:X.Y.Z image (digest suffix optional).
  local line
  line="$(grep -E 'golang:[0-9]+\.[0-9]+' "$1" | head -n 1 || true)"
  if [[ -z "$line" ]]; then
    echo ""
    return 0
  fi
  echo "$line" | sed -E 's/.*golang:([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/'
}

echo "DO: read versions from ${GOMOD} and ${DOCKERFILE}"
GO_MOD_VER="$(extract_go_mod_version "$GOMOD")"
DOCKER_VER="$(extract_dockerfile_version "$DOCKERFILE")"

if [[ -z "$GO_MOD_VER" ]]; then
  echo "FAIL: no go X.Y.Z directive in ${GOMOD}"
  echo "DONE: ok=false reason=unparseable-go-mod"
  exit 1
fi
if [[ -z "$DOCKER_VER" ]]; then
  echo "FAIL: no golang:X.Y.Z image in ${DOCKERFILE}"
  echo "DONE: ok=false reason=unparseable-dockerfile"
  exit 1
fi

echo "OK: go.mod=${GO_MOD_VER} dockerfile=${DOCKER_VER}"

if [[ "$GO_MOD_VER" == "$DOCKER_VER" ]]; then
  echo "DONE: ok=true synced=true go=${GO_MOD_VER}"
  echo "NEXT: none"
  exit 0
fi

if [[ "$MODE" != "write" ]]; then
  echo "FAIL: go.mod go ${GO_MOD_VER} != Dockerfile golang:${DOCKER_VER}"
  echo "HINT: run 'bash hack/verify-go-version-sync.sh --write' or let the Dependabot auto-merge workflow sync docker PRs."
  echo "DONE: ok=false reason=mismatch go_mod=${GO_MOD_VER} dockerfile=${DOCKER_VER}"
  echo "NEXT: bump the go.mod go directive to ${DOCKER_VER} (or the Dockerfile tag to ${GO_MOD_VER})"
  exit 1
fi

echo "DO: write go ${DOCKER_VER} into ${GOMOD}"
tmp="$(mktemp)"
# Replace only the first `go X.Y[.Z]` directive. Leave toolchain and comments.
awk -v ver="$DOCKER_VER" '
  BEGIN { done = 0 }
  /^go [0-9]+\.[0-9]+(\.[0-9]+)?[[:space:]]*$/ && !done {
    print "go " ver
    done = 1
    next
  }
  { print }
' "$GOMOD" > "$tmp"
mv "$tmp" "$GOMOD"

echo "OK: updated go.mod to go ${DOCKER_VER}"
echo "DONE: ok=true synced=false wrote=true go=${DOCKER_VER}"
echo "NEXT: commit go.mod"
exit 0
