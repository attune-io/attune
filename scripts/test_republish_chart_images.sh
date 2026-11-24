#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# The republish helper rewrites a local chart to the rendered image,
# falls back when that tag was never published, and does not push
# unless --push is set.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/republish-chart-images.sh"
WF="${ROOT}/.github/workflows/republish-chart-images.yaml"

echo "PLAN: classify republish-chart-images.sh"

python3 "${ROOT}/scripts/test_repoint_chart_image.py"
echo "OK: annotation rewrite unit tests"

if ! command -v helm >/dev/null 2>&1; then
  echo "FAIL: helm is required"
  echo "DONE: ok=false reason=helm-missing"
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
cp -R "${ROOT}/charts/attune" "${TMP}/attune"
python3 - "${TMP}/attune/Chart.yaml" << 'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = text.replace("ghcr.io/attune-io/attune:0.1.39", "ghcr.io/attune-io/attune:latest")
path.write_text(text)
PY

# Invoked through EXISTS_CMD, not by a direct call.
# shellcheck disable=SC2329
exists_all() { return 0; }
export -f exists_all
echo "DO: rendered bare tag replaces :latest and does not push"
set +e
EXISTS_CMD=exists_all bash "$SCRIPT" --chart "${TMP}/attune" > "${TMP}/plan.txt" 2>&1
status=$?
set -e
if [[ "$status" -ne 0 ]]; then
  echo "FAIL: plan mode exited ${status}"
  cat "${TMP}/plan.txt"
  echo "DONE: ok=false reason=plan-failed"
  exit 1
fi
if grep -q 'helm push' "${TMP}/plan.txt"; then
  echo "FAIL: plan mode invoked helm push"
  echo "DONE: ok=false reason=pushed-without-flag"
  exit 1
fi
if grep -q 'attune:latest' "${TMP}/attune/Chart.yaml"; then
  echo "FAIL: :latest annotation remained"
  echo "DONE: ok=false reason=latest-remained"
  exit 1
fi
if ! grep -q 'ghcr.io/attune-io/attune:0.1.39' "${TMP}/attune/Chart.yaml"; then
  echo "FAIL: annotation was not pinned to the rendered 0.1.39 tag"
  echo "DONE: ok=false reason=missing-rendered-tag"
  exit 1
fi
echo "OK: plan rewrote :latest to 0.1.39 without pushing"

echo "DO: missing bare tag falls back to the published v tag"
cp -R "${ROOT}/charts/attune" "${TMP}/fallback"
# Invoked through EXISTS_CMD, not by a direct call.
# shellcheck disable=SC2329
exists_v_only() {
  [[ "$1" == "v0.1.39" ]]
}
export -f exists_v_only
set +e
EXISTS_CMD=exists_v_only bash "$SCRIPT" --chart "${TMP}/fallback" > "${TMP}/fallback.txt" 2>&1
status=$?
set -e
if [[ "$status" -ne 0 ]]; then
  echo "FAIL: fallback plan exited ${status}"
  cat "${TMP}/fallback.txt"
  echo "DONE: ok=false reason=fallback-failed"
  exit 1
fi
if ! grep -q 'ghcr.io/attune-io/attune:v0.1.39' "${TMP}/fallback/Chart.yaml"; then
  echo "FAIL: missing bare tag did not fall back to v0.1.39"
  cat "${TMP}/fallback.txt"
  echo "DONE: ok=false reason=no-v-fallback"
  exit 1
fi
echo "OK: fallback selected v0.1.39"

echo "DO: --push records helm push and cosign sign, and does not run them"
RECORD="${TMP}/record"
mkdir -p "$RECORD"
export REPUBLISH_RECORD_DIR="$RECORD"
export EXISTS_CMD=exists_all
cp -R "${ROOT}/charts/attune" "${TMP}/push"
python3 - "${TMP}/push/Chart.yaml" << 'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = text.replace("ghcr.io/attune-io/attune:0.1.39", "ghcr.io/attune-io/attune:latest")
path.write_text(text)
PY
set +e
bash "$SCRIPT" --chart "${TMP}/push" --push > "${TMP}/push.txt" 2>&1
status=$?
set -e
if [[ "$status" -ne 0 ]]; then
  echo "FAIL: recorded push exited ${status}"
  cat "${TMP}/push.txt"
  echo "DONE: ok=false reason=push-record-failed"
  exit 1
