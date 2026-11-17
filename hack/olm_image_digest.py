#!/usr/bin/env python3
"""Print the registry digest from `docker manifest inspect -v` JSON."""

import json
import sys


def digest_from_inspect(payload):
    """Return a sha256 digest, or an empty string when none is present."""
    data = json.loads(payload)
    if isinstance(data, dict):
        return _descriptor_digest(data)
    if isinstance(data, list):
        for item in data:
            if not isinstance(item, dict):
                continue
            media = ((item.get("Descriptor") or {}).get("mediaType") or "")
            if "manifest.list" in media or media.endswith("image.index.v1+json"):
                found = _descriptor_digest(item)
                if found:
                    return found
        for item in data:
            if isinstance(item, dict):
                found = _descriptor_digest(item)
                if found:
                    return found
    return ""


def _descriptor_digest(item):
    digest = ((item.get("Descriptor") or {}).get("digest") or "")
    if digest.startswith("sha256:"):
        return digest
    return ""


def main():
    digest = digest_from_inspect(sys.stdin.read())
    if not digest:
        print("ERROR: manifest inspect JSON has no Descriptor.digest", file=sys.stderr)
        return 1
    print(digest)
    return 0


if __name__ == "__main__":
    sys.exit(main())
