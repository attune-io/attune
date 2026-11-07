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
    r"\b(?:docker\s+(?:pull|save|load|exec|builder\s+prune|system\s+prune)"
    r"|k3d\s+cluster\s+create)\b"
)
mirror = re.compile(r"\bdocker\s+(?:pull|push)\b")
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
if bad:
    print("\n".join(bad))
    sys.exit(1)
print("OK: watched commands are bounded")
PY

echo "DONE: ok=true"
echo "NEXT: none"