fi
if ! grep -q 'helm push' "${RECORD}/commands"; then
  echo "FAIL: --push did not record helm push"
  echo "DONE: ok=false reason=push-not-recorded"
  exit 1
fi
if ! grep -q 'cosign sign --yes ghcr.io/attune-io/charts/attune:' "${RECORD}/commands"; then
  echo "FAIL: --push did not record the GHCR signature"
  echo "DONE: ok=false reason=ghcr-sign-missing"
  exit 1
fi
if ! grep -q 'cosign sign --yes docker.io/attuneio/attune-chart:' "${RECORD}/commands"; then
  echo "FAIL: --push did not record the Docker Hub signature"
  echo "DONE: ok=false reason=docker-sign-missing"
  exit 1
fi
echo "OK: push path records push and both signatures"

echo "DO: an already correct chart is signed again and not repackaged"
RECORD2="${TMP}/record-unchanged"
mkdir -p "$RECORD2"
export REPUBLISH_RECORD_DIR="$RECORD2"
cp -R "${ROOT}/charts/attune" "${TMP}/unchanged"
set +e
bash "$SCRIPT" --chart "${TMP}/unchanged" --push > "${TMP}/unchanged.txt" 2>&1
status=$?
set -e
if [[ "$status" -ne 0 ]]; then
  echo "FAIL: unchanged push exited ${status}"
  cat "${TMP}/unchanged.txt"
  echo "DONE: ok=false reason=unchanged-push-failed"
  exit 1
fi
if grep -q 'helm push' "${RECORD2}/commands"; then
  echo "FAIL: unchanged chart was repackaged"
  echo "DONE: ok=false reason=unchanged-repackaged"
  exit 1
fi
if ! grep -q 'cosign sign --yes ghcr.io/attune-io/charts/attune:' "${RECORD2}/commands"; then
  echo "FAIL: unchanged chart was not signed"
  echo "DONE: ok=false reason=unchanged-not-signed"
  exit 1
fi
echo "OK: unchanged chart is signed without a new push"

echo "DO: a registry check error does not fall back to another tag"
cp -R "${ROOT}/charts/attune" "${TMP}/boom"
python3 - "${TMP}/boom/Chart.yaml" << 'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = text.replace("ghcr.io/attune-io/attune:0.1.39", "ghcr.io/attune-io/attune:latest")
path.write_text(text)
PY
# Invoked through EXISTS_CMD, not by a direct call.
# shellcheck disable=SC2329
exists_boom() { echo "registry unavailable" >&2; return 2; }
export -f exists_boom
set +e
EXISTS_CMD=exists_boom bash "$SCRIPT" --chart "${TMP}/boom" > "${TMP}/boom.txt" 2>&1
status=$?
set -e
if [[ "$status" -eq 0 ]]; then
  echo "FAIL: registry error was treated as a successful plan"
  cat "${TMP}/boom.txt"
  echo "DONE: ok=false reason=registry-error-ignored"
  exit 1
fi
if ! grep -q 'attune:latest' "${TMP}/boom/Chart.yaml"; then
  echo "FAIL: registry error rewrote the annotation"
  echo "DONE: ok=false reason=registry-error-rewrote"
  exit 1
fi
echo "OK: registry error left the chart unchanged"

echo "DO: workflow is dispatch-only and calls the helper"
# A push trigger sits directly under on:. The push input is nested deeper.
if awk '
  /^on:/ {on=1; next}
  on && /^[^ #[:space:]]/ {on=0}
  on && /^  push:/ {bad=1}
  END {exit bad ? 0 : 1}
' "$WF"; then
  echo "FAIL: republish workflow runs on push"
  echo "DONE: ok=false reason=workflow-on-push"
  exit 1
fi
if ! grep -q 'workflow_dispatch:' "$WF"; then
  echo "FAIL: republish workflow has no workflow_dispatch"
  echo "DONE: ok=false reason=no-dispatch"
  exit 1
fi
if ! grep -q 'hack/republish-chart-images.sh' "$WF"; then
  echo "FAIL: workflow does not call hack/republish-chart-images.sh"
  echo "DONE: ok=false reason=workflow-missing-script"
  exit 1
fi
echo "OK: workflow is a manual republish"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
