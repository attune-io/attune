#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# The generic release-please updater replaces the first semver on a
# line that contains x-release-please-version. The yaml updater dumps
# Chart.yaml and deletes that marker, which left artifacthub.io/images
# on the previous tag in release PR #1183.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "PLAN: generic updater bumps chart version, appVersion, and image tag"
echo "DO: reject a yaml extra-file and apply the generic line replace"

python3 - "$ROOT" <<'PY'
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
cfg = json.loads((root / "release-please-config.json").read_text())
files = cfg["packages"]["."]["extra-files"]
chart = [item for item in files if item.get("path") == "charts/attune/Chart.yaml"]
types = [item.get("type") for item in chart]
if types != ["generic"]:
    print(f"FAIL: Chart.yaml extra-files are {types}, want ['generic']")
    print("DONE: ok=false reason=yaml-updater-enabled")
    print("NEXT: remove the yaml extra-file. It deletes x-release-please-version.")
    sys.exit(1)

version_re = re.compile(
    r"(?P<major>\d+)\.(?P<minor>\d+)\.(?P<patch>\d+)(?:-(?P<pre>[\w.]+))?(?:\+(?P<build>[-\w.]+))?"
)
lines = (root / "charts/attune/Chart.yaml").read_text().splitlines()
out = []
marked = 0
for line in lines:
    if "x-release-please-version" in line and "x-release-please-start" not in line:
        new, count = version_re.subn("9.9.9", line, count=1)
        if count != 1:
            print(f"FAIL: marker line has no semver: {line}")
            print("DONE: ok=false reason=marker-without-version")
            sys.exit(1)
        if "x-release-please-version" not in new:
            print(f"FAIL: replace dropped the marker: {new}")
            print("DONE: ok=false reason=marker-lost")
            sys.exit(1)
        out.append(new)
        marked += 1
    else:
        out.append(line)

if marked != 3:
    print(f"FAIL: expected 3 marker lines, found {marked}")
    print("DONE: ok=false reason=marker-count")
    sys.exit(1)

text = "\n".join(out) + "\n"
need = [
    "version: 9.9.9 # x-release-please-version",
    "appVersion: 9.9.9 # x-release-please-version",
    "ghcr.io/attune-io/attune:9.9.9",
]
for item in need:
    if item not in text:
        print(f"FAIL: missing {item}")
        print("DONE: ok=false reason=version-not-replaced")
        sys.exit(1)

for line in text.splitlines():
    if line.startswith("  artifacthub.io/images:"):
        quote_end = line.rfind('"')
        if "x-release-please" in line[: quote_end + 1]:
            print(f"FAIL: marker is inside the annotation string: {line}")
            print("DONE: ok=false reason=marker-inside-quotes")
            sys.exit(1)
        if not line.rstrip().endswith("# x-release-please-version"):
            print(f"FAIL: marker is not after the annotation: {line}")
            print("DONE: ok=false reason=marker-position")
            sys.exit(1)
        break
else:
    print("FAIL: artifacthub.io/images line missing after replace")
    print("DONE: ok=false reason=annotation-missing")
    sys.exit(1)

print("OK: generic replace updates version, appVersion, and the image tag")
PY

echo "DONE: ok=true"
echo "NEXT: none"
