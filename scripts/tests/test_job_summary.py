# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Job summaries preserve previous output and handle failures and missing context."""

import os
from pathlib import Path
import subprocess

import pytest

import ci_automation as ci


@pytest.fixture
def summary_file(tmp_path, monkeypatch):
    path = tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(path))
    for name in ("JOB_STATUS", "PR_NUMBER", "PR", "GIT_USER_NAME", "GIT_USER_EMAIL"):
        monkeypatch.delenv(name, raising=False)
    return path


def test_failed_job_summary_preserves_tracking_output(summary_file, monkeypatch):
    summary_file.write_text("Tracking issue: #12\n")
    monkeypatch.setenv("JOB_STATUS", "failure")
    monkeypatch.setenv("PR_NUMBER", "42")
    monkeypatch.setenv("GIT_USER_NAME", "Chris")
    monkeypatch.setenv("GIT_USER_EMAIL", "goabonga@pm.me")
    monkeypatch.setenv("SECRET_TOKEN", "must-not-be-written")
    ci.summary("Dependabot signed rewrite")
    text = summary_file.read_text()
    assert text.startswith("Tracking issue: #12\n")
    assert "**Result:** failure" in text
    assert "Pull request: 42" in text
    assert "Identity: Chris <goabonga@pm.me>" in text
    assert "must-not-be-written" not in text


def test_summary_without_pr_or_identity_omits_empty_fields(summary_file):
    ci.summary("CI validation", "Checks: branding and documentation.")
    text = summary_file.read_text()
    assert "**Result:** unknown" in text
    assert "Pull request:" not in text
    assert "Identity:" not in text
    assert "Checks: branding and documentation." in text


@pytest.mark.parametrize("number,expected", [("42", "42"), ("", "not validated")])
def test_missing_validation_and_merge_pr_context(summary_file, monkeypatch, number, expected):
    monkeypatch.setenv("PR_NUMBER", "")
    monkeypatch.setenv("PR", number)
    ci.summary("Signed automatic merge")
    assert f"Pull request: {expected}" in summary_file.read_text()


def test_summary_cli_treats_title_and_body_as_data(summary_file, tmp_path):
    script = Path(ci.__file__).resolve()
    title = "$(touch injected)"
    subprocess.run(["python3", str(script), "summary", "--title", title,
                    "--body", "First line\nSecond line"],
                   cwd=tmp_path, env=os.environ.copy(), check=True)
    assert title in summary_file.read_text()
    assert "First line\nSecond line" in summary_file.read_text()
    assert not (tmp_path / "injected").exists()
