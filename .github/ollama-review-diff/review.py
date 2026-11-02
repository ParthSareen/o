#!/usr/bin/env python3
"""Diff-only entry point sharing the existing review lifecycle and validators."""

from pathlib import Path
import signal
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "ollama-review"))
from review import ReviewError, main


if __name__ == "__main__":
    def interrupted(signum, frame):
        raise ReviewError("Diff review was cancelled")

    signal.signal(signal.SIGTERM, interrupted)
    main(mode="diff")
