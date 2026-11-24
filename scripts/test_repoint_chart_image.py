#!/usr/bin/env python3
"""Pin artifacthub.io/images without touching the rest of Chart.yaml."""

import unittest
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "hack"))
import repoint_chart_image as repoint  # noqa: E402


BLOCK = """\
annotations:
  artifacthub.io/crds: |
    - kind: AttunePolicy
      version: v1alpha1
  artifacthub.io/images: |
    - name: attune
      image: ghcr.io/attune-io/attune:latest
    - name: sidecar
      image: ghcr.io/example/sidecar:1
  artifacthub.io/license: Apache-2.0
  artifacthub.io/links: |
    - name: Source
      url: https://github.com/attune-io/attune
version: 0.1.26
"""

QUOTED = (
    '  artifacthub.io/links: |\n'
    '    - name: Source\n'
    '      url: https://github.com/attune-io/attune\n'
    '  # x-release-please-version keeps this tag on appVersion. The marker\n'
    '  # stays outside the quoted string so Artifact Hub does not see it.\n'
    '  artifacthub.io/images: "- name: attune\\n  image: ghcr.io/attune-io/attune:0.1.39" '
    '# x-release-please-version\n'
)


class ReplaceTests(unittest.TestCase):
    def test_block_scalar_replaces_only_the_attune_image(self):
        got = repoint.replace_annotation_image(BLOCK, "ghcr.io/attune-io/attune:0.1.26")
        self.assertIn("image: ghcr.io/attune-io/attune:0.1.26", got)
        self.assertNotIn("attune:latest", got)
        self.assertIn("image: ghcr.io/example/sidecar:1", got)
        self.assertIn("artifacthub.io/license: Apache-2.0", got)
        self.assertIn("kind: AttunePolicy", got)
        self.assertIn("url: https://github.com/attune-io/attune", got)
        self.assertIn("version: 0.1.26", got)

    def test_quoted_annotation_keeps_the_release_please_marker(self):
        got = repoint.replace_annotation_image(QUOTED, "ghcr.io/attune-io/attune:0.1.40")
        self.assertIn("ghcr.io/attune-io/attune:0.1.40", got)
        self.assertNotIn("0.1.39", got)
        self.assertIn("# x-release-please-version", got)
        self.assertIn("url: https://github.com/attune-io/attune", got)

    def test_same_image_is_a_no_op(self):
        original = BLOCK.replace(":latest", ":0.1.26")
        got = repoint.replace_annotation_image(original, "ghcr.io/attune-io/attune:0.1.26")
        self.assertEqual(got, original)

    def test_refuses_latest_and_a_missing_annotation(self):
        with self.assertRaises(ValueError):
            repoint.replace_annotation_image(BLOCK, "ghcr.io/attune-io/attune:latest")
        with self.assertRaises(ValueError):
            repoint.replace_annotation_image("version: 0.1.26\n", "ghcr.io/attune-io/attune:0.1.26")


class ChooseTests(unittest.TestCase):
    def test_rendered_tag_wins_when_it_exists(self):
        image = "ghcr.io/attune-io/attune:0.1.26"
        self.assertEqual(repoint.choose_image(image, ["0.1.26", "v0.1.26"]), image)

    def test_v_prefix_helper_keeps_the_rendered_tag(self):
        image = "ghcr.io/attune-io/attune:v0.1.24"
        self.assertEqual(repoint.choose_image(image, ["0.1.24", "v0.1.24"]), image)

    def test_missing_bare_tag_falls_back_to_the_published_v_tag(self):
        got = repoint.choose_image("ghcr.io/attune-io/attune:0.1.23", ["v0.1.23"])
        self.assertEqual(got, "ghcr.io/attune-io/attune:v0.1.23")

    def test_latest_and_unknown_tags_fail(self):
        with self.assertRaises(ValueError):
            repoint.choose_image("ghcr.io/attune-io/attune:latest", ["latest"])
        with self.assertRaises(ValueError):
            repoint.choose_image("ghcr.io/attune-io/attune:0.1.23", [])
        with self.assertRaises(ValueError):
            repoint.choose_image("ghcr.io/example/other:1", ["1"])

    def test_chart_version_filter_skips_digests_and_metadata(self):
        self.assertTrue(repoint.is_chart_version("0.1.26"))
        self.assertFalse(repoint.is_chart_version("v0.1.26"))
        self.assertFalse(repoint.is_chart_version("artifacthub.io"))
        self.assertFalse(repoint.is_chart_version("latest"))
        self.assertFalse(repoint.is_chart_version("sha256-abc.sig"))


class RenderedTests(unittest.TestCase):
    def test_one_image_from_template_output(self):
        text = 'image: "ghcr.io/attune-io/attune:0.1.26"\nimage: "ghcr.io/example/sidecar:1"\n'
        self.assertEqual(
            repoint.rendered_images(text),
            ["ghcr.io/attune-io/attune:0.1.26"],
        )

    def test_duplicate_lines_count_once(self):
        text = "image: ghcr.io/attune-io/attune:0.1.39\nimage: ghcr.io/attune-io/attune:0.1.39\n"
        self.assertEqual(repoint.rendered_images(text), ["ghcr.io/attune-io/attune:0.1.39"])


if __name__ == "__main__":
    unittest.main()
