# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Classify runtime/bundler changes and linked Go modules without builds."""

import pytest

from pathlib import Path
import subprocess
import sys
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import dependency_release as release


@pytest.fixture(autouse=True)
def isolated_repository(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)


def git(*args):
    return (
        subprocess.check_output(["git", *args], stderr=subprocess.DEVNULL)
        .decode()
        .strip()
    )


def go_repo():
    git("init", "--initial-branch=main")
    git("config", "user.name", "Test")
    git("config", "user.email", "test@example.test")
    git("config", "commit.gpgsign", "false")
    Path("go.mod").write_text("module example.test/maestro\n\ngo 1.26.0\n")
    Path("cmd/maestro").mkdir(parents=True)
    Path("cmd/maestro/main.go").write_text("package main\nfunc main() {}\n")
    git("add", "go.mod", "cmd/maestro/main.go")
    git("commit", "-m", "feat: add binary")
    Path("go.sum").write_text("example.test/unused v1.0.0 h1:unused\n")
    git("add", "go.sum")
    git("commit", "-m", "chore(deps): bump module")


def test_go_linked_changes_release_and_temporary_worktree_is_removed():
    go_repo()
    with patch.object(release, "linked_modules", side_effect=[{"a@v1"}, {"a@v2"}]):
        assert release.go_bump_ships()
    with patch.object(release, "linked_modules", return_value={"a@v1"}):
        assert not release.go_bump_ships()
    assert git("worktree", "list", "--porcelain").count("worktree ") == 1


def test_go_resolution_failure_does_not_leave_worktree():
    go_repo()
    with patch.object(
        release, "linked_modules", side_effect=RuntimeError("Resolution failed")
    ):
        with pytest.raises(RuntimeError):
            release.go_bump_ships()
    assert git("worktree", "list", "--porcelain").count("worktree ") == 1


def test_go_compiler_change_releases_implemented_binary():
    go_repo()
    Path("go.mod").write_text("module example.test/maestro\n\ngo 1.26.1\n")
    git("add", "go.mod")
    git("commit", "-m", "chore(deps): bump compiler")
    with patch.object(release, "linked_modules") as graph:
        assert release.go_bump_ships()
        graph.assert_not_called()
