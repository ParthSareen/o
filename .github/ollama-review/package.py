#!/usr/bin/env python3
"""Build a reproducible, checksummed review-workflow bundle; no network calls."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import zipfile


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / ".github" / "ollama-review"
RUNTIME = (
    ".gitignore", "engine.mjs", "engine.test.mjs", "repository.mjs", "repository.test.mjs",
    "review.py", "test_review.py", "feedback.py", "test_feedback.py", "package.json",
    "package-lock.json", "prompt.md", "security.md", "settings.json", "evaluate.py", "test_evaluate.py",
)
EVALUATION = ("storage.py", "cases.json", "labels.csv", "baseline/api.py", "vulnerable/api.py",
              "fixed/api.py", "clean/api.py")


def bundle(output, profile="general"):
    if profile not in {"general", "security"}:
        raise ValueError("Unknown review profile")
    files = {f".github/ollama-review/{name}": (RUNNER / name).read_bytes() for name in RUNTIME}
    for name in EVALUATION:
        files[f".github/ollama-review/evals/{name}"] = (RUNNER / "evals" / name).read_bytes()
    for name in ("review.py", "prompt.md", "security.md", "settings.json"):
        files[f".github/ollama-review-diff/{name}"] = (ROOT / ".github/ollama-review-diff" / name).read_bytes()
    files[".github/ollama-review/settings.json"] = (json.dumps({"profile": profile, "budget_usd": 5}, indent=2) + "\n").encode()
    files[".github/ollama-review-diff/settings.json"] = files[".github/ollama-review/settings.json"]
    files[".github/workflows/ollama-review.yml"] = (ROOT / ".github/workflows/ollama-review.yml").read_bytes()
    files[".github/workflows/ollama-review-diff.yml"] = (ROOT / ".github/workflows/ollama-review-diff.yml").read_bytes()
    files["docs/ollama-review.md"] = (ROOT / "docs/ollama-review.md").read_bytes()
    targets = sorted(files)
    files["install.py"] = (RUNNER / "install.py").read_bytes()
    files["LICENSE"] = (ROOT / "LICENSE").read_bytes()
    files["README.md"] = b"Run python3 install.py --target /path/to/repository to preview. Add --apply to install. See docs/ollama-review.md.\n"
    base = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    manifest = {"format": 1, "base_commit": base, "profile": profile, "install": targets,
                "files": {path: hashlib.sha256(data).hexdigest() for path, data in sorted(files.items())}}
    files["bundle.json"] = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    with zipfile.ZipFile(output, "x", compression=zipfile.ZIP_DEFLATED) as archive:
        for path, data in sorted(files.items()):
            info = zipfile.ZipInfo(path, date_time=(1980, 1, 1, 0, 0, 0))
            info.external_attr = 0o100644 << 16
            archive.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED)
    return hashlib.sha256(Path(output).read_bytes()).hexdigest()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("--profile", choices=["general", "security"], default="general")
    args = parser.parse_args()
    print(f"{bundle(args.output, args.profile)}  {args.output.name}")
