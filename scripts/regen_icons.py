#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Regenerate documentation branding from the canonical SVG."""

from pathlib import Path
import shutil
import subprocess


def main():
    root = Path(__file__).resolve().parent.parent
    source = root / "assets/maestro.svg"
    if not source.is_file():
        raise SystemExit(f"Missing canonical SVG: {source}")
    shutil.copyfile(source, root / "docs/maestro.svg")
    subprocess.run(["uv", "run", "--project", str(root / "scripts"), "--locked",
                    "--no-dev", "python", str(root / "scripts/generate_favicon.py"),
                    "-i", str(root / "docs/maestro.svg"), "-o", str(root / "docs/favicon.ico")],
                   check=True)


if __name__ == "__main__":
    main()
