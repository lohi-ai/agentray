#!/usr/bin/env python3
"""Verify the complete pinned Pi source, optionally against an upstream checkout."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys


def verify(root: Path, checkout: str | None = None, work: bool = False) -> list[str]:
    manifest = json.loads((root / "UPSTREAM.json").read_text())
    expected = manifest["files"]
    source = root / (".work" if work else "upstream")
    actual = {str(p.relative_to(source)) for p in source.rglob("*") if p.is_file() or p.is_symlink()}
    if work:
        # Pi generates this untracked catalog before running its tests. No
        # generated files are allowed inside either requested package.
        actual = {p for p in actual if not (
            p.startswith("packages/ai/src/providers/data/") and p.endswith(".json")
        )}
    errors = [f"missing: {p}" for p in sorted(expected.keys() - actual)]
    errors += [f"unexpected: {p}" for p in sorted(actual - expected.keys())]
    for name in sorted(actual & expected.keys()):
        path = source / name
        if path.is_symlink():
            errors.append(f"unexpected symlink: {name}")
            continue
        data = path.read_bytes()
        blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
        if blob != expected[name]["git_blob"] or hashlib.sha256(data).hexdigest() != expected[name]["sha256"]:
            errors.append(f"content mismatch: {name}")
        executable = bool(path.stat().st_mode & 0o111)
        if executable != (expected[name]["mode"] == "100755"):
            errors.append(f"mode mismatch: {name}")
    if checkout:
        # The remote commit's tree is authoritative, not our local manifest.
        output = subprocess.check_output([
            "git", "-C", checkout, "ls-tree", "-r", "-z", manifest["commit"],
            "--", *manifest["roots"],
        ])
        upstream = {}
        for entry in output.split(b"\0"):
            if not entry:
                continue
            meta, name = entry.split(b"\t", 1)
            mode, kind, oid = meta.decode().split()
            upstream[name.decode()] = (mode, kind, oid)
        recorded = {name: (info["mode"], "blob", info["git_blob"]) for name, info in expected.items()}
        if upstream != recorded:
            errors.append("manifest does not match the pinned upstream tree")
    return errors


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream", help="Git checkout containing the pinned upstream commit")
    parser.add_argument("--work", action="store_true", help="Verify the test copy, allowing generated AI model JSON")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    errors = verify(root, args.upstream, args.work)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        sys.exit(1)
    manifest = json.loads((root / "UPSTREAM.json").read_text())
    print(f"Verified {len(manifest['files'])} byte-identical files at {manifest['commit']}")
