#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/rebase-outdated-dependabot.sh and the
# rebase-outdated job. Locks: no @dependabot rebase comments, App-token
# git rebase + force-with-lease, behind-only selection.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/rebase-outdated-dependabot.sh"
WORKFLOW="${ROOT}/.github/workflows/dependabot-auto-merge.yaml"
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

assert_not_contains() {
  local needle="$1" haystack="$2" name="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "FAIL: $name: unexpectedly contains '${needle}'" >&2
    exit 1
  fi
  echo "ok: $name"
}

echo "PLAN: lock Dependabot rebase-outdated to App-token git rebase"

[[ -f "${SCRIPT}" ]] || { echo "FAIL: missing ${SCRIPT}" >&2; exit 1; }
[[ -f "${WORKFLOW}" ]] || { echo "FAIL: missing ${WORKFLOW}" >&2; exit 1; }
[[ -x "${SCRIPT}" ]] || chmod +x "${SCRIPT}"

echo "DO: workflow must not comment @dependabot rebase"
wf="$(cat "${WORKFLOW}")"
wf_code="$(grep -vE '^\s*#' "${WORKFLOW}")"
assert_not_contains 'gh pr comment' "$wf_code" "workflow has no gh pr comment"
assert_not_contains '@dependabot rebase' "$wf_code" "workflow run steps omit @dependabot rebase"
assert_contains 'hack/rebase-outdated-dependabot.sh' "$wf" "workflow calls rebase script"
assert_contains 'permission-contents: write' "$wf" "App token can push"
assert_contains 'permission-workflows: write' "$wf" "App token can push workflow files"
assert_contains 'persist-credentials: false' "$wf" "checkout does not keep GITHUB_TOKEN remote"

echo "DO: script never asks Dependabot to rebase"
src="$(cat "${SCRIPT}")"
assert_not_contains 'gh pr comment' "$src" "script has no gh pr comment"
# Header may mention the rejected command; the executable path must not
# invoke it. Strip comments and re-check.
code="$(grep -vE '^\s*#' "${SCRIPT}")"
assert_not_contains '@dependabot rebase' "$code" "executable lines omit @dependabot rebase"
assert_contains '--force-with-lease=' "$src" "script pins expected SHA on push"

echo "DO: missing REPO fails closed"
set +e
out="$("${SCRIPT}" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "missing REPO exit 1"
assert_contains "REPO is required" "$out" "missing REPO message"

echo "DO: missing GH_TOKEN fails closed unless dry-run"
set +e
out="$(REPO=attune-io/attune "${SCRIPT}" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "missing GH_TOKEN exit 1"
assert_contains "GH_TOKEN is required" "$out" "missing GH_TOKEN message"

