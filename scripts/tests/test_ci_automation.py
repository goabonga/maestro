# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Checks must exist, pass and still correspond to the authorized PR head."""

import pytest
import os
import json
from pathlib import Path
import sys
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import ci_automation as ci


def test_explicit_api_token_is_passed_only_to_the_subprocess_environment():
    with patch.dict(os.environ, GH_TOKEN="merge-token"), patch.object(ci, "run", return_value="{}") as command:
        assert ci.api("repos/owner/maestro/commits/head/status", token="checks-token") == {}
        assert command.call_args.kwargs["env"]["GH_TOKEN"] == "checks-token"
        assert "checks-token" not in str(command.call_args.args)
        assert os.environ["GH_TOKEN"] == "merge-token"
    with pytest.raises(ValueError, match="must not be empty"):
        ci.api("unused", token="")


@pytest.mark.parametrize("signed", [False, True])
def test_ci_reads_use_the_workflow_token_before_and_after_signing(signed):
    sha = "a" * 40
    with patch.dict(os.environ, GH_REPO="owner/maestro", HEAD_SHA=sha, CHECKS_TOKEN="checks-token"), \
         patch.object(ci, "pull_request", return_value={}), \
         patch.object(ci, "merge_authorized", return_value=True), \
         patch.object(ci, "git", return_value=sha), \
         patch.object(ci, "api", side_effect=[[{"check_runs": [check()]}], {"statuses": []}]) as client:
        ci.wait_checks(signed)
        assert client.call_count == 2
        for call in client.call_args_list:
            assert call.kwargs["token"] == "checks-token"
            assert sha in call.args[0]


def test_ci_read_token_is_required_without_a_pat_fallback():
    with patch.dict(os.environ, GH_TOKEN="merge-token", CHECKS_TOKEN=""):
        with pytest.raises(ValueError, match="CHECKS_TOKEN is required"):
            ci.wait_checks()


def test_consecutive_paginated_documents_are_all_decoded():
    with patch.object(
        ci, "run", return_value='[{"number": 1}]\n[{"number": 2}]'
    ) as command:
        assert ci.api("repos/owner/maestro/issues", paginate=True) == [
            [{"number": 1}],
            [{"number": 2}],
        ]
        assert "--slurp" not in command.call_args.args


def test_truncated_page_is_an_error_instead_of_partial_results():
    with patch.object(ci, "run", return_value='[{"number": 1}]\n[{'):
        with pytest.raises(json.JSONDecodeError):
            ci.api("repos/owner/maestro/issues", paginate=True)


def check(
    number=1,
    state="completed",
    conclusion="success",
    name="validate",
):
    return {
        "id": number,
        "status": state,
        "conclusion": conclusion,
        "name": name,
        "app": {"id": 10},
    }


def test_missing_or_skipped_ci_cannot_merge():
    assert not ci.checks_ready([], {"statuses": []})
    assert not ci.checks_ready([check(conclusion="skipped")], {"statuses": []})


def test_latest_success_supersedes_failed_attempt():
    assert ci.checks_ready(
        [check(conclusion="failure"), check(2)], {"statuses": []}
    )


def test_pending_or_failed_checks_block_merge():
    assert not ci.checks_ready(
        [check(state="in_progress", conclusion=None)], {"statuses": []}
    )
    with pytest.raises(ValueError):
        ci.checks_ready([check(conclusion="failure")], {"statuses": []})
    with pytest.raises(ValueError):
        ci.checks_ready([check()], {"statuses": [{}], "state": "failure"})


def test_removed_label_fork_or_changed_head_revokes_authorization():
    pr = {
        "state": "open",
        "draft": False,
        "head": {
            "repo": {"full_name": "owner/maestro"},
            "sha": "a" * 40,
            "ref": "feature",
        },
        "base": {"ref": "main"},
        "labels": [{"name": "auto-merge"}],
    }
    with patch.dict(
        os.environ,
        GH_REPO="owner/maestro",
        HEAD_REF="feature",
        MERGE_LABEL="auto-merge",
    ):
        assert ci.merge_authorized(pr, "a" * 40)
        assert not ci.merge_authorized(pr, "b" * 40)
        pr["labels"] = []
        assert not ci.merge_authorized(pr, "a" * 40)
        pr["labels"] = [{"name": "auto-merge"}]
        pr["head"]["repo"] = {"full_name": "fork/maestro"}
        assert not ci.merge_authorized(pr, "a" * 40)
