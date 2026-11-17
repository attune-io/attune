#!/usr/bin/env bash
# wait_for runs "$@". A quoted kubectl line is one argv, so the shell
# looks for a program with that whole name and the demo stops.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEMO="${ROOT}/hack/demo.sh"

echo "DO: demo wait_for commands are separate arguments"
if grep -nE '^[[:space:]]*"kubectl ' "${DEMO}"; then
  echo "FAIL: wait_for command is one quoted string"
  exit 1
fi

# The function must invoke the command as "$@", not as a single string.
if ! grep -q 'while ! "\$@"' "${DEMO}"; then
  echo "FAIL: wait_for does not run \"\$@\""
  exit 1
fi

echo "DO: wait_for succeeds when the command is real arguments"
wait_for() {
  local msg="$1"; shift
  local timeout="${1:-120}"; shift || true
  "$@"
}
wait_for "probe" 5 true
echo "OK: wait_for ran true"
echo "DONE: ok=true"
