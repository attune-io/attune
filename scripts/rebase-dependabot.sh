#!/usr/bin/env bash
#
# rebase-dependabot.sh
#
# Cleanly rebases a Dependabot branch onto latest main and force-pushes with lease.
# This is the supported way to "update" a Dependabot PR without introducing
# merge commits that trip DCO.
#
# Usage:
#   scripts/rebase-dependabot.sh origin/dependabot/go_modules/foo-abc123
#   scripts/rebase-dependabot.sh 348          # PR number (uses gh)
#
# Why this matters for Scorecard:
# - Preserves CI-Tests=10 (fresh runs after rebase)
# - Avoids weakening Branch-Protection or DCO hygiene
# - See AGENTS.md "Handling Dependabot PRs"
#
set -euo pipefail

TARGET="${1:-}"

if [[ -z "$TARGET" ]]; then
  echo "Usage: $0 <branch-ref-or-PR-number>"
  echo "  e.g. $0 origin/dependabot/..."
  echo "       $0 348"
  exit 1
fi

if [[ "$TARGET" =~ ^[0-9]+$ ]]; then
  # Treat as PR number
  echo "Resolving PR #$TARGET ..."
  BRANCH=$(gh pr view "$TARGET" --json headRefName --jq .headRefName)
  echo "Branch: $BRANCH"
else
  BRANCH="$TARGET"
fi

echo "Fetching..."
git fetch origin

LOCAL_BRANCH="rebase-dependabot-$(date +%s)"

echo "Checking out clean worktree for $BRANCH ..."
git checkout -B "$LOCAL_BRANCH" "origin/$BRANCH"

echo "Rebasing onto origin/main ..."
git rebase origin/main

echo "Pushing with --force-with-lease ..."
git push origin "HEAD:$BRANCH" --force-with-lease

echo "Done. New head pushed. Workflows will re-run with current definitions."
echo "Delete local branch with: git branch -D $LOCAL_BRANCH"
