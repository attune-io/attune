#!/usr/bin/env bash
# Unit tests for hack/verify-go-version-sync.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/verify-go-version-sync.sh"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

assert_eq() {
  local want="$1" got="$2" name="$3"
  if [[ "$want" != "$got" ]]; then
    echo "FAIL: $name: want=$want got=$got" >&2
    exit 1
  fi
  echo "ok: $name"
}

assert_contains() {
  local needle="$1" haystack="$2" name="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "FAIL: $name: missing '${needle}' in: ${haystack}" >&2
    exit 1
  fi
  echo "ok: $name"
}

write_pair() {
  local dir="$1" go_ver="$2" docker_ver="$3"
  mkdir -p "$dir"
  printf 'module example.com/x\n\ngo %s\n' "$go_ver" >"${dir}/go.mod"
  printf 'FROM --platform=$BUILDPLATFORM golang:%s@sha256:deadbeef AS builder\n' \
    "$docker_ver" >"${dir}/Dockerfile"
}

echo "PLAN: exercise verify-go-version-sync check/write modes"

# Matching versions: success.
pair="${tmpdir}/match"
write_pair "$pair" "1.26.6" "1.26.6"
out="$("$SCRIPT" --check --root "$pair")"
assert_contains "DONE: ok=true" "$out" "match prints ok=true"
"$SCRIPT" --check --root "$pair" >/dev/null
echo "ok: match exits 0"

# Mismatch: check fails, write updates go.mod.
pair="${tmpdir}/mismatch"
write_pair "$pair" "1.26.5" "1.26.6"
set +e
out="$("$SCRIPT" --check --root "$pair" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "mismatch check exit 1"
assert_contains "reason=mismatch" "$out" "mismatch names reason"
"$SCRIPT" --write --root "$pair" >/dev/null
got="$(grep -E '^go ' "${pair}/go.mod")"
assert_eq "go 1.26.6" "$got" "write updates go.mod"
"$SCRIPT" --check --root "$pair" >/dev/null
echo "ok: write then check exits 0"

# Missing Dockerfile.
pair="${tmpdir}/nodocker"
mkdir -p "$pair"
printf 'module example.com/x\n\ngo 1.26.6\n' >"${pair}/go.mod"
set +e
out="$("$SCRIPT" --check --root "$pair" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "missing Dockerfile exit 1"
assert_contains "missing-dockerfile" "$out" "missing Dockerfile reason"

# Unparseable Dockerfile.
pair="${tmpdir}/badimage"
mkdir -p "$pair"
printf 'module example.com/x\n\ngo 1.26.6\n' >"${pair}/go.mod"
printf 'FROM alpine:3.22\n' >"${pair}/Dockerfile"
set +e
out="$("$SCRIPT" --check --root "$pair" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "unparseable Dockerfile exit 1"
assert_contains "unparseable-dockerfile" "$out" "unparseable Dockerfile reason"

echo "DONE: verify-go-version-sync tests passed"
