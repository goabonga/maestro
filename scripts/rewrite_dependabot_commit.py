#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Re-author and sign a commit using only trusted code and Git plumbing."""

import re
import subprocess
import sys
from pathlib import Path

from dependency_release import go_bump_ships


def git(*args):
    return subprocess.check_output(["git", *args])


paths = git("diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
paths = [path.decode() for path in paths.split(b"\0") if path]
if not paths:
    raise SystemExit("Refusing to rewrite an empty dependency commit")


def action(path):
    return path.startswith((".github/workflows/", ".github/actions/")) and path.endswith((".yml", ".yaml"))


lines = git("log", "-1", "--format=%B").decode().splitlines()
if all(action(path) for path in paths):
    prefix = "ci"
elif all(path in {"go.mod", "go.sum"} for path in paths):
    prefix = "fix(deps)" if go_bump_ships() else "chore(deps)"
else:
    raise SystemExit("Refusing to rewrite files outside the configured dependency ecosystems")

subject = re.sub(r"^[a-z-]+(?:\([^)]*\))?!?:\s*", "", lines[0])
subject = re.sub(r"^Bump\b", "bump", subject)
if not subject:
    raise SystemExit("Refusing an empty commit subject")
subject = f"{prefix}: {subject}"[:72].rstrip(" .")
body = []
for line in lines[1:]:
    if re.match(r"\s*(?:co-authored-by|signed-off-by|breaking[ _-]change):", line, re.I):
        continue
    if re.match(r"\s*generated (?:with|by)\b", line, re.I):
        continue
    body.append(line)
message = subject + "\n"
if "\n".join(body).strip():
    message += "\n" + "\n".join(body).strip() + "\n"
signing_key = git("config", "--get", "user.signingkey").decode().strip()
subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "commit", "--amend",
                "--reset-author", f"--gpg-sign={signing_key}", "--file=-", "--quiet"],
               input=message.encode(), check=True)
subprocess.run(["git", "verify-commit", "HEAD"], check=True)
