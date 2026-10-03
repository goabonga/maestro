# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Script tests run only for the files a change touches."""

import shutil
import subprocess
from pathlib import Path

import pytest

import ci_automation as ci

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture
def repo(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    shutil.copy(ROOT / "multicz.toml", "multicz.toml")
    shutil.copytree(ROOT / "scripts", "scripts", ignore=shutil.ignore_patterns(".venv", "__pycache__"))
    Path("docs").mkdir()
    Path("docs/index.md").write_text("Initial")
    Path("assets").mkdir()
    Path("assets/maestro.svg").write_text("<svg/>")
    for command in (["git", "init", "-b", "main"], ["git", "config", "user.name", "Test"],
                    ["git", "config", "user.email", "test@example.test"], ["git", "config", "commit.gpgsign", "false"],
                    ["git", "add", "."], ["git", "commit", "-m", "chore: initialize project"]):
        subprocess.run(command, check=True, capture_output=True)
    return tmp_path


def commit(path, text="changed\n"):
    base = ci.git("rev-parse", "HEAD")
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text(text)
    subprocess.run(["git", "add", path], check=True)
    subprocess.run(["git", "commit", "-m", "fix: change file"], check=True, capture_output=True)
    return base


def test_script_change_selects_only_its_tests(repo):
    base = commit("scripts/init_github.py", "# changed\n")
    assert ci.script_tests(base) == ["scripts/tests/test_init_github.py"]


def test_test_file_change_selects_itself(repo):
    base = commit("scripts/tests/test_rewrite_dependabot.py", "# changed\n")
    assert ci.script_tests(base) == ["scripts/tests/test_rewrite_dependabot.py"]


def test_shared_module_change_runs_full_suite(repo):
    base = commit("scripts/ci_automation.py", "# changed\n")
    assert ci.script_tests(base) is None


def test_changelog_only_change_selects_no_tests(repo):
    base = commit("scripts/CHANGELOG.md", "notes\n")
    assert ci.script_tests(base) == []


def test_unmapped_script_runs_full_suite(repo):
    base = commit("scripts/brand_new.py", "# new\n")
    assert ci.script_tests(base) is None


def test_missing_base_runs_full_suite():
    assert ci.script_tests("") is None
