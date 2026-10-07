"""Stage tracked working-tree source and the pinned 2ai module for Cloud Build.

GitHub credentials stay on the operator's host. Only dependency source enters
the build context; the local replace exists only in its temporary go.mod.
"""
import argparse
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/2found/2ai"


def prepare(output, root=ROOT):
    output = Path(output).resolve()
    if output.exists():
        raise ValueError("Build context must be a new directory")
    if output == root or root in output.parents:
        raise ValueError("Build context must be outside the source checkout")
    module = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", MODULE], cwd=root, text=True))
    if module.get("Replace") or not module.get("Version"):
        raise ValueError("2ai must be pinned; remove local replacements before building")
    source = Path(module["Dir"])
    files = subprocess.check_output(
        ["git", "ls-files", "-z"], cwd=root).decode().split("\0")
    output.mkdir(parents=True)
    for name in filter(None, files):
        relative = Path(name)
        if any(part == ".env" or part.startswith(".env.") and part != ".env.example"
               or part == "secret.env" for part in relative.parts):
            continue
        original = root / relative
        if not original.is_file():
            continue  # Deleted source or an unrelated submodule gitlink.
        target = output / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(original, target)
    shutil.copytree(source, output / ".go-deps/2ai")
    (output / ".go-deps/source.json").write_text(json.dumps(
        {key: module[key] for key in ("Path", "Version", "Sum") if key in module},
        indent=2) + "\n")
    subprocess.run(["go", "mod", "edit",
                    f"-replace={MODULE}=./.go-deps/2ai"], cwd=output, check=True)
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", help="New staging directory outside the checkout")
    args = parser.parse_args()
    print(prepare(args.output))
