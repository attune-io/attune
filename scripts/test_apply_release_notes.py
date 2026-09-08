#!/usr/bin/env python3
"""Tests for hack/apply-release-notes.sh."""

from __future__ import annotations

import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "hack" / "apply-release-notes.sh"


def run_script(
    env: dict[str, str], cwd: Path | None = None
) -> subprocess.CompletedProcess[str]:
    merged = os.environ.copy()
    merged.pop("GH_TOKEN", None)
    merged.pop("GITHUB_TOKEN", None)
    merged.pop("RELEASE_NOTES", None)
    merged.pop("RELEASE_NOTES_TAG", None)
    merged.update(env)
    return subprocess.run(
        ["bash", str(SCRIPT)],
        capture_output=True,
        text=True,
        env=merged,
        cwd=cwd,
        check=False,
    )


class ApplyReleaseNotesTests(unittest.TestCase):
    def test_rejects_bad_tag(self) -> None:
        r = run_script({"TAG": "canact-v0.1.2", "GH_REPO": "attune-io/attune"})
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("vX.Y.Z", r.stderr)

    def test_missing_source_is_noop(self) -> None:
        r = run_script(
            {"TAG": "v0.1.2", "GH_REPO": "attune-io/attune", "DRY_RUN": "1"}
        )
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("leaving auto notes", r.stdout)

    def test_notes_file_dry_run(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            notes = Path(td) / "RELEASE_NOTES.md"
            notes.write_text("attune 0.1.27 notes\n", encoding="utf-8")
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "DRY_RUN": "1",
                    "NOTES_FILE": str(notes),
                }
            )
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("DRY_RUN: would apply file:", r.stdout)
            self.assertIn("BYTES: 20", r.stdout)
            self.assertNotIn("would delete branch", r.stdout)

    def test_empty_file_is_noop(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            notes = Path(td) / "RELEASE_NOTES.md"
            notes.write_text("", encoding="utf-8")
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "DRY_RUN": "1",
                    "NOTES_FILE": str(notes),
                }
            )
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("leaving auto notes", r.stdout)

    def test_variable_requires_matching_tag(self) -> None:
        r = run_script(
            {
                "TAG": "v0.1.2",
                "GH_REPO": "attune-io/attune",
                "DRY_RUN": "1",
                "RELEASE_NOTES": "stale notes",
                "RELEASE_NOTES_TAG": "v0.1.1",
            }
        )
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("leaving auto notes", r.stdout)

    def test_variable_applies_when_tag_matches(self) -> None:
        r = run_script(
            {
                "TAG": "v0.1.2",
                "GH_REPO": "attune-io/attune",
                "DRY_RUN": "1",
                "RELEASE_NOTES": "from var",
                "RELEASE_NOTES_TAG": "0.1.2",
            }
        )
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Actions variable", r.stdout)
        self.assertIn("DRY_RUN: would apply variable to v0.1.2", r.stdout)
        self.assertNotIn("would delete branch", r.stdout)

    def test_branch_fetch_dry_run_deletes_branch(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            fake_bin = Path(td) / "bin"
            fake_bin.mkdir()
            gh = fake_bin / "gh"
            gh.write_text(
                "#!/bin/bash\n"
                "if [ \"$1\" = api ] && [ \"$2\" != -X ]; then\n"
                "  printf '%s\\n' 'from branch'\n"
                "  exit 0\n"
                "fi\n"
                "exit 1\n",
                encoding="utf-8",
            )
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            env_path = f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}"
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "DRY_RUN": "1",
                    "GH_TOKEN": "test",
                    "PATH": env_path,
                }
            )
            self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
            self.assertIn("release-note-0.1.2", r.stdout)
            self.assertIn("DRY_RUN: would delete branch release-note-0.1.2", r.stdout)

    def test_release_view_fail_skips_edit(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            td_path = Path(td)
            notes = td_path / "RELEASE_NOTES.md"
            notes.write_text("curated notes\n", encoding="utf-8")
            fake_bin = td_path / "bin"
            fake_bin.mkdir()
            log = td_path / "gh.log"
            gh = fake_bin / "gh"
            gh.write_text(
                "#!/bin/bash\n"
                f"printf '%s\\n' \"$*\" >> '{log}'\n"
                "if [ \"$1\" = release ] && [ \"$2\" = view ]; then\n"
                "  exit 1\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            env_path = f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}"
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "NOTES_FILE": str(notes),
                    "GH_TOKEN": "test",
                    "PATH": env_path,
                }
            )
            self.assertEqual(r.returncode, 1, r.stderr + r.stdout)
            self.assertIn("does not exist", r.stderr)
            logged = log.read_text(encoding="utf-8") if log.exists() else ""
            self.assertIn("release view", logged)
            self.assertNotIn("release edit", logged)

    def test_delete_fail_warns_and_exits_zero(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            td_path = Path(td)
            fake_bin = td_path / "bin"
            fake_bin.mkdir()
            log = td_path / "gh.log"
            gh = fake_bin / "gh"
            gh.write_text(
                "#!/bin/bash\n"
                f"printf '%s\\n' \"$*\" >> '{log}'\n"
                "if [ \"$1\" = api ] && [ \"$2\" != -X ]; then\n"
                "  printf '%s\\n' 'from branch'\n"
                "  exit 0\n"
                "fi\n"
                "if [ \"$1\" = release ] && [ \"$2\" = view ]; then\n"
                "  exit 0\n"
                "fi\n"
                "if [ \"$1\" = release ] && [ \"$2\" = edit ]; then\n"
                "  exit 0\n"
                "fi\n"
                "if [ \"$1\" = api ] && [ \"$2\" = -X ] && [ \"$3\" = DELETE ]; then\n"
                "  exit 1\n"
                "fi\n"
                "exit 1\n",
                encoding="utf-8",
            )
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            env_path = f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}"
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "GH_TOKEN": "test",
                    "PATH": env_path,
                }
            )
            self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
            combined = f"{r.stderr}\n{r.stdout}"
            self.assertIn("WARN", combined)
            self.assertIn("could not delete", combined)
            logged = log.read_text(encoding="utf-8") if log.exists() else ""
            self.assertIn("release view", logged)
            self.assertIn("release edit", logged)
            self.assertIn("DELETE", logged)

    def test_failed_branch_fetch_applies_matching_variable(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            td_path = Path(td)
            fake_bin = td_path / "bin"
            fake_bin.mkdir()
            log = td_path / "gh.log"
            notes_seen = td_path / "notes_seen.txt"
            gh = fake_bin / "gh"
            gh.write_text(
                "#!/bin/bash\n"
                f"printf '%s\\n' \"$*\" >> '{log}'\n"
                "if [ \"$1\" = api ] && [ \"$2\" != -X ]; then\n"
                "  printf '%s\\n' "
                "'{\"message\":\"Not Found\","
                "\"documentation_url\":\"https://docs.github.com\"}'\n"
                "  exit 1\n"
                "fi\n"
                "if [ \"$1\" = release ] && [ \"$2\" = view ]; then\n"
                "  exit 0\n"
                "fi\n"
                "if [ \"$1\" = release ] && [ \"$2\" = edit ]; then\n"
                "  while [ $# -gt 0 ]; do\n"
                "    if [ \"$1\" = --notes-file ]; then\n"
                f"      cat \"$2\" > '{notes_seen}'\n"
                "      break\n"
                "    fi\n"
                "    shift\n"
                "  done\n"
                "  exit 0\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            env_path = f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}"
            r = run_script(
                {
                    "TAG": "v0.1.2",
                    "GH_REPO": "attune-io/attune",
                    "GH_TOKEN": "test",
                    "RELEASE_NOTES": "from var after failed fetch",
                    "RELEASE_NOTES_TAG": "v0.1.2",
                    "PATH": env_path,
                }
            )
            self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
            self.assertIn("variable", r.stdout)
            self.assertTrue(notes_seen.is_file(), r.stderr + r.stdout)
            seen = notes_seen.read_text(encoding="utf-8")
            self.assertIn("from var after failed fetch", seen)
            self.assertNotIn("Not Found", seen)
            self.assertNotIn("documentation_url", seen)
            logged = log.read_text(encoding="utf-8") if log.exists() else ""
            self.assertIn("release edit", logged)


if __name__ == "__main__":
    unittest.main()
