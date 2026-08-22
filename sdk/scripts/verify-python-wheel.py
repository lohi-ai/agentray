"""Assert the built wheel contains the package, not just metadata.

Run from inside sdk/python after `python -m build`.

The repo-root .gitignore carries `/agentray` for the compiled Go binary. Git
anchors that to the repo root, so the Python package is correctly tracked --
but hatchling re-anchors the same pattern to sdk/python/, where it matches the
package itself. With VCS ignores on, `python -m build` cheerfully produced a
wheel containing metadata, a licence, and no code. It passed `twine check`. It
failed at `import agentray`, which is to say: at the first user.
"""
import glob
import sys
import zipfile

REQUIRED = ["agentray/__init__.py", "agentray/client.py", "agentray/py.typed"]

wheels = glob.glob("dist/*.whl")
if not wheels:
    sys.exit("no wheel in dist/ -- run `python -m build` first")

wheel = wheels[0]
names = zipfile.ZipFile(wheel).namelist()
missing = [n for n in REQUIRED if n not in names]
if missing:
    sys.exit(
        f"{wheel} is missing {missing}\npresent: {names}\nsee docs/RELEASING-SDK.md"
    )

print(f"wheel ok: {wheel}, {len(names)} files")
