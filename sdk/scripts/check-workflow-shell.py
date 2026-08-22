"""Parse every `run:` block in the SDK workflows with the shell that will run it.

Run on macOS, where /bin/bash is still 3.2 -- the same interpreter GitHub's
macOS runners use.

This exists because of a real failed release. The Swift job wrote its release
notes with `NOTES=$(cat <<EOF ... EOF)`, and bash 3.2 scans a heredoc body for
quotes while looking for the closing paren of a command substitution. One
apostrophe in prose ("the mirror's root") was an unterminated quote, and the step
exited 2 before `gh` was ever called. bash 5 on the Linux runners tolerates it,
so the npm and Python jobs passed on the luck of which prose they happened to
contain rather than on being correct -- and the failure was invisible until a tag
was pushed, because a workflow is not run by anything until then.

`${{ }}` expressions are replaced with a placeholder: this checks shell grammar,
not expression values.
"""
import glob
import os
import re
import subprocess
import sys
import tempfile

import yaml

WORKFLOWS = sorted(glob.glob(os.path.join(os.path.dirname(__file__), "../../.github/workflows/sdk*.yml")))
SHELLS = [s for s in ("/bin/bash", "/opt/homebrew/bin/bash", "/usr/local/bin/bash") if os.path.exists(s)]

failures = []
checked = 0

for wf in WORKFLOWS:
    doc = yaml.safe_load(open(wf))
    for job_name, job in (doc.get("jobs") or {}).items():
        for step in job.get("steps") or []:
            run = step.get("run")
            if not run:
                continue
            script = re.sub(r"\$\{\{(.*?)\}\}", "X", run)
            with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as fh:
                fh.write(script)
                path = fh.name
            label = step.get("name") or run.splitlines()[0][:40]
            for shell in SHELLS:
                checked += 1
                proc = subprocess.run([shell, "-n", path], capture_output=True, text=True)
                if proc.returncode != 0:
                    ver = subprocess.run([shell, "-c", "echo $BASH_VERSION"], capture_output=True, text=True).stdout.strip()
                    failures.append(
                        f"{os.path.basename(wf)} :: {job_name} :: {label}\n"
                        f"    bash {ver} ({shell}): {proc.stderr.strip().splitlines()[0] if proc.stderr.strip() else 'parse error'}"
                    )
            os.unlink(path)

if not SHELLS:
    sys.exit("no bash found to check with")

if failures:
    print("\n".join(failures))
    sys.exit(f"\n{len(failures)} run block(s) will not parse on a runner")

print(f"ok: every run block parses ({checked} shell checks across {len(WORKFLOWS)} workflows, shells: {', '.join(SHELLS)})")
