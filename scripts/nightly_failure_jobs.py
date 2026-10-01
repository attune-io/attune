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
    bad = {"failure", "cancelled", "timed_out"}
    for j in jobs:
        if not isinstance(j, dict) or j.get("conclusion") not in bad:
            continue
        name = j.get("name") or "unknown"
        # The report job fails because a matrix leg failed. Naming its
        # Summary step hides the leg that was still running.
        if name == "Nightly Results":
            continue
        steps = [
            s.get("name")
            for s in (j.get("steps") or [])
            if isinstance(s, dict)
            and s.get("conclusion") in bad
            and s.get("name")
            and not str(s.get("name")).startswith("Post ")
        ]
        if steps:
            lines.append(f"- **{name}** (steps: {', '.join(steps)})")
        else:
            lines.append(f"- **{name}**")
    sys.stdout.write("\n".join(lines))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
