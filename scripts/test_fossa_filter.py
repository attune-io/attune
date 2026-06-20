#!/usr/bin/env python3
"""Tests for scripts/fossa-filter.py."""

import json
import os
import sys
import tempfile
import unittest

# Import the module under test from the scripts directory.
sys.path.insert(0, os.path.join(os.path.dirname(__file__)))
from importlib import import_module

ff = import_module("fossa-filter")

extract_package = ff.extract_package
is_false_positive = ff.is_false_positive
filter_via_text = ff.filter_via_text
main = ff.main


class TestExtractPackage(unittest.TestCase):
    def test_go_module(self):
        self.assertEqual(
            extract_package("go+k8s.io/client-go$v0.36.2"),
            "k8s.io/client-go",
        )

    def test_nested_path(self):
        self.assertEqual(
            extract_package("go+golang.org/x/text$v0.37.0"),
            "golang.org/x/text",
        )

    def test_no_ecosystem(self):
        self.assertEqual(extract_package("k8s.io/client-go$v1.0"), "k8s.io/client-go")

    def test_no_version(self):
        self.assertEqual(extract_package("go+k8s.io/client-go"), "k8s.io/client-go")

    def test_plain_string(self):
        self.assertEqual(extract_package("some-package"), "some-package")


class TestIsFalsePositive(unittest.TestCase):
    def test_k8s_outdated(self):
        self.assertTrue(is_false_positive({
            "revisionId": "go+k8s.io/client-go$v0.36.2",
            "type": "outdated",
        }))

    def test_k8s_apimachinery_outdated(self):
        self.assertTrue(is_false_positive({
            "revisionId": "go+k8s.io/apimachinery$v0.36.2",
            "type": "outdated",
        }))

    def test_golang_text_ccbysa(self):
        self.assertTrue(is_false_positive({
            "revisionId": "go+golang.org/x/text$v0.37.0",
            "type": "policy_conflict",
            "license": "CC-BY-SA-4.0",
        }))

    def test_golang_crypto_openssl(self):
        self.assertTrue(is_false_positive({
            "revisionId": "go+golang.org/x/crypto$v0.36.0",
            "type": "policy_conflict",
            "license": "openssl-ssleay",
        }))

    def test_genuine_gpl_issue(self):
        self.assertFalse(is_false_positive({
            "revisionId": "go+github.com/evil/pkg$v1.0.0",
            "type": "policy_conflict",
            "license": "GPL-3.0",
        }))

    def test_genuine_outdated_non_k8s(self):
        self.assertFalse(is_false_positive({
            "revisionId": "go+github.com/some/lib$v1.0.0",
            "type": "outdated",
        }))

    def test_empty_issue(self):
        self.assertFalse(is_false_positive({}))

    def test_licenseId_field(self):
        """FOSSA sometimes uses licenseId instead of license."""
        self.assertTrue(is_false_positive({
            "revisionId": "go+golang.org/x/text$v0.37.0",
            "type": "policy_flag",
            "licenseId": "CC-BY-SA-3.0",
        }))


class TestFilterViaText(unittest.TestCase):
    def _write_tmp(self, content):
        f = tempfile.NamedTemporaryFile(mode="w", suffix=".txt", delete=False)
        f.write(content)
        f.close()
        self.addCleanup(os.unlink, f.name)
        return f.name

    def test_k8s_outdated_filtered(self):
        path = self._write_tmp(
            "⚑ Outdated dependency detected in k8s.io/client-go@v0.36.2\n"
        )
        self.assertEqual(filter_via_text(path), 0)

    def test_genuine_issue_fails(self):
        path = self._write_tmp(
            "⚑ Outdated dependency detected in github.com/evil/pkg@v1.0\n"
        )
        self.assertEqual(filter_via_text(path), 1)

    def test_mixed_issues(self):
        path = self._write_tmp(
            "⚑ Outdated dependency detected in k8s.io/client-go@v0.36.2\n"
            "⚑ Outdated dependency detected in github.com/evil/pkg@v1.0\n"
        )
        self.assertEqual(filter_via_text(path), 1)

    def test_empty_file(self):
        path = self._write_tmp("")
        self.assertEqual(filter_via_text(path), 1)

    def test_missing_file(self):
        self.assertEqual(filter_via_text("/nonexistent/file.txt"), 1)


class TestMain(unittest.TestCase):
    def _write_tmp(self, content, suffix=".json"):
        f = tempfile.NamedTemporaryFile(mode="w", suffix=suffix, delete=False)
        f.write(content)
        f.close()
        self.addCleanup(os.unlink, f.name)
        return f.name

    def test_all_false_positives_exit_0(self):
        path = self._write_tmp(json.dumps([
            {"revisionId": "go+k8s.io/client-go$v0.36.2", "type": "outdated"},
        ]))
        sys.argv = ["fossa-filter.py", path]
        self.assertEqual(main(), 0)

    def test_genuine_issue_exit_1(self):
        path = self._write_tmp(json.dumps([
            {"revisionId": "go+github.com/evil/pkg$v1.0.0", "type": "policy_conflict", "license": "GPL-3.0"},
        ]))
        sys.argv = ["fossa-filter.py", path]
        self.assertEqual(main(), 1)

    def test_mixed_issues_exit_1(self):
        path = self._write_tmp(json.dumps([
            {"revisionId": "go+k8s.io/client-go$v0.36.2", "type": "outdated"},
            {"revisionId": "go+github.com/evil/pkg$v1.0.0", "type": "policy_conflict", "license": "GPL-3.0"},
        ]))
        sys.argv = ["fossa-filter.py", path]
        self.assertEqual(main(), 1)

    def test_empty_json_with_text_fallback(self):
        json_path = self._write_tmp("")
        stderr_path = self._write_tmp(
            "⚑ Outdated dependency detected in k8s.io/client-go@v0.36.2\n",
            suffix=".txt",
        )
        sys.argv = ["fossa-filter.py", json_path, stderr_path]
        self.assertEqual(main(), 0)

    def test_malformed_json_with_text_fallback(self):
        json_path = self._write_tmp("not valid json {{{")
        stderr_path = self._write_tmp(
            "⚑ Outdated dependency detected in k8s.io/client-go@v0.36.2\n",
            suffix=".txt",
        )
        sys.argv = ["fossa-filter.py", json_path, stderr_path]
        self.assertEqual(main(), 0)

    def test_no_issues_exit_0(self):
        path = self._write_tmp(json.dumps([]))
        sys.argv = ["fossa-filter.py", path]
        self.assertEqual(main(), 0)

    def test_missing_file_exit_1(self):
        sys.argv = ["fossa-filter.py", "/nonexistent/results.json"]
        self.assertEqual(main(), 1)

    def test_issues_in_dict_format(self):
        """fossa test may return {issues: [...]} instead of a bare list."""
        path = self._write_tmp(json.dumps({
            "issues": [{"revisionId": "go+k8s.io/api$v0.36.2", "type": "outdated"}]
        }))
        sys.argv = ["fossa-filter.py", path]
        self.assertEqual(main(), 0)


if __name__ == "__main__":
    unittest.main()
