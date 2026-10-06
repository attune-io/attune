#!/usr/bin/env python3
"""Open, update, or close the assigned E2E nightly failure issue.

A full cancel (every required job cancelled or skipped, none failed)
does nothing: it must not open an issue and must not close one.
A failure is red. A cancelled or skipped job beside a success is also
red, so a timed-out matrix leg or a skipped E2E after a failed
prepare-matrix still pages. Timeout conclusions are cancelled.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from datetime import date, datetime, timezone
from typing import Callable

DEFAULT_ASSIGNEE = "SebTardif"
LIST_LABEL = "e2e-nightly-failure"
DEFAULT_LABELS = [LIST_LABEL, "ready"]
MARKER = "<!-- nightly-failure-state:"
LABEL_DEFAULTS: dict[str, tuple[str, str]] = {
    LIST_LABEL: ("B60205", "Scheduled E2E nightly failure"),
    "ready": ("0E8A16", "Ready for a maintainer"),
}
GhRunner = Callable[[list[str]], subprocess.CompletedProcess[str]]


def outcome(results: dict[str, str]) -> str:
    if not results:
        return "noop"
    vals = list(results.values())
    known = {"success", "failure", "cancelled", "skipped"}
    if any(status == "failure" or status not in known for status in vals):
        return "red"
    if "success" in vals and any(status in {"cancelled", "skipped"} for status in vals):
        return "red"
    if all(status == "success" for status in vals):
        return "green"
    # Cancelled and skipped, with no success and no failure.
    return "noop"


def failure_signature(results: dict[str, str]) -> str:
    """Jobs that failed or timed out.

    A skipped job is left out when another job failed or was cancelled,
    because that skip is the dependent job that never started. A skip
    beside an otherwise green run is the failure, so it stays. Cancelled
    stays: a timeout is its own failure, and a new signature replaces
    the issue.
    """
    known = {"success", "failure", "cancelled", "skipped"}
    failed = [
        name
        for name, status in results.items()
        if status == "failure" or status not in known
    ]
    cancelled = [name for name, status in results.items() if status == "cancelled"]
    if failed or cancelled:
        return ",".join(sorted(failed + cancelled))
    skipped = [name for name, status in results.items() if status == "skipped"]
    return ",".join(sorted(skipped))


def day_count(today: date, first_failed_on: str) -> int:
    delta = (today - date.fromisoformat(first_failed_on)).days
    if delta < 0:
        delta = 0
    return delta + 1


def decide(
    *,
    today: date,
    results: dict[str, str],
    issue: dict[str, str] | None,
    run_id: str,
) -> dict[str, object]:
    kind = outcome(results)
    if kind == "noop":
        return {"action": "noop"}
    if kind == "green":
        if issue is None:
            return {"action": "noop"}
        return {"action": "close"}
    signature = failure_signature(results)
    if issue is not None and issue.get("run_id") == run_id:
        return {"action": "noop"}
    if issue is None:
        return {
            "action": "create",
            "signature": signature,
            "first_failed_on": today.isoformat(),
            "day_count": 1,
            "run_id": run_id,
        }
    first = str(issue["first_failed_on"])
    count = day_count(today, first)
    if issue.get("signature") == signature and first == today.isoformat():
        action = "update"
    else:
        action = "replace"
    return {
        "action": action,
        "signature": signature,
        "first_failed_on": first,
        "day_count": count,
        "run_id": run_id,
    }


def parse_state(body: str) -> dict[str, str] | None:
    start = body.find(MARKER)
    if start < 0:
        return None
    json_start = body.find("{", start)
    json_end = body.find("-->", json_start)
    if json_start < 0 or json_end < 0:
        return None
    try:
        data = json.loads(body[json_start:json_end].strip())
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict):
        return None
    state: dict[str, str] = {}
    for key in ("signature", "first_failed_on", "run_id"):
        value = data.get(key)
        if not isinstance(value, str) or not value:
            return None
        state[key] = value
    return state


def prepare_matches(
    matches: list[dict[str, object]],
    *,
    results: dict[str, str],
    today: date,
) -> tuple[dict[str, str] | None, list[dict[str, object]]]:
    issue: dict[str, str] | None = None
    ordered = list(matches)
    for item in matches:
        parsed = parse_state(str(item.get("body") or ""))
        if parsed is not None:
            issue = parsed
            ordered = [item] + [other for other in matches if other is not item]
            break
    kind = outcome(results)
    if issue is None and matches and kind == "green":
        issue = {
            "signature": "unparsed",
            "first_failed_on": today.isoformat(),
            "run_id": "unparsed",
        }
    if issue is None and matches and kind == "red":
        issue = {
            "signature": "legacy",
            "first_failed_on": today.isoformat(),
            "run_id": "legacy",
        }
    return issue, ordered


def normalize_assignee(value: str, org: str = "") -> str:
    cleaned = value.strip().lstrip("@")
    if not cleaned or (org and cleaned == org):
        return DEFAULT_ASSIGNEE
    return cleaned


def issue_title(signature: str, day_count_value: int) -> str:
    unit = "day" if day_count_value == 1 else "days"
    return f"Nightly failed: {signature} ({day_count_value} consecutive {unit})"


def compose_body(
    detail: str,
    *,
    run_url: str,
    signature: str,
    day_count_value: int,
    first_failed_on: str,
    run_id: str,
) -> str:
    state = json.dumps(
        {
            "signature": signature,
            "first_failed_on": first_failed_on,
            "run_id": run_id,
        },
        separators=(",", ":"),
    )
    kept: list[str] = []
    for line in detail.replace("\r\n", "\n").split("\n"):
        if "nightly-failure-state:" in line:
            continue
        kept.append(line)
    text = "\n".join(kept).strip()
    if not text:
        text = "\n".join(["Scheduled nightly failed.", "", f"Run: {run_url}"])
    return text + "\n\n" + f"{MARKER} {state} -->" + "\n"


def close_comment(run_url: str) -> str:
    if run_url:
        return f"Scheduled nightly passed. Closing this failure. Run: {run_url}"
    return "Scheduled nightly passed. Closing this failure."


def parse_results(raw: str) -> dict[str, str]:
    parsed: dict[str, str] = {}
    for part in raw.split():
        if "=" not in part:
            continue
        name, status = part.split("=", 1)
        if name and status:
            parsed[name] = status
    return parsed


def repo_owner(repo: str) -> str:
    owner, sep, _name = repo.partition("/")
    if not sep:
        return ""
    return owner


def _run_gh(args: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=False, text=True, capture_output=True)


def _fail_gh(proc: subprocess.CompletedProcess[str], what: str) -> int:
    print(f"FAIL: gh {what} exited {proc.returncode}", file=sys.stderr)
    if proc.stderr:
        print(proc.stderr, file=sys.stderr)
    if proc.stdout:
        print(proc.stdout, file=sys.stderr)
    return 1


def _list_issues(repo: str, label: str, gh: GhRunner) -> tuple[list[dict[str, object]], int]:
    proc = gh(
        [
            "gh",
            "issue",
            "list",
            "--repo",
            repo,
            "--label",
            label,
            "--state",
            "open",
            "--limit",
            "50",
            "--json",
            "number,title,body",
        ]
    )
    if proc.returncode != 0:
        return [], _fail_gh(proc, "issue list")
    try:
        data = json.loads(proc.stdout or "[]")
    except json.JSONDecodeError:
        print("FAIL: gh issue list returned unexpected JSON", file=sys.stderr)
        return [], 1
    if not isinstance(data, list):
        print("FAIL: gh issue list returned unexpected JSON", file=sys.stderr)
        return [], 1
    return data, 0


def ensure_labels(repo: str, labels: list[str], gh: GhRunner) -> int:
    proc = gh(
        [
            "gh",
            "label",
            "list",
            "--repo",
            repo,
            "--limit",
            "200",
            "--json",
            "name",
        ]
    )
    if proc.returncode != 0:
        return _fail_gh(proc, "label list")
    try:
        data = json.loads(proc.stdout or "[]")
    except json.JSONDecodeError:
        print("FAIL: gh label list returned unexpected JSON", file=sys.stderr)
        return 1
    existing: set[str] = set()
    if isinstance(data, list):
        for item in data:
            if isinstance(item, dict) and isinstance(item.get("name"), str):
                existing.add(str(item["name"]))
    for name in labels:
        if name in existing:
            continue
        spec = LABEL_DEFAULTS.get(name)
        if spec is None:
            print(f"FAIL: label {name} does not exist", file=sys.stderr)
            return 1
        color, description = spec
        print(f"DO: create label {name}")
        created = gh(
            [
                "gh",
                "label",
                "create",
                name,
                "--repo",
                repo,
                "--color",
                color,
                "--description",
                description,
            ]
        )
        if created.returncode != 0:
            return _fail_gh(created, "label create")
    return 0


def apply_decision(
    *,
    repo: str,
    labels: list[str],
    assignee: str,
    run_url: str,
    detail: str,
    decision: dict[str, object],
    matches: list[dict[str, object]],
    gh: GhRunner = _run_gh,
) -> int:
    action = str(decision["action"])
    print(f"OK: decision {action}")
    if action == "noop":
        print(json.dumps({"action": "noop"}))
        print("DONE: no issue change")
        return 0
    if action == "close":
        comment = close_comment(run_url)
        for item in matches:
            number = str(item["number"])
            print(f"DO: close issue {number}")
            proc = gh(
                [
                    "gh",
                    "issue",
                    "close",
                    number,
                    "--repo",
                    repo,
                    "--reason",
                    "completed",
                    "--comment",
                    comment,
                ]
            )
            if proc.returncode != 0:
                return _fail_gh(proc, "issue close")
        print(json.dumps({"action": "close", "count": len(matches)}))
        print("DONE: closed matching issues")
        return 0

    signature = str(decision["signature"])
    first = str(decision["first_failed_on"])
    count = int(str(decision["day_count"]))
    run_id = str(decision["run_id"])
    title = issue_title(signature, count)
    body = compose_body(
        detail,
        run_url=run_url,
        signature=signature,
        day_count_value=count,
        first_failed_on=first,
        run_id=run_id,
    )
    body_path = os.environ.get("REPORT_BODY_PATH", "")
    if not body_path:
        body_path = os.path.join(
            os.environ.get("RUNNER_TEMP", "/tmp"),
            "nightly-failure-body.md",
        )
    with open(body_path, "w", encoding="utf-8") as handle:
        handle.write(body)

    if action == "update":
        number = str(matches[0]["number"])
        print(f"DO: update issue {number}")
        cmd = [
            "gh",
            "issue",
            "edit",
            number,
            "--repo",
            repo,
            "--title",
            title,
            "--body-file",
            body_path,
            "--add-assignee",
            assignee,
        ]
        for label in labels:
            cmd.extend(["--add-label", label])
        proc = gh(cmd)
        if proc.returncode != 0:
            return _fail_gh(proc, "issue edit")
        print(json.dumps({"action": "update", "number": int(number)}))
        print("DONE: updated the open failure issue")
        return 0

    if action not in {"create", "replace"}:
        print(f"FAIL: unknown decision {action}", file=sys.stderr)
        return 1

    # Create the replacement before closing the old issue. A failed
    # create must leave yesterday's issue in place.
    code = ensure_labels(repo, labels, gh)
    if code != 0:
        return code
    print(f"DO: create issue {title}")
    cmd = [
        "gh",
        "issue",
        "create",
        "--repo",
        repo,
        "--title",
        title,
        "--body-file",
        body_path,
        "--assignee",
        assignee,
    ]
    for label in labels:
        cmd.extend(["--label", label])
    proc = gh(cmd)
    if proc.returncode != 0:
        return _fail_gh(proc, "issue create")
    url = (proc.stdout or "").strip()
    print(f"OK: created {url}")

    if action == "replace":
        for item in matches:
            number = str(item["number"])
            print(f"DO: close replaced issue {number}")
            closed = gh(
                [
                    "gh",
                    "issue",
                    "close",
                    number,
                    "--repo",
                    repo,
                    "--reason",
                    "completed",
                    "--comment",
                    "Replaced by a new nightly failure issue for this run.",
                ]
            )
            if closed.returncode != 0:
                return _fail_gh(closed, "issue close")

    print(json.dumps({"action": action, "title": title}))
    print("DONE: recorded the scheduled failure")
    return 0


def _finish(kind: str, code: int) -> int:
    if kind == "red" and code == 0:
        return 1
    return code


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id", default="")
    parser.add_argument("--run-url", default="")
    parser.add_argument("--repo", default=os.environ.get("GH_REPO", ""))
    parser.add_argument(
        "--assignee",
        default=os.environ.get("NIGHTLY_FAILURE_ASSIGNEE", ""),
    )
    parser.add_argument("--label", action="append", default=None)
    parser.add_argument("--today", default="")
    parser.add_argument("--body-file", default="")
    parser.add_argument("--issue-json", default="")
    parser.add_argument("--outcome-only", action="store_true")
    parser.add_argument("--no-issues", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)

    print("PLAN: record nightly failure issue", file=sys.stderr)
    raw_results = os.environ.get("JOB_RESULTS", "")
    if not raw_results.strip():
        print("FAIL: JOB_RESULTS is required", file=sys.stderr)
        return 2
    results = parse_results(raw_results)
    if not results:
        print("FAIL: JOB_RESULTS had no job=status pairs", file=sys.stderr)
        return 2
    kind = outcome(results)
    if args.outcome_only:
        print(kind)
        print("DONE: outcome", file=sys.stderr)
        return 0
    if args.today:
        today = date.fromisoformat(args.today)
    else:
        today = datetime.now(timezone.utc).date()
    if args.no_issues:
        print(json.dumps({"action": "no-issues", "outcome": kind}))
        print("DONE: issues not changed")
        return 1 if kind == "red" else 0
    if not args.run_id or not args.repo:
        print("FAIL: --run-id and --repo are required", file=sys.stderr)
        return 2
    labels = list(args.label) if args.label else list(DEFAULT_LABELS)
    if LIST_LABEL not in labels:
        labels.insert(0, LIST_LABEL)
    assignee = normalize_assignee(args.assignee or "", repo_owner(args.repo))
    detail = ""
    if args.body_file:
        with open(args.body_file, encoding="utf-8") as handle:
            detail = handle.read()
    if args.dry_run:
        issue = json.loads(args.issue_json) if args.issue_json else None
        decision = decide(today=today, results=results, issue=issue, run_id=args.run_id)
        print(f"OK: decision {decision['action']}")
        print(json.dumps(decision))
        print("DONE: dry run")
        return 0

    print("DO: list open issues")
    found, code = _list_issues(args.repo, LIST_LABEL, _run_gh)
    if code != 0:
        return code
    issue, matches = prepare_matches(found, results=results, today=today)
    decision = decide(today=today, results=results, issue=issue, run_id=args.run_id)
    code = apply_decision(
        repo=args.repo,
        labels=labels,
        assignee=assignee,
        run_url=args.run_url,
        detail=detail,
        decision=decision,
        matches=matches,
    )
    return _finish(kind, code)


if __name__ == "__main__":
    sys.exit(main())
