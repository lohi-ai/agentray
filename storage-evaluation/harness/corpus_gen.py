"""Subprocess entrypoint for bounded corpus generation.

`run_leg` spawns this as a child process so a hung or disk-filling
generation can be killed — an in-process generate() cannot be cancelled
by the engine watchdog (no engine exists yet) and a Timer cannot
preempt Python code.
"""
import sys

from .corpus import generate

if __name__ == "__main__":
    scale, seed, ingest_rows = (int(sys.argv[1]), int(sys.argv[2]),
                                int(sys.argv[3]))
    out = generate(scale, seed, ingest_rows)
    print(out)
