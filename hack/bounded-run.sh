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
# DURATION is seconds, or a number with an s, m, or h suffix (30s, 5m, 1h).
# A fractional suffix is allowed (2.5m). 1d and other units are rejected.
# Exit status is the command status, or 124 when the bound fires.
# SIGINT exits 130 and SIGTERM exits 143, after the child group is signaled.
# BOUNDED_KILL_AFTER_SEC (default 15) is the pause between TERM and KILL.
# BOUNDED_ABANDON_SEC (default 2) is how long to notice that KILL did
# not reap the child before this process returns anyway.

bounded_duration_secs() {
  local v="$1"
  if [[ ! "${v}" =~ ^([0-9]+)(\.([0-9]+))?(s|m|h)?$ ]]; then
    echo "bounded-run: bad duration ${v}" >&2
    return 2
  fi
  # 10# forces decimal. Bash otherwise treats a leading zero as octal,
  # so 1.08m and 08s abort or parse as the wrong number of seconds.
  local whole=$((10#${BASH_REMATCH[1]}))
  local frac_raw="${BASH_REMATCH[3]}"
  local unit="${BASH_REMATCH[4]}"
  local mult=1
  case "${unit}" in
    "" | s) mult=1 ;;
    m) mult=60 ;;
    h) mult=3600 ;;
  esac
  if [[ -z "${frac_raw}" ]]; then
    echo $((whole * mult))
    return 0
  fi
  local frac=$((10#${frac_raw}))
  local scale=1
  local n=${#frac_raw}
  while [[ "${n}" -gt 0 ]]; do
    scale=$((scale * 10))
    n=$((n - 1))
  done
  local result=$((whole * mult + frac * mult / scale))
  # 0.9s truncates to 0 and would kill the command on the first poll.
  if [[ "${result}" -eq 0 && "${frac}" -ne 0 ]]; then
    result=1
  fi
  echo "${result}"
}

# A foreground sleep becomes the terminal's foreground group after
# monitor mode, so Ctrl-C never reaches this script. A background
# sleep is not that group. The shell stays in the foreground and the
# INT trap runs.
bounded_pause() {
  sleep 0.2 &
  wait "$!" || true
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
  # Monitor mode also makes a later foreground sleep the terminal's
  # foreground group, so Ctrl-C would hit that sleep and miss this trap.
  # Turn it off once the child has its own group.
  set -m
  "$@" &
  local pid=$!
  set +m
  # The child is not in this shell's foreground group, so Ctrl-C and a
  # cancelled job would otherwise leave docker or k3d running.
  trap 'bounded_kill_group TERM "${pid}"; bounded_kill_group KILL "${pid}"; trap - INT TERM; exit 130' INT
  trap 'bounded_kill_group TERM "${pid}"; bounded_kill_group KILL "${pid}"; trap - INT TERM; exit 143' TERM
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
          trap - INT TERM
          return 124
        fi
        bounded_pause
        polls=$((polls + 1))
      done
      # Still alive after KILL. Returning reparents a D-state child
      # instead of waiting for the Docker syscall to finish.
      trap - INT TERM
      return 124
    fi
    bounded_pause
  done
  local status=0
  wait "${pid}" || status=$?
  trap - INT TERM
  if [[ "${signaled}" -eq 1 ]]; then
    return 124
  fi
  return "${status}"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  set -euo pipefail
  bounded_run "$@"
fi
