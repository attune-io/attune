#!/usr/bin/env python3
"""Format failed GitHub Actions jobs from jobs API JSON (stdin)."""
from __future__ import annotations

import json
import sys


def main() -> int:
    try:
        data = json.load(sys.stdin)
    except Exception:
        return 0
    jobs = data
    if isinstance(data, dict):
        jobs = data.get("jobs") or []
    if not isinstance(jobs, list):
        return 0
    lines: list[str] = []
    for j in jobs:
        if not isinstance(j, dict) or j.get("conclusion") != "failure":
            continue
        name = j.get("name") or "unknown"
        steps = [
            s.get("name")
            for s in (j.get("steps") or [])
            if isinstance(s, dict) and s.get("conclusion") == "failure" and s.get("name")
        ]
        if steps:
            lines.append(f"- **{name}** (steps: {', '.join(steps)})")
        else:
            lines.append(f"- **{name}**")
    sys.stdout.write("\n".join(lines))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
