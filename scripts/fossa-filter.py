#!/usr/bin/env python3
"""Filter known false positives from FOSSA test JSON output.

Usage:
    fossa test --format json > fossa-results.json 2>fossa-stderr.txt
    python3 scripts/fossa-filter.py fossa-results.json [fossa-stderr.txt]

Exit codes:
    0 - All issues are known false positives (or no issues found)
    1 - Genuine issues remain after filtering
"""

import json
import re
import sys


def extract_package(revision_id: str) -> str:
    """Extract the package name from a FOSSA revisionId.

    Format: {ecosystem}+{package}${version}
    Example: go+k8s.io/client-go$v0.36.2 -> k8s.io/client-go
    """
    # Strip ecosystem prefix (everything before and including first +)
    if "+" in revision_id:
        revision_id = revision_id.split("+", 1)[1]
    # Strip version suffix (everything from last $ onward)
    if "$" in revision_id:
        revision_id = revision_id.rsplit("$", 1)[0]
    return revision_id


# Known false positives: package prefix -> set of issue type patterns.
# "outdated" matches FOSSA's outdated dependency quality checks.
# License IDs match licensing policy conflicts/flags.
KNOWN_FALSE_POSITIVES: dict[str, set[str]] = {
    # k8s.io/client-go is pinned to match the target K8s API version.
    # FOSSA flags it as outdated because patch versions within the same
    # minor exist, but we intentionally pin to v0.36.x for K8s 1.36 API
    # compatibility. This applies to all k8s.io modules.
    "k8s.io/": {"outdated"},
    # golang.org/x/text bundles Unicode CLDR data files with CC-BY-SA
    # notices. The module itself is BSD-3-Clause. See golang/go#53534.
    "golang.org/x/text": {"CC-BY-SA-1.0", "CC-BY-SA-2.0", "CC-BY-SA-2.5",
                          "CC-BY-SA-3.0", "CC-BY-SA-4.0"},
    # golang.org/x/crypto has openssl-ssleay license text in test fixtures.
    # The module is BSD-3-Clause.
    "golang.org/x/crypto": {"openssl-ssleay"},
}

# Text-based patterns for fallback when JSON is empty or unparseable.
# Each entry: (regex matching the FOSSA text output, reason for filtering).
TEXT_FALSE_POSITIVES: list[tuple[re.Pattern, str]] = [
    (re.compile(r"Outdated dependency detected in (k8s\.io/\S+)"),
     "k8s.io module pinned to match target K8s API version"),
]


def is_false_positive(issue: dict) -> bool:
    """Check if a FOSSA issue matches a known false positive pattern."""
    revision_id = issue.get("revisionId", "")
    package = extract_package(revision_id)

    # Determine the issue type string to match against
    issue_type = issue.get("type", "")
    license_id = issue.get("license", issue.get("licenseId", ""))

    for prefix, patterns in KNOWN_FALSE_POSITIVES.items():
        if not package.startswith(prefix):
            continue
        # Check outdated quality issues
        if "outdated" in patterns and issue_type in (
            "outdated", "policy_flag", "quality"
        ):
            if prefix == "k8s.io/":
                return True
        # Check license issues
        if license_id in patterns:
            return True

    return False


def filter_via_text(stderr_file: str) -> int:
    """Fallback: filter using text output when JSON is empty/unparseable.

    Returns 0 if all text-matched issues are known false positives,
    1 if genuine issues remain or no patterns matched.
    """
    try:
        with open(stderr_file) as f:
            text = f.read()
    except FileNotFoundError:
        return 1

    if not text.strip():
        return 1

    matched_all = True
    found_any = False

    for line in text.splitlines():
        # Look for issue lines (start with ⚑ or contain "dependency detected")
        if "dependency detected" not in line.lower() and "issue" not in line.lower():
            continue

        line_matched = False
        for pattern, reason in TEXT_FALSE_POSITIVES:
            if pattern.search(line):
                print(f"  Filtered (text): {line.strip()}")
                print(f"    Reason: {reason}")
                line_matched = True
                found_any = True
                break

        if not line_matched and ("dependency detected" in line.lower()):
            print(f"  Genuine issue: {line.strip()}")
            matched_all = False
            found_any = True

    if found_any and matched_all:
        print("\nAll issues are known false positives. FOSSA check passed.")
        return 0

    return 1


def main() -> int:
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <fossa-results.json> [fossa-stderr.txt]",
              file=sys.stderr)
        return 1

    results_file = sys.argv[1]
    stderr_file = sys.argv[2] if len(sys.argv) > 2 else None

    # Try JSON-based filtering first
    try:
        with open(results_file) as f:
            content = f.read().strip()
    except FileNotFoundError:
        print(f"Results file not found: {results_file}", file=sys.stderr)
        if stderr_file:
            print("Falling back to text-based filtering...")
            return filter_via_text(stderr_file)
        return 1

    # fossa test --format json may produce empty output for quality issues
    if not content:
        print("JSON output is empty (common for quality-only issues).")
        if stderr_file:
            print("Falling back to text-based filtering...")
            return filter_via_text(stderr_file)
        print("No stderr file provided for fallback.", file=sys.stderr)
        return 1

    try:
        data = json.loads(content)
    except json.JSONDecodeError:
        print("JSON output is not valid JSON.", file=sys.stderr)
        if stderr_file:
            print("Falling back to text-based filtering...")
            return filter_via_text(stderr_file)
        return 1

    # fossa test --format json returns a list of issues
    issues = data if isinstance(data, list) else data.get("issues", [])

    if not issues:
        # JSON parsed but no issues array; try text fallback
        if stderr_file:
            print("JSON has no issues array. Falling back to text-based filtering...")
            return filter_via_text(stderr_file)
        print("No issues found in JSON output.")
        return 0

    genuine = []
    filtered = []

    for issue in issues:
        if is_false_positive(issue):
            filtered.append(issue)
        else:
            genuine.append(issue)

    if filtered:
        print(f"Filtered {len(filtered)} known false positive(s):")
        for issue in filtered:
            rev = issue.get("revisionId", "unknown")
            itype = issue.get("type", "unknown")
            print(f"  - {rev} ({itype})")

    if genuine:
        print(f"\n{len(genuine)} genuine issue(s) remain:")
        for issue in genuine:
            rev = issue.get("revisionId", "unknown")
            itype = issue.get("type", "unknown")
            print(f"  - {rev} ({itype})")
        return 1

    print("\nAll issues are known false positives. FOSSA check passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
