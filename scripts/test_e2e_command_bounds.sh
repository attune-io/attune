#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier: docker and k3d commands that can block in the daemon must
# run under hack/bounded-run.sh, or under a helper that does.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "PLAN: classify outer bounds on docker and k3d commands"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

python3 - "${ROOT}" <<'PY'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1])
e2e = re.compile(
    r"\b(?:docker\s+(?:pull|save|load|exec|tag|inspect|builder\s+prune|system\s+prune)"
    r"|k3d\s+cluster\s+(?:create|list)|k3d\s+kubeconfig)\b"
)
mirror = re.compile(r"\bdocker\s+(?:pull|push|tag)\b")
security = re.compile(r"\bdocker\s+(?:buildx\s+build|build|save)\b")
local = re.compile(
    r"\b(?:k3d\s+image\s+import|k3d\s+cluster\s+delete|kind\s+load\s+image-archive)\b"
)
files = {
    ".github/workflows/e2e-nightly.yaml": e2e,
    ".github/actions/setup-e2e-cluster/action.yaml": e2e,
    ".github/workflows/mirror-e2e-images.yaml": mirror,
    ".github/workflows/security.yaml": security,
    "hack/demo.sh": local,
    "Makefile": local,
}
allow = ("bounded-run.sh", "k3d-image-import.sh", "k3d-delete.sh")
bad = []
for rel, pat in files.items():
    path = root / rel
    for n, line in enumerate(path.read_text().splitlines(), 1):
        code = line.split("#", 1)[0].strip()
        if code.startswith("echo ") or code.startswith("warn "):
            continue
        if not pat.search(code):
            continue
        if any(token in code for token in allow):
            continue
        bad.append(f"{rel}:{n}:{line.strip()}")
verify = (root / "hack/e2e-verify-resize-subresource.sh").read_text().splitlines()
if not any("bounded-run.sh" in line and "cluster create" in line for line in verify):
    bad.append("hack/e2e-verify-resize-subresource.sh: cluster create is not under bounded-run.sh")
# A sparse checkout that calls the helper must list the helper file.
# The image mirror only fetched ci.yaml, so the push to main failed
# with "hack/bounded-run.sh: No such file or directory" (run 42403006288).
for rel in (".github/workflows/mirror-e2e-images.yaml",):
    lines = (root / rel).read_text().splitlines()
    calls_helper = any("bounded-run.sh" in line for line in lines)
    for i, line in enumerate(lines):
        if "sparse-checkout:" not in line or "cone-mode" in line:
            continue
        indent = len(line) - len(line.lstrip())
        block = [line]
        for nxt in lines[i + 1 :]:
            if nxt.strip() == "":
                continue
            if len(nxt) - len(nxt.lstrip()) <= indent:
                break
            block.append(nxt)
        if calls_helper and not any("hack/bounded-run.sh" in item for item in block):
            bad.append(f"{rel}: sparse-checkout does not include hack/bounded-run.sh")
nightly = (root / ".github/workflows/e2e-nightly.yaml").read_text()
if re.search(r"bounded-run\.sh[^\n]*\\(?:\n[^\n]*)*\|[^\n]*\bjq\b", nightly):
    bad.append("e2e-nightly.yaml pipes bounded-run.sh output to jq")
if bad:
    print("\n".join(bad))
    sys.exit(1)
print("OK: watched commands are bounded")
PY

echo "PLAN: duration forms"
# shellcheck disable=SC1091
source "${ROOT}/hack/bounded-run.sh"
check_dur() {
  local got
  got="$(bounded_duration_secs "$1")"
  if [[ "${got}" != "$2" ]]; then
    fail "duration $1 want $2 got ${got}"
  fi
}
check_dur 1h 3600
check_dur 2.5m 150
check_dur 1.08m 64
check_dur 1.010m 60
check_dur 0.9s 1
check_dur 08 8
check_dur 5m 300
check_dur 30s 30
check_dur 90 90
if bounded_duration_secs 1d >/dev/null 2>"${TMPDIR:-/tmp}/bounded-bad-duration"; then
  fail "1d should be rejected"
fi
if ! grep -q "bad duration" "${TMPDIR:-/tmp}/bounded-bad-duration"; then
  fail "1d did not print bad duration"
fi

echo "PLAN: SIGINT and SIGTERM stop the child"
python3 - "${ROOT}" <<'PY'
import os, signal, subprocess, sys, time
root = sys.argv[1]
script = os.path.join(root, "hack", "bounded-run.sh")

def run(sig, want):
    proc = subprocess.Popen(
        ["bash", script, "30", "--", "sleep", "30"],
        start_new_session=True,
    )
    time.sleep(0.3)
    os.kill(proc.pid, sig)
    try:
        code = proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        os.kill(proc.pid, signal.SIGKILL)
        proc.wait(timeout=5)
        raise SystemExit(f"FAIL: signal {sig} did not stop bounded-run")
    if code != want:
        raise SystemExit(f"FAIL: signal {sig} exit want {want} got {code}")
    # The sleep must not outlive the wrapper.
    time.sleep(0.2)

run(signal.SIGINT, 130)
run(signal.SIGTERM, 143)
print("OK: signals stop the child")

print("PLAN: the wrapper stays the terminal foreground group")
import fcntl, pty, struct, termios
pid, fd = pty.fork()
if pid == 0:
    os.execv("/bin/bash", ["bash", script, "30", "--", "sleep", "30"])
time.sleep(0.5)
tpgid = struct.unpack("i", fcntl.ioctl(fd, termios.TIOCGPGRP, b"\0"*4))[0]
if tpgid != pid:
    os.kill(pid, signal.SIGKILL)
    os.waitpid(pid, 0)
    raise SystemExit(f"FAIL: terminal foreground group is {tpgid}, wrapper is {pid}")
os.killpg(tpgid, signal.SIGINT)
deadline = time.time() + 4
while time.time() < deadline:
    wpid, status = os.waitpid(pid, os.WNOHANG)
    if wpid == pid:
        code = os.waitstatus_to_exitcode(status)
        if code != 130:
            raise SystemExit(f"FAIL: foreground SIGINT exit want 130 got {code}")
        print("OK: foreground SIGINT stops the child")
        raise SystemExit(0)
    time.sleep(0.1)
os.kill(pid, signal.SIGKILL)
os.waitpid(pid, 0)
raise SystemExit("FAIL: foreground SIGINT did not stop bounded-run")
PY

echo "DONE: ok=true"
echo "NEXT: none"
