#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Re-author and sign a commit using only trusted code and Git plumbing."""

import re
import subprocess
import sys
from pathlib import Path


def git(*args):
    return subprocess.check_output(["git", *args])


lines = git("log", "-1", "--format=%B").decode().splitlines()
subject = re.sub(r"^([a-z-]+(?:\([^)]*\))?)!: ", r"\1: ", lines[0])
if not re.fullmatch(r"(?:feat|fix|refactor|test|docs|chore|style|perf|build|ci|revert)(?:\([a-z0-9-]+\))?: [a-z].{0,70}", subject) or len(subject) > 72 or subject.endswith("."):
    raise SystemExit("Refusing an invalid Conventional Commit subject")
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
