#!/usr/bin/env bash
# A draft marked ready does not push. ready_for_review must run auto-approve.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WF="${ROOT}/.github/workflows/auto-approve.yaml"

echo "DO: auto-approve runs on ready_for_review"
grep -q 'ready_for_review' "${WF}" || {
  echo "FAIL: auto-approve.yaml has no ready_for_review trigger"
  exit 1
}
echo "OK: ready_for_review is a trigger"
echo "DONE: ok=true"