# Fake gh: prints Dependabot PR numbers and mergeable_state / head.ref.
write_fake_gh() {
  local dest="$1"
  cat >"${dest}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
jq_filter=""
path=""
prev=""
for a in "$@"; do
  if [[ "${prev}" == "--jq" ]]; then
    jq_filter="${a}"
  elif [[ "${a}" == repos/* ]]; then
    path="${a}"
  fi
  prev="${a}"
done
state_file="${FAKE_GH_STATE:-/tmp/fake-gh-state}"
case "${path}" in
  *'/pulls?'*|*'pulls?state='*)
    if [[ "${jq_filter}" == *number* && -n "${FAKE_PR_NUMS:-}" ]]; then
      # shellcheck disable=SC2086
      printf '%s\n' ${FAKE_PR_NUMS}
    fi
    ;;
  *'/pulls/'*)
    if [[ "${jq_filter}" == *mergeable_state* ]]; then
      if [[ "${FAKE_STATE:-}" == "unknown-then-behind" ]]; then
        if [[ -f "${state_file}" ]]; then
          echo behind
        else
          echo unknown >"${state_file}"
          echo unknown
        fi
      else
        echo "${FAKE_STATE:-behind}"
      fi
    elif [[ "${jq_filter}" == *head.ref* ]]; then
      echo "${FAKE_BRANCH:-dependabot/go_modules/foo}"
    fi
    ;;
esac
EOF
  chmod +x "${dest}"
}

echo "DO: dry-run with no Dependabot PRs"
bins="${tmpdir}/bins-empty"
mkdir -p "${bins}"
write_fake_gh "${bins}/gh"
FAKE_PR_NUMS=""
out="$(PATH="${bins}:$PATH" REPO=attune-io/attune GH="${bins}/gh" \
  "${SCRIPT}" --dry-run)"
assert_contains "OK: no open Dependabot PRs" "$out" "empty list message"
assert_contains "DONE: asked=0 rebased=0 failed=0" "$out" "empty list counts"

echo "DO: dry-run rebases behind, skips clean"
bins="${tmpdir}/bins-mix"
mkdir -p "${bins}"
write_fake_gh "${bins}/gh"
# Two PRs: first behind (default), second we cannot easily vary per-number
# in this fake. Separate runs instead.
FAKE_PR_NUMS="7"
FAKE_STATE=behind
FAKE_BRANCH=dependabot/github_actions/actions-abc
out="$(PATH="${bins}:$PATH" REPO=attune-io/attune GH="${bins}/gh" \
  FAKE_PR_NUMS=7 FAKE_STATE=behind FAKE_BRANCH=dependabot/github_actions/actions-abc \
  "${SCRIPT}" --dry-run)"
assert_contains "OK: #7 mergeable_state=behind" "$out" "behind state logged"
assert_contains "DO: rebase #7" "$out" "behind is selected"
assert_contains "OK: dry-run skip push #7" "$out" "dry-run skips push"
assert_contains "DONE: asked=1 rebased=1 failed=0" "$out" "behind counts"

FAKE_STATE=clean
out="$(PATH="${bins}:$PATH" REPO=attune-io/attune GH="${bins}/gh" \
  FAKE_PR_NUMS=8 FAKE_STATE=clean \
  "${SCRIPT}" --dry-run)"
assert_contains "OK: #8 mergeable_state=clean" "$out" "clean state logged"
assert_not_contains "DO: rebase #8" "$out" "clean is not selected"
assert_contains "DONE: asked=0 rebased=0 failed=0" "$out" "clean counts"

echo "DO: unknown then behind is selected after retry"
statef="${tmpdir}/unknown-state"
rm -f "${statef}"
out="$(PATH="${bins}:$PATH" REPO=attune-io/attune GH="${bins}/gh" \
  FAKE_PR_NUMS=9 FAKE_STATE=unknown-then-behind FAKE_GH_STATE="${statef}" \
  FAKE_BRANCH=dependabot/go_modules/bar \
  "${SCRIPT}" --dry-run)"
assert_contains "OK: #9 mergeable_state=behind" "$out" "unknown retried to behind"
assert_contains "DO: rebase #9" "$out" "retried behind is selected"

echo "DO: real git rebase + force-with-lease on a behind branch"
origin="${tmpdir}/origin.git"
work="${tmpdir}/work"
"${GIT:-git}" init --bare "${origin}" >/dev/null
"${GIT:-git}" clone "${origin}" "${work}" >/dev/null 2>&1
git -C "${work}" config user.name testdep
git -C "${work}" config user.email testdep@example.com
printf 'base\n' >"${work}/README"
git -C "${work}" add README
git -C "${work}" commit -m "base" >/dev/null
git -C "${work}" branch -M main
git -C "${work}" checkout -b dependabot/go_modules/foo >/dev/null 2>&1
printf 'dep\n' >>"${work}/dep.txt"
git -C "${work}" add dep.txt
git -C "${work}" commit -m "chore(deps): bump foo" >/dev/null
git -C "${work}" checkout main >/dev/null 2>&1
printf 'main2\n' >>"${work}/README"
git -C "${work}" add README
git -C "${work}" commit -m "main moves" >/dev/null
git -C "${work}" push -u origin main >/dev/null 2>&1
git -C "${work}" push origin dependabot/go_modules/foo >/dev/null 2>&1

# Script must run inside a clone that has origin.
clone="${tmpdir}/clone"
git clone "${origin}" "${clone}" >/dev/null 2>&1
git -C "${clone}" fetch origin dependabot/go_modules/foo >/dev/null 2>&1
old="$(git -C "${clone}" rev-parse origin/dependabot/go_modules/foo)"

bins="${tmpdir}/bins-git"
mkdir -p "${bins}"
write_fake_gh "${bins}/gh"
(
  cd "${clone}"
  PATH="${bins}:$PATH" REPO=attune-io/attune GH_TOKEN=test-token \
    GH="${bins}/gh" GIT=git PUSH_URL="${origin}" \
    FAKE_PR_NUMS=3 FAKE_STATE=behind FAKE_BRANCH=dependabot/go_modules/foo \
    "${SCRIPT}"
) >"${tmpdir}/rebase.out"
out="$(cat "${tmpdir}/rebase.out")"
assert_contains "OK: pushed rebase of #3" "$out" "live rebase pushed"
git -C "${clone}" fetch origin dependabot/go_modules/foo >/dev/null 2>&1
new="$(git -C "${clone}" rev-parse origin/dependabot/go_modules/foo)"
if [[ "${old}" == "${new}" ]]; then
  echo "FAIL: branch SHA did not move after rebase" >&2
  exit 1
fi
# Rebased branch must contain main's latest commit.
if ! git -C "${clone}" merge-base --is-ancestor origin/main origin/dependabot/go_modules/foo; then
  echo "FAIL: rebased branch is not based on origin/main" >&2
  exit 1
fi
echo "ok: rebased branch contains origin/main"

echo "DONE: ok=true"
exit 0
