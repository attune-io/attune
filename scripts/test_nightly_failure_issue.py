#!/usr/bin/env python3
"""Decision table for scripts/nightly_failure_issue.py."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import unittest
from datetime import date
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "nightly_failure_issue.py"


def load():
    spec = importlib.util.spec_from_file_location("nightly_failure_issue", SCRIPT)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load nightly_failure_issue.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class NightlyFailureIssueTest(unittest.TestCase):
    def setUp(self) -> None:
        self.m = load()
        self.today = date(2026, 10, 6)

    def test_outcome(self) -> None:
        outcome = self.m.outcome
        green = {
            "prepare-matrix": "success",
            "test-e2e": "success",
            "fuzz": "success",
        }
        self.assertEqual(outcome(green), "green")
        self.assertEqual(
            outcome(
                {
                    "prepare-matrix": "failure",
                    "test-e2e": "skipped",
                    "fuzz": "success",
                }
            ),
            "red",
        )
        self.assertEqual(
            outcome(
                {
                    "prepare-matrix": "success",
                    "test-e2e": "cancelled",
                    "fuzz": "success",
                }
            ),
            "red",
        )
        self.assertEqual(
            outcome(
                {
                    "prepare-matrix": "cancelled",
                    "test-e2e": "cancelled",
                    "fuzz": "skipped",
                }
            ),
            "noop",
        )
        self.assertEqual(
            outcome({"test-e2e": "failure", "fuzz": "success"}),
            "red",
        )
        # prepare-matrix cancelled skips E2E. Fuzz cancelled too. No success.
        self.assertEqual(
            outcome(
                {
                    "prepare-matrix": "cancelled",
                    "test-e2e": "skipped",
                    "fuzz": "cancelled",
                }
            ),
            "noop",
        )

    def test_decide(self) -> None:
        red = {"test-e2e": "failure", "fuzz": "success"}
        created = self.m.decide(
            today=self.today, results=red, issue=None, run_id="9"
        )
        self.assertEqual(created["action"], "create")
        self.assertEqual(created["signature"], "test-e2e")
        self.assertEqual(created["day_count"], 1)

        same_day = {
            "signature": "test-e2e",
            "first_failed_on": "2026-10-06",
            "run_id": "1",
        }
        updated = self.m.decide(
            today=self.today, results=red, issue=same_day, run_id="2"
        )
        self.assertEqual(updated["action"], "update")

        next_day = {
            "signature": "test-e2e",
            "first_failed_on": "2026-10-05",
            "run_id": "1",
        }
        replaced = self.m.decide(
            today=self.today, results=red, issue=next_day, run_id="2"
        )
        self.assertEqual(replaced["action"], "replace")
        self.assertEqual(replaced["day_count"], 2)
        self.assertEqual(
            self.m.issue_title("test-e2e", 2),
            "Nightly failed: test-e2e (2 consecutive days)",
        )
        self.assertEqual(
            self.m.issue_title("test-e2e", 1),
            "Nightly failed: test-e2e (1 consecutive day)",
        )

        same_run = self.m.decide(
            today=self.today,
            results=red,
            issue={
                "signature": "test-e2e",
                "first_failed_on": "2026-10-06",
                "run_id": "9",
            },
            run_id="9",
        )
        self.assertEqual(same_run["action"], "noop")

        green = {"test-e2e": "success", "fuzz": "success"}
        self.assertEqual(
            self.m.decide(
                today=self.today, results=green, issue=same_day, run_id="3"
            )["action"],
            "close",
        )
        self.assertEqual(
            self.m.decide(
                today=self.today, results=green, issue=None, run_id="3"
            )["action"],
            "noop",
        )
        cancelled = {"test-e2e": "cancelled", "fuzz": "cancelled"}
        self.assertEqual(
            self.m.decide(
                today=self.today, results=cancelled, issue=same_day, run_id="4"
            )["action"],
            "noop",
        )
        live_cancel = {
            "prepare-matrix": "cancelled",
            "test-e2e": "skipped",
            "fuzz": "cancelled",
        }
        self.assertEqual(
            self.m.decide(
                today=self.today, results=live_cancel, issue=same_day, run_id="5"
            )["action"],
            "noop",
        )

    def test_signature_omits_dependent_skip(self) -> None:
        prepare_failed = {
            "prepare-matrix": "failure",
            "test-e2e": "skipped",
            "fuzz": "success",
        }
        self.assertEqual(self.m.failure_signature(prepare_failed), "prepare-matrix")
        created = self.m.decide(
            today=self.today, results=prepare_failed, issue=None, run_id="1"
        )
        self.assertEqual(created["signature"], "prepare-matrix")
        timed_out = {
            "prepare-matrix": "success",
            "test-e2e": "cancelled",
            "fuzz": "success",
        }
        self.assertEqual(self.m.failure_signature(timed_out), "test-e2e")
        fuzz_also = {
            "prepare-matrix": "failure",
            "test-e2e": "skipped",
            "fuzz": "cancelled",
        }
        self.assertEqual(self.m.failure_signature(fuzz_also), "fuzz,prepare-matrix")
        skip_only = {
            "prepare-matrix": "success",
            "test-e2e": "skipped",
            "fuzz": "success",
        }
        self.assertEqual(self.m.failure_signature(skip_only), "test-e2e")

    def test_legacy_open_issue_is_replaced(self) -> None:
        matches = [
            {
                "number": 3,
                "title": "Nightly failed on 2026-10-05",
                "body": "old body",
            }
        ]
        issue, ordered = self.m.prepare_matches(
            matches,
            results={"test-e2e": "failure", "fuzz": "success"},
            today=self.today,
        )
        self.assertEqual(ordered[0]["number"], 3)
        decision = self.m.decide(
            today=self.today,
            results={"test-e2e": "failure", "fuzz": "success"},
            issue=issue,
            run_id="8",
        )
        self.assertEqual(decision["action"], "replace")

    def test_assignee(self) -> None:
        self.assertEqual(self.m.normalize_assignee("", "attune-io"), "SebTardif")
        self.assertEqual(
            self.m.normalize_assignee("attune-io", "attune-io"), "SebTardif"
        )
        self.assertEqual(
            self.m.normalize_assignee("@SebTardif", "attune-io"), "SebTardif"
        )

    def test_body_state_round_trip(self) -> None:
        body = self.m.compose_body(
            "E2E result: `failure`",
            run_url="https://example.com/runs/9",
            signature="test-e2e",
            day_count_value=1,
            first_failed_on="2026-10-06",
            run_id="9",
        )
        parsed = self.m.parse_state(body)
        self.assertIsNotNone(parsed)
        assert parsed is not None
        self.assertEqual(parsed["signature"], "test-e2e")
        self.assertEqual(parsed["run_id"], "9")
        self.assertIn("E2E result: `failure`", body)

    def test_apply_create_assigns(self) -> None:
        calls: list[list[str]] = []

        def gh(args: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(args)
            stdout = ""
            if args[1:3] == ["label", "list"]:
                stdout = json.dumps(
                    [{"name": "ready"}, {"name": "e2e-nightly-failure"}]
                )
            if args[1:3] == ["issue", "create"]:
                stdout = "https://example.com/issues/4\n"
            return subprocess.CompletedProcess(args, 0, stdout=stdout, stderr="")

        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="https://example.com/runs/9",
            detail="boom",
            decision={
                "action": "create",
                "signature": "fuzz",
                "first_failed_on": "2026-10-06",
                "day_count": 1,
                "run_id": "9",
            },
            matches=[],
            gh=gh,
        )
        self.assertEqual(code, 0)
        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="https://example.com/runs/9",
            detail="boom",
            decision={
                "action": "update",
                "signature": "fuzz",
                "first_failed_on": "2026-10-06",
                "day_count": 1,
                "run_id": "9",
            },
            matches=[{"number": 4, "title": "old", "body": "old"}],
            gh=gh,
        )
        self.assertEqual(code, 0)
        edited = next(call for call in calls if call[1:3] == ["issue", "edit"])
        self.assertEqual(edited[edited.index("--add-assignee") + 1], "SebTardif")
        self.assertIn("ready", edited)
        self.assertEqual(edited[edited.index("--add-label") + 1], "e2e-nightly-failure")
        create = next(call for call in calls if call[1:3] == ["issue", "create"])
        self.assertEqual(create[create.index("--assignee") + 1], "SebTardif")
        self.assertIn("e2e-nightly-failure", create)
        self.assertIn("ready", create)
        body_path = create[create.index("--body-file") + 1]
        text = Path(body_path).read_text(encoding="utf-8")
        self.assertIn("nightly-failure-state:", text)

    def test_apply_green_closes_and_cancel_does_not(self) -> None:
        calls: list[list[str]] = []

        def gh(args: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="https://example.com/runs/3",
            detail="",
            decision={"action": "close"},
            matches=[{"number": 7, "title": "old", "body": ""}],
            gh=gh,
        )
        self.assertEqual(code, 0)
        close = calls[0]
        self.assertEqual(close[1:3], ["issue", "close"])
        self.assertIn("https://example.com/runs/3", " ".join(close))

        calls.clear()
        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="",
            detail="",
            decision={"action": "noop"},
            matches=[{"number": 7, "title": "old", "body": ""}],
            gh=gh,
        )
        self.assertEqual(code, 0)
        self.assertEqual(calls, [])

    def test_replace_creates_before_close(self) -> None:
        calls: list[list[str]] = []

        def gh(args: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(args)
            stdout = ""
            code = 0
            if args[1:3] == ["label", "list"]:
                stdout = json.dumps(
                    [{"name": "ready"}, {"name": "e2e-nightly-failure"}]
                )
            elif args[1:3] == ["issue", "create"]:
                stdout = "https://example.com/issues/8\n"
            return subprocess.CompletedProcess(args, code, stdout=stdout, stderr="")

        decision = {
            "action": "replace",
            "signature": "prepare-matrix",
            "first_failed_on": "2026-10-05",
            "day_count": 2,
            "run_id": "2",
        }
        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="https://example.com/runs/2",
            detail="boom",
            decision=decision,
            matches=[{"number": 3, "title": "old", "body": "old"}],
            gh=gh,
        )
        self.assertEqual(code, 0)
        kinds = [call[1:3] for call in calls]
        self.assertLess(kinds.index(["issue", "create"]), kinds.index(["issue", "close"]))

        calls.clear()

        def gh_fail(args: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(args)
            if args[1:3] == ["label", "list"]:
                stdout = json.dumps(
                    [{"name": "ready"}, {"name": "e2e-nightly-failure"}]
                )
                return subprocess.CompletedProcess(args, 0, stdout=stdout, stderr="")
            if args[1:3] == ["issue", "create"]:
                return subprocess.CompletedProcess(
                    args, 1, stdout="", stderr="assignee invalid"
                )
            return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

        code = self.m.apply_decision(
            repo="attune-io/attune",
            labels=["e2e-nightly-failure", "ready"],
            assignee="SebTardif",
            run_url="https://example.com/runs/2",
            detail="boom",
            decision=decision,
            matches=[{"number": 3, "title": "old", "body": "old"}],
            gh=gh_fail,
        )
        self.assertEqual(code, 1)
        self.assertFalse(any(call[1:3] == ["issue", "close"] for call in calls))

    def test_same_run_main_stays_red_without_a_second_issue(self) -> None:
        body = self.m.compose_body(
            "boom",
            run_url="https://example.com/runs/9",
            signature="test-e2e",
            day_count_value=1,
            first_failed_on="2026-10-06",
            run_id="9",
        )
        payload = json.dumps([{"number": 4, "title": "open", "body": body}])
        calls: list[list[str]] = []

        def fake(args: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, stdout=payload, stderr="")

        original = self.m._run_gh
        self.m._run_gh = fake
        previous = os.environ.get("JOB_RESULTS")
        os.environ["JOB_RESULTS"] = "test-e2e=failure fuzz=success"
        try:
            code = self.m.main(
                [
                    "--repo",
                    "attune-io/attune",
                    "--run-id",
                    "9",
                    "--run-url",
                    "https://example.com/runs/9",
                    "--today",
                    "2026-10-06",
                    "--assignee",
                    "SebTardif",
                ]
            )
        finally:
            self.m._run_gh = original
            if previous is None:
                os.environ.pop("JOB_RESULTS", None)
            else:
                os.environ["JOB_RESULTS"] = previous
        self.assertEqual(code, 1)
        self.assertEqual(calls[0][1:3], ["issue", "list"])
        self.assertFalse(any(call[1:3] == ["issue", "create"] for call in calls))
        self.assertFalse(any(call[1:3] == ["issue", "edit"] for call in calls))

    def test_cli_exit_codes(self) -> None:
        red = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--repo",
                "attune-io/attune",
                "--run-id",
                "1",
                "--no-issues",
            ],
            check=False,
            capture_output=True,
            text=True,
            env={**os.environ, "JOB_RESULTS": "test-e2e=failure fuzz=success"},
        )
        self.assertEqual(red.returncode, 1, red.stderr)
        only = subprocess.run(
            [sys.executable, str(SCRIPT), "--outcome-only"],
            check=False,
            capture_output=True,
            text=True,
            env={**os.environ, "JOB_RESULTS": "test-e2e=failure fuzz=success"},
        )
        self.assertEqual(only.returncode, 0, only.stderr)
        self.assertEqual(only.stdout, "red\n")
        cancelled = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--no-issues",
            ],
            check=False,
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "JOB_RESULTS": "prepare-matrix=cancelled test-e2e=cancelled fuzz=cancelled",
            },
        )
        self.assertEqual(cancelled.returncode, 0, cancelled.stderr)
        self.assertIn('"outcome": "noop"', cancelled.stdout)


if __name__ == "__main__":
    unittest.main()
