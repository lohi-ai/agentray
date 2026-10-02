#!/usr/bin/env python3
"""Prepare a disposable test copy without changing the pinned upstream source."""

from pathlib import Path
import shutil

from verify import verify

root = Path(__file__).resolve().parent
errors = verify(root)
if errors:
    raise SystemExit("\n".join(errors))
target = root / ".work"
if target.exists():
    shutil.rmtree(target)
shutil.copytree(root / "upstream", target)
print("Prepared .work; hydrate its model catalog before running the upstream tests.")
