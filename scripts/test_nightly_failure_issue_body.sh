#!/usr/bin/env bash
# Unit tests for scripts/nightly-failure-issue-body.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/nightly-failure-issue-body.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/art"
printf '%s\n' '--- FAIL: TestE2E_NodeMemoryPressure_SkipsMemoryIncrease (1s)' >"$TMP/art/go-e2e.log"
# Passing variant sorts first; the reporter must still surface the failing cell.
printf '%s\n' 'Tests Summary...' '- Failed  tests 0' >"$TMP/art/chainsaw-v1.32.log"
printf '%s\n' '--- FAIL: chainsaw/configmap-export (200.33s)' 'Tests Summary...' '- Failed  tests 1' \
  >"$TMP/art/chainsaw-v1.34.log"

body=$("$SCRIPT" \
  --run-url 'https://example.com/actions/runs/99' \
  --e2e-result failure \
  --fuzz-result success \
  --artifact-dir "$TMP/art")

echo "$body" | grep -q 'E2E result: `failure`'
echo "$body" | grep -q 'Fuzz result: `success`'
echo "$body" | grep -q 'TestE2E_NodeMemoryPressure_SkipsMemoryIncrease'
echo "$body" | grep -q 'https://example.com/actions/runs/99'
echo "$body" | grep -q 'configmap-export'
echo "$body" | grep -q 'Failed  tests 1'

# Minimal path without artifacts still works
body2=$("$SCRIPT" \
  --run-url 'https://example.com/r/1' \
  --e2e-result success \
  --fuzz-result failure)
echo "$body2" | grep -q 'Fuzz result: `failure`'
echo "$body2" | grep -q 'Check the workflow run'

echo "OK: nightly-failure-issue-body tests passed"
