#!/usr/bin/env python3
"""The OLM digest parser reads Descriptor.digest, not a hash of the JSON."""

import json
import unittest

from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "hack"))
import olm_image_digest  # noqa: E402


SINGLE = {
    "Descriptor": {
        "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
    }
}

LIST = [
    {
        "Descriptor": {
            "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
            "mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
        }
    },
    {
        "Descriptor": {
            "digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
            "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
        }
    },
]


class DigestTests(unittest.TestCase):
    def test_single_manifest(self):
        got = olm_image_digest.digest_from_inspect(json.dumps(SINGLE))
        self.assertEqual(got, SINGLE["Descriptor"]["digest"])

    def test_manifest_list_prefers_the_index(self):
        got = olm_image_digest.digest_from_inspect(json.dumps(LIST))
        self.assertEqual(got, LIST[0]["Descriptor"]["digest"])

    def test_hashed_json_is_not_the_digest(self):
        import hashlib
        raw = json.dumps(SINGLE)
        hashed = "sha256:" + hashlib.sha256(raw.encode()).hexdigest()
        got = olm_image_digest.digest_from_inspect(raw)
        self.assertNotEqual(got, hashed)


if __name__ == "__main__":
    unittest.main()
