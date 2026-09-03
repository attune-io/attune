#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Fail if a Chainsaw try-step script has a bare grep -q or curl -sf
# (including curl -sf ... && echo). Chainsaw runs script.content under
# sh without implicit errexit. set -e does not apply to && / || lists,
# so a failed curl on the left of && still exits 0 (#628).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ "${1:-}" == "--root" ]]; then
  ROOT="$2"
fi
python3 - "$ROOT" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
failed = []
standalone = re.compile(r"^(grep\s+-q|curl\s+-sf)\b")

def script_blocks(text: str):
    """Yield (start_line, body) for script.content | / |- blocks under try."""
    lines = text.splitlines()
    i = 0
    in_catch = False
    catch_indent = None
    while i < len(lines):
        raw = lines[i]
        stripped = raw.lstrip(" ")
        indent = len(raw) - len(stripped)
        if stripped.startswith("catch:"):
            in_catch = True
            catch_indent = indent
            i += 1
            continue
        if in_catch and stripped and indent <= catch_indent and not stripped.startswith("#"):
            in_catch = False
            catch_indent = None
        if in_catch:
            i += 1
            continue
        if stripped in ("content: |", "content: |-"):
            look = "\n".join(lines[max(0, i - 8):i + 1])
            if "script:" not in look:
                i += 1
                continue
            body_indent = indent + 2
            body = []
            start = i + 2
            j = i + 1
            while j < len(lines):
                bl = lines[j]
                if bl.strip() == "":
                    body.append(bl)
                    j += 1
                    continue
                bi = len(bl) - len(bl.lstrip(" "))
                if bi < body_indent:
                    break
                body.append(bl)
                j += 1
            yield start, "\n".join(body)
            i = j
            continue
        i += 1

for path in sorted((root / "test" / "e2e").glob("*/chainsaw-test.yaml")):
    text = path.read_text()
    for start, body in script_blocks(text):
        has_errexit = "set -e" in body
        for offset, line in enumerate(body.splitlines()):
            s = line.strip()
            if s.startswith("if ") or s.startswith("elif ") or s.startswith("#"):
                continue
            if not standalone.match(s):
                continue
            if s.endswith("|| true") or " || true" in s:
                continue
            if " && " in s or " || " in s:
                failed.append(
                    f"{path.relative_to(root)}:{start + offset}: "
                    f"'{s.split()[0]}' in &&/|| is not covered by set -e"
                )
            elif not has_errexit:
                failed.append(
                    f"{path.relative_to(root)}:{start + offset}: "
                    f"bare '{s.split()[0]}' without set -e (exit code is ignored)"
                )

if failed:
    print("FAIL: Chainsaw try-scripts ignore command failures (#628):")
    for line in failed:
        print(f"  {line}")
    sys.exit(1)
print("OK: no bare grep -q / curl -sf without set -e")
PY
