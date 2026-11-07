#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Run one command and return inside the bound.
#
# GNU timeout waits for a child stuck in uninterruptible Docker I/O,
# because that child does not die on SIGKILL until the syscall returns.
# This helper signals the process group, then returns 124 if the leader
# is still alive, so the caller is not held until the job cap.
#
# Usage: bounded-run.sh DURATION -- command...
# DURATION is seconds, or a number with an s or m suffix (5m, 30s).
# Exit status is the command status, or 124 when the bound fires.
# BOUNDED_KILL_AFTER_SEC (default 15) is the pause between TERM and KILL.
# BOUNDED_ABANDON_SEC (default 2) is how long to notice that KILL did
# not reap the child before this process returns anyway.

bounded_duration_secs() {
  local v="$1"
  if [[ "${v}" =~ ^([0-9]+)m$ ]]; then
    echo $((BASH_REMATCH[1] * 60))
    return 0
  fi
  if [[ "${v}" =~ ^([0-9]+)s$ ]]; then
    echo "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "${v}" =~ ^[0-9]+$ ]]; then
    echo "${v}"
    return 0
  fi
  echo "bounded-run: bad duration ${v}" >&2
  return 2
}

bounded_kill_group() {
  local sig="$1"
  local pid="$2"
  # Group first, so children k3d spawned are signaled with the leader.
  kill "-${sig}" -- "-${pid}" 2>/dev/null || kill "-${sig}" "${pid}" 2>/dev/null || true
}

bounded_run() {
  if [[ $# -lt 2 || "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    echo "usage: bounded-run.sh DURATION -- command..." >&2
    return 2
  fi
  local secs kill_after abandon
  secs="$(bounded_duration_secs "$1")" || return 2
  shift
  if [[ "${1:-}" != "--" ]]; then
    echo "usage: bounded-run.sh DURATION -- command..." >&2
    return 2
  fi
  shift
  if [[ $# -lt 1 ]]; then
    echo "usage: bounded-run.sh DURATION -- command..." >&2
    return 2
  fi
  kill_after="${BOUNDED_KILL_AFTER_SEC:-15}"
  abandon="${BOUNDED_ABANDON_SEC:-2}"

  # A separate process group so one signal reaches docker and k3d children.
  set -m
  "$@" &
  local pid=$!
  local start now signaled term_at polls
  start=$(date +%s)
  signaled=0
  term_at=0
  while kill -0 "${pid}" 2>/dev/null; do
    now=$(date +%s)
    if [[ "${signaled}" -eq 0 && $((now - start)) -ge ${secs} ]]; then
      bounded_kill_group TERM "${pid}"
      signaled=1
      term_at=${now}
    elif [[ "${signaled}" -eq 1 && $((now - term_at)) -ge ${kill_after} ]]; then
      bounded_kill_group KILL "${pid}"
      polls=0
      while [[ "${polls}" -lt $((abandon * 5)) ]]; do
        if ! kill -0 "${pid}" 2>/dev/null; then
          wait "${pid}" 2>/dev/null || true
          return 124
        fi
        sleep 0.2
        polls=$((polls + 1))
      done
      # Still alive after KILL. Returning reparents a D-state child
      # instead of waiting for the Docker syscall to finish.
      return 124
    fi
    sleep 0.2
  done
  local status=0
  wait "${pid}" || status=$?
  if [[ "${signaled}" -eq 1 ]]; then
    return 124
  fi
  return "${status}"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  set -euo pipefail
  bounded_run "$@"
fi
