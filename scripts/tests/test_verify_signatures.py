# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Every pull request commit must carry a signature GitHub verifies."""

import pytest

from unittest.mock import Mock

import ci_automation as ci


@pytest.fixture(autouse=True)
def environment(tmp_path, monkeypatch):
    monkeypatch.setenv("GH_REPO", "owner/maestro")
    monkeypatch.setenv("PR_NUMBER", "7")
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(tmp_path / "summary"))
    monkeypatch.setenv("JOB_STATUS", "success")


def commit(sha, verified, reason="valid", subject="feat(cli): add status"):
    return {"sha": sha, "commit": {"message": subject + "\n\nbody",
                                   "verification": {"verified": verified, "reason": reason}}}


def test_all_verified_commits_pass(monkeypatch, capsys):
    api = Mock(return_value=[[commit("a" * 40, True), commit("b" * 40, True)]])
    monkeypatch.setattr(ci, "api", api)
    ci.verify_signatures()
    api.assert_called_once_with("repos/owner/maestro/pulls/7/commits?per_page=100", paginate=True)
    assert capsys.readouterr().out.count("ok ") == 2


def test_one_unverified_commit_fails_with_sha_and_reason(monkeypatch):
    api = Mock(return_value=[[commit("a" * 40, True), commit("b" * 40, False, "unsigned")]])
    monkeypatch.setattr(ci, "api", api)
    with pytest.raises(ValueError, match="bbbbbbbbbb \\(unsigned\\)"):
        ci.verify_signatures()


def test_missing_verification_block_fails(monkeypatch):
    broken = {"sha": "c" * 40, "commit": {"message": "fix: x"}}
    monkeypatch.setattr(ci, "api", Mock(return_value=[[broken]]))
    with pytest.raises(ValueError, match="cccccccccc \\(unknown\\)"):
        ci.verify_signatures()


def test_empty_pull_request_is_rejected(monkeypatch):
    monkeypatch.setattr(ci, "api", Mock(return_value=[[]]))
    with pytest.raises(ValueError, match="No commits"):
        ci.verify_signatures()
