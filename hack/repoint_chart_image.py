#!/usr/bin/env python3
"""Rewrite artifacthub.io/images to the operator image a chart installs.

Artifact Hub scans the image named in that annotation. Published charts
still say ghcr.io/attune-io/attune:latest, so a moving tag is scanned
once per chart version. The replacement is the rendered default image
when that tag exists. When the rendered tag was never published, the
v-prefixed (or unprefixed) tag that does exist is used instead.
"""

import argparse
import re
import sys

IMAGE_RE = re.compile(r"ghcr\.io/attune-io/attune:([A-Za-z0-9][A-Za-z0-9._-]*)")
VERSION_RE = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
KEY_RE = re.compile(r"^(\s*)artifacthub\.io/images:\s*(.*)$")


def is_chart_version(tag):
    """True for a chart tag such as 0.1.26, not a digest or artifacthub.io."""
    return VERSION_RE.fullmatch(tag or "") is not None


def candidate_tags(rendered):
    """Return the rendered tag and its v-flipped alternate."""
    tag = _tag(rendered)
    alt = tag[1:] if tag.startswith("v") else "v" + tag
    return [tag, alt]


def choose_image(rendered, existing_tags):
    """Pick the annotation image.

    existing_tags is an iterable of tags that are present in the registry.
    latest is never chosen. A missing rendered tag falls back to the
    v-flipped tag when that one exists.
    """
    tag = _tag(rendered)
    if tag == "latest":
        raise ValueError("rendered image tag is latest")
    existing = set(existing_tags)
    if tag in existing:
        return "ghcr.io/attune-io/attune:" + tag
    alt = candidate_tags(rendered)[1]
    if alt in existing:
        return "ghcr.io/attune-io/attune:" + alt
    raise ValueError("neither %s nor %s is published" % (tag, alt))


def rendered_images(template_text):
    """Unique attune operator images in helm template output."""
    found = []
    seen = set()
    for match in IMAGE_RE.finditer(template_text or ""):
        image = match.group(0)
        if image not in seen:
            seen.add(image)
            found.append(image)
    return found


def replace_annotation_image(text, image):
    """Return Chart.yaml text with the annotation image replaced.

    Only the artifacthub.io/images value is edited. Other annotations,
    comments, and image references outside that value stay as they are.
    """
    _tag(image)
    if _tag(image) == "latest":
        raise ValueError("refusing to write :latest")
    lines = text.splitlines(keepends=True)
    start, end = _annotation_span(lines)
    blob = "".join(lines[start:end])
    if not IMAGE_RE.search(blob):
        raise ValueError("artifacthub.io/images has no attune image")
    new_blob, count = IMAGE_RE.subn(image, blob)
    if count < 1:
        raise ValueError("artifacthub.io/images has no attune image")
    lines[start:end] = [new_blob]
    return "".join(lines)


def annotation_image(text):
    """The attune image currently stored in the annotation, if any."""
    lines = text.splitlines(keepends=True)
    try:
        start, end = _annotation_span(lines)
    except ValueError:
        return ""
    match = IMAGE_RE.search("".join(lines[start:end]))
    if not match:
        return ""
    return match.group(0)


def _tag(image):
    match = IMAGE_RE.fullmatch(image or "")
    if not match:
        raise ValueError("not an attune operator image: %r" % (image,))
    return match.group(1)


def _annotation_span(lines):
    for index, line in enumerate(lines):
        stripped = line.rstrip("\r\n")
        match = KEY_RE.match(stripped)
        if not match:
            continue
        indent = len(match.group(1))
        rest = match.group(2).strip()
        if rest.startswith("|") or rest.startswith(">"):
            end = index + 1
            while end < len(lines):
                content = lines[end].rstrip("\r\n")
                if content.strip() == "":
                    end += 1
                    continue
                content_indent = len(content) - len(content.lstrip(" "))
                if content_indent <= indent:
                    break
                end += 1
            return index, end
        return index, index + 1
    raise ValueError("Chart.yaml has no artifacthub.io/images annotation")


def _apply_file(path, image):
    original = open(path, encoding="utf-8").read()
    old = annotation_image(original)
    updated = replace_annotation_image(original, image)
    changed = updated != original
    if changed:
        with open(path, "w", encoding="utf-8", newline="") as handle:
            handle.write(updated)
    print("old=%s" % old)
    print("new=%s" % image)
    print("changed=%s" % ("yes" if changed else "no"))


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)

    apply_cmd = sub.add_parser("apply")
    apply_cmd.add_argument("--file", required=True)
    apply_cmd.add_argument("--image", required=True)

    choose_cmd = sub.add_parser("choose")
    choose_cmd.add_argument("--rendered", required=True)
    choose_cmd.add_argument("--exists", action="append", default=[])

    rendered_cmd = sub.add_parser("rendered")
    rendered_cmd.add_argument("template", nargs="?")

    version_cmd = sub.add_parser("version-ok")
    version_cmd.add_argument("tag")

    candidates_cmd = sub.add_parser("candidates")
    candidates_cmd.add_argument("--rendered", required=True)

    args = parser.parse_args(argv)
    try:
        if args.cmd == "apply":
            _apply_file(args.file, args.image)
        elif args.cmd == "choose":
            print(choose_image(args.rendered, args.exists))
        elif args.cmd == "rendered":
            text = sys.stdin.read() if not args.template else open(args.template, encoding="utf-8").read()
            images = rendered_images(text)
            if len(images) != 1:
                raise ValueError("expected one attune image, found %s" % (images,))
            print(images[0])
        elif args.cmd == "version-ok":
            print("yes" if is_chart_version(args.tag) else "no")
        elif args.cmd == "candidates":
            for tag in candidate_tags(args.rendered):
                print(tag)
    except ValueError as exc:
        print("FAIL: %s" % exc, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
