#!/usr/bin/env python3
"""Preview or install a verified bundle without replacing existing files."""

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath


def checked_path(root, name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or str(path) != name or "\\" in name:
        raise ValueError("Invalid bundle path")
    target = root.joinpath(*path.parts)
    for part in [target, *target.parents]:
        if part == root.parent:
            break
        if part.is_symlink():
            raise ValueError(f"Refusing symlink: {name}")
    return target


def install(source, destination, apply=False):
    if source.is_symlink() or destination.is_symlink() or not destination.is_dir():
        raise ValueError("Source and destination must be real directories")
    source, destination = source.resolve(), destination.resolve()
    manifest = json.loads((source / "bundle.json").read_text())
    if manifest.get("format") != 1 or not isinstance(manifest.get("files"), dict):
        raise ValueError("Unsupported bundle manifest")
    files = {}
    for name, digest in manifest["files"].items():
        path = checked_path(source, name)
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != digest:
            raise ValueError(f"Checksum mismatch: {name}")
        files[name] = data
    pending = []
    for name in manifest["install"]:
        if not (name.startswith((".github/ollama-review/", ".github/ollama-review-diff/")) or name in {
                ".github/workflows/ollama-review.yml", ".github/workflows/ollama-review-diff.yml", "docs/ollama-review.md"}):
            raise ValueError("Unexpected installation target")
        target = checked_path(destination, name)
        if target.exists():
            if not target.is_file() or target.read_bytes() != files[name]:
                raise ValueError(f"Existing file differs; review manually: {name}")
        else:
            pending.append((name, target, files[name]))
    if apply:
        for name, target, data in pending:
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as output:
                output.write(data)
    return [name for name, _, _ in pending]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", type=Path, required=True)
    parser.add_argument("--apply", action="store_true", help="Write missing files after all checks pass")
    args = parser.parse_args()
    try:
        paths = install(Path(__file__).resolve().parent, args.target, args.apply)
    except (ValueError, OSError, KeyError) as error:
        parser.exit(1, f"Install refused: {error}\n")
    print(("Installed" if args.apply else "Would install") + f" {len(paths)} files.")
    print("\n".join(paths))
    if not args.apply:
        print("Preview only. Review the files, then add --apply to install.")
