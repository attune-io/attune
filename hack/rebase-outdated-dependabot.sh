#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Rebase behind Dependabot PRs onto origin/main and force-push with an
# App token. Dependabot rejects `@dependabot rebase` from GitHub Apps
# ("only users with push access"). A GITHUB_TOKEN push would not start
# PR CI. App-token git rebase plus --force-with-lease is the working path.

set -euo pipefail

REPO="${REPO:-}"
GH_TOKEN="${GH_TOKEN:-}"
PUSH_URL="${PUSH_URL:-}"
DRY_RUN=0
GH="${GH:-gh}"
GIT="${GIT:-git}"

usage() {
  cat <<'EOF'
Usage: REPO=owner/repo GH_TOKEN=... rebase-outdated-dependabot.sh [--dry-run]

Rebase open Dependabot PRs whose mergeable_state is behind onto origin/main
and force-push with --force-with-lease. Does not ask Dependabot via comment.

--dry-run  Print the plan (list + mergeable_state) and skip git push.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "FAIL: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "${REPO}" ]]; then
  echo "FAIL: REPO is required (owner/repo)" >&2
  exit 1
fi
if [[ "${DRY_RUN}" -eq 0 && -z "${GH_TOKEN}" ]]; then
  echo "FAIL: GH_TOKEN is required unless --dry-run" >&2
  exit 1
fi
if [[ "${DRY_RUN}" -eq 0 && -z "${PUSH_URL}" ]]; then
  PUSH_URL="https://x-access-token:${GH_TOKEN}@github.com/${REPO}.git"
fi

should_rebase() {
  [[ "$1" == "behind" ]]
}

mergeable_state() {
  local n="$1"
  local state
  state="$("${GH}" api "repos/${REPO}/pulls/${n}" --jq .mergeable_state)"
  if [[ "${state}" == "unknown" ]]; then
    sleep 2
    state="$("${GH}" api "repos/${REPO}/pulls/${n}" --jq .mergeable_state)"
  fi
  printf '%s\n' "${state}"
}

echo "PLAN: rebase behind Dependabot PRs onto origin/main (App token push)"

mapfile -t nums < <("${GH}" api "repos/${REPO}/pulls?state=open&per_page=100" \
  --jq '.[] | select(.user.login=="dependabot[bot]") | .number')
if [[ ${#nums[@]} -eq 0 ]]; then
  echo "OK: no open Dependabot PRs"
  echo "DONE: asked=0 rebased=0 failed=0"
  exit 0
fi

asked=0
rebased=0
failed=0

for n in "${nums[@]}"; do
  state="$(mergeable_state "${n}")"
  echo "OK: #${n} mergeable_state=${state}"
  if ! should_rebase "${state}"; then
    continue
  fi
  asked=$((asked + 1))
  branch="$("${GH}" api "repos/${REPO}/pulls/${n}" --jq .head.ref)"
  echo "DO: rebase #${n} branch=${branch}"
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    echo "OK: dry-run skip push #${n}"
    rebased=$((rebased + 1))
    continue
  fi

  "${GIT}" fetch origin main
  "${GIT}" fetch origin "refs/heads/${branch}:refs/remotes/origin/${branch}"
  expected="$("${GIT}" rev-parse "origin/${branch}")"
  "${GIT}" checkout -B "${branch}" "origin/${branch}"
  "${GIT}" config user.name "attune-release-bot[bot]"
  "${GIT}" config user.email "3904235+attune-release-bot[bot]@users.noreply.github.com"
  if ! "${GIT}" rebase origin/main; then
    echo "FAIL: #${n} rebase conflict; aborting"
    "${GIT}" rebase --abort || true
    failed=$((failed + 1))
    continue
  fi
  "${GIT}" push --force-with-lease="refs/heads/${branch}:${expected}" \
    "${PUSH_URL}" \
    "HEAD:refs/heads/${branch}"
  echo "OK: pushed rebase of #${n} to ${branch}"
  rebased=$((rebased + 1))
done

echo "DONE: asked=${asked} rebased=${rebased} failed=${failed}"
if [[ "${failed}" -gt 0 ]]; then
  echo "NEXT: resolve conflicts or close the conflicting Dependabot PR"
  exit 1
fi
echo "NEXT: wait for PR CI; auto-merge squash when CLEAN"
exit 0
