#!/usr/bin/env bash
# Build an enriched body for e2e-nightly failure issues (#484).
# Best-effort: never exits non-zero solely because enrichment failed.
#
# Usage:
#   scripts/nightly-failure-issue-body.sh \
#     --run-url URL --run-id ID --repo OWNER/REPO \
#     --e2e-result RESULT --fuzz-result RESULT \
#     [--artifact-dir DIR]
#
# Env: GH_TOKEN for gh api (optional for artifact-only mode)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RUN_URL=""
RUN_ID=""
REPO=""
E2E_RESULT=""
FUZZ_RESULT=""
ARTIFACT_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run-url) RUN_URL="${2:-}"; shift 2 ;;
    --run-id) RUN_ID="${2:-}"; shift 2 ;;
    --repo) REPO="${2:-}"; shift 2 ;;
    --e2e-result) E2E_RESULT="${2:-}"; shift 2 ;;
    --fuzz-result) FUZZ_RESULT="${2:-}"; shift 2 ;;
    --artifact-dir) ARTIFACT_DIR="${2:-}"; shift 2 ;;
    -h|--help)
      sed -n '2,20p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

if [[ -z "$RUN_URL" || -z "$E2E_RESULT" || -z "$FUZZ_RESULT" ]]; then
  echo "required: --run-url, --e2e-result, --fuzz-result" >&2
  exit 2
fi

failed_jobs_md=""
if [[ -n "$RUN_ID" && -n "$REPO" ]] && command -v gh >/dev/null 2>&1; then
  # shellcheck disable=SC2016
  jobs_json=$(gh api "repos/${REPO}/actions/runs/${RUN_ID}/jobs" --paginate 2>/dev/null || true)
  if [[ -n "$jobs_json" ]]; then
    failed_jobs_md=$(printf '%s' "$jobs_json" | python3 "${SCRIPT_DIR}/nightly_failure_jobs.py" 2>/dev/null || true)
  fi
fi

fail_tests_md=""
if [[ -n "$ARTIFACT_DIR" && -d "$ARTIFACT_DIR" ]]; then
  # Cap at 5 unique Go test names across go-e2e logs
  mapfile -t fails < <(
    # shellcheck disable=SC2086
    find "$ARTIFACT_DIR" -type f \( -name 'go-e2e*.log' -o -name '*go-e2e*.log' -o -name 'go-e2e.log' \) 2>/dev/null \
      | while read -r f; do
          # Prefer --- FAIL: lines
          grep -E '^--- FAIL: ' "$f" 2>/dev/null || true
        done \
      | sed -E 's/^--- FAIL: ([^ ]+).*/\1/' \
      | awk 'NF && !seen[$0]++' \
      | head -5
  )
  if ((${#fails[@]} > 0)); then
    fail_tests_md=$(printf -- '- `%s`\n' "${fails[@]}")
  fi
  # Chainsaw summary: prefer a log that actually failed tests. find | head -1
  # is filesystem-order and can pick a passing matrix variant ("Failed tests 0")
  # while another variant failed (nightly #520).
  chainsaw_line=""
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    snippet=$(grep -E 'Failed[[:space:]]+tests[[:space:]]+[1-9]|--- FAIL: chainsaw/' "$f" 2>/dev/null | tail -8 || true)
    if [ -n "$snippet" ]; then
      chainsaw_line="$snippet"
      break
    fi
  done < <(find "$ARTIFACT_DIR" -type f -name 'chainsaw*.log' 2>/dev/null | sort)
  if [ -z "$chainsaw_line" ]; then
    first=$(find "$ARTIFACT_DIR" -type f -name 'chainsaw*.log' 2>/dev/null | sort | head -1)
    if [ -n "$first" ]; then
      chainsaw_line=$(grep -E 'Failed[[:space:]]+tests|Tests Summary|--- FAIL:' "$first" 2>/dev/null | tail -8 || true)
    fi
  fi
fi

{
  echo "The [nightly run](${RUN_URL}) failed."
  echo
  echo "- E2E result: \`${E2E_RESULT}\`"
  echo "- Fuzz result: \`${FUZZ_RESULT}\`"
  if [[ -n "$failed_jobs_md" ]]; then
    echo
    echo "### Failed jobs"
    echo
    echo "$failed_jobs_md"
  fi
  if [[ -n "$fail_tests_md" ]]; then
    echo
    echo "### Failing Go E2E tests (first matches)"
    echo
    printf '%s' "$fail_tests_md"
  fi
  if [[ -n "${chainsaw_line:-}" ]]; then
    echo
    echo "### Chainsaw log hints"
    echo
    echo '```'
    echo "$chainsaw_line"
    echo '```'
  fi
  echo
  echo "Check the workflow run and artifacts for full logs."
} 
