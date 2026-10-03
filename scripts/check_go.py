#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Validate a registered Go executable and its actual internal dependencies."""

import argparse
from pathlib import Path
import subprocess
import tomllib

from ci_automation import run


def packages(component):
    config = tomllib.loads(Path("multicz.toml").read_text())
    declared = config["plugins"]["go-deps"]["packages"]
    if component not in declared:
        raise ValueError(f"Unknown Go component: {component}")
    entry = declared[component]
    entry = [entry] if isinstance(entry, str) else entry
    module = run("go", "list", "-m")
    paths = run("go", "list", "-deps", "-f", "{{.ImportPath}}", *entry).splitlines()
    owned = sorted(path for path in set(paths) if path.startswith(module + "/"))
    if not owned:
        raise ValueError(f"No Go packages resolved for {component}")
    return entry, owned


def check(component):
    entry, owned = packages(component)
    root = run("go", "list", "-m") + "/"
    local = ["./" + package.removeprefix(root) for package in owned]
    formatted = run("gofmt", "-l", *[str(path) for directory in local for path in Path(directory).glob("*.go")])
    if formatted:
        raise ValueError("Go formatting required: " + formatted)
    subprocess.run(["go", "vet", *owned], check=True)
    subprocess.run(["go", "test", "-race", *owned], check=True)
    subprocess.run(["go", "build", "-o", "/dev/null", *entry], check=True)
    subprocess.run(["go", "run", "github.com/securego/gosec/v2/cmd/gosec@v2.29.0", *local], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("component")
    args = parser.parse_args()
    check(args.component)


if __name__ == "__main__":
    main()
