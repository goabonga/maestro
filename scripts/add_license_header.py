#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Add or check the two-line SPDX header across the repository.

The comment prefix is chosen per file extension so the same header convention
applies to Go (``//``) as well as Python / YAML / TOML / shell (``#``):

    // SPDX-License-Identifier: MIT          # SPDX-License-Identifier: MIT
    // Copyright (c) 2026 Chris ...          # Copyright (c) 2026 Chris ...

The header is inserted at the top of the file, after a ``#!`` shebang when one
is present. For Go files that open with a ``//go:build`` constraint the header
is still placed first: line comments are allowed before a build constraint, and
the constraint keeps its trailing blank line, so the file stays valid.
"""

import argparse
import os
import sys

SPDX = "SPDX-License-Identifier: MIT"
COPYRIGHT = "Copyright (c) 2026 Chris <goabonga@pm.me>"

# Comment prefix by file extension (without the leading dot).
PREFIX_BY_EXT = {
    "go": "//",
    "ts": "//",
    "tsx": "//",
    "js": "//",
    "jsx": "//",
    "mjs": "//",
    "cjs": "//",
    "py": "#",
    "sh": "#",
    "bash": "#",
    "yml": "#",
    "yaml": "#",
    "toml": "#",
}
DEFAULT_PREFIX = "#"


def prefix_for(ext: str) -> str:
    return PREFIX_BY_EXT.get(ext.lower(), DEFAULT_PREFIX)


def header_lines(prefix: str) -> list[str]:
    return [f"{prefix} {SPDX}", f"{prefix} {COPYRIGHT}"]


def is_shebang(line: str) -> bool:
    return line.startswith("#!")


def has_license(lines: list[str], header: list[str]) -> bool:
    stripped = [line.rstrip("\n") for line in lines]
    if stripped and is_shebang(stripped[0]):
        return stripped[2 : 2 + len(header)] == header
    return stripped[: len(header)] == header


def add_license_header(file_path: str, prefix: str) -> bool:
    header = header_lines(prefix)
    with open(file_path, encoding="utf-8") as f:
        lines = f.readlines()

    if has_license(lines, header):
        return False  # already compliant

    body = [line.rstrip("\n") for line in lines]
    new_lines: list[str] = []
    if body and is_shebang(body[0]):
        new_lines.append(body[0])
        new_lines.append("")
        new_lines.extend(header)
        new_lines.append("")
        new_lines.extend(body[1:])
    else:
        new_lines.extend(header)
        new_lines.append("")
        new_lines.extend(body)

    with open(file_path, "w", encoding="utf-8") as f:
        f.write("\n".join(new_lines) + "\n")

    print(f"License header added to: {file_path}")
    return True


def check_license(file_path: str, prefix: str) -> bool:
    with open(file_path, encoding="utf-8") as f:
        lines = f.readlines()
    return has_license(lines, header_lines(prefix))


def matches(filename: str, extensions: list[str]) -> bool:
    if not extensions:
        return True
    ext = os.path.splitext(filename)[1]
    if ext == "" and "none" in extensions:
        return True
    return any(filename.endswith(f".{e}") for e in extensions)


def process_directory(root: str, extensions: list[str], check_only: bool) -> int:
    missing: list[str] = []
    for dirpath, directories, filenames in os.walk(root):
        directories[:] = [name for name in directories if name not in {".venv", "__pycache__", ".pytest_cache", "node_modules", "dist", "coverage"}]
        for filename in filenames:
            if not matches(filename, extensions):
                continue
            path = os.path.join(dirpath, filename)
            ext = os.path.splitext(filename)[1].lstrip(".")
            prefix = prefix_for(ext)
            if check_only:
                if not check_license(path, prefix):
                    missing.append(path)
            else:
                add_license_header(path, prefix)

    if check_only:
        if missing:
            print("Missing license headers in:")
            for path in missing:
                print(f" - {path}")
            return 1
        print("All files have license headers.")
    return 0


def main() -> None:
    parser = argparse.ArgumentParser(description="Add or check SPDX license headers.")
    parser.add_argument(
        "--path", type=str, default=".", help="Root directory to process"
    )
    parser.add_argument(
        "--types",
        type=str,
        default="go",
        help=(
            "Comma-separated list of file extensions (e.g. go,py,sh). "
            "Use 'none' to include files without extension, or pass an empty "
            "string to process all files regardless of extension."
        ),
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help=(
            "Only check for missing license headers without modifying files. "
            "Exits 1 if any are missing."
        ),
    )
    args = parser.parse_args()

    extensions = [ext.strip() for ext in args.types.split(",") if ext.strip()]
    sys.exit(process_directory(args.path, extensions, check_only=args.check))


if __name__ == "__main__":
    main()
