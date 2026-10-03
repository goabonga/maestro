# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Recover durable issue identity after partial writes and Dependabot edits."""

import pytest

import copy
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import track_dependabot_issue as tracking


@pytest.fixture
def state(monkeypatch):
    monkeypatch.setenv("GH_REPO", "owner/maestro")
    monkeypatch.setenv("PR_NUMBER", "42")
    state = {
        "pr": {
            "number": 42,
            "title": "chore(deps): bump example",
            "body": "Original description",
            "state": "open",
            "merged": False,
            "user": {"login": "dependabot[bot]"},
            "head": {"repo": {"full_name": "owner/maestro"}},
        },
        "issues": [],
        "creates": 0,
        "fail_link": False,
        "lose_create_response": False,
        "fail_list": False,
        "merge_after_create": False,
    }

    def api(endpoint, method="GET", payload=None, paginate=False):
        if method == "GET" and "/pulls/42" in endpoint:
            return copy.deepcopy(state["pr"])
        if method == "GET" and "/issues?" in endpoint:
            assert paginate
            assert "state=all" in endpoint
            if state["fail_list"]:
                raise RuntimeError("Second page unavailable")
            return [[], copy.deepcopy(state["issues"])]
        if method == "POST":
            state["creates"] += 1
            issue = {"number": 100 + state["creates"], "state": "open", **payload}
            state["issues"].append(issue)
            if state["merge_after_create"]:
                state["pr"].update(state="closed", merged=True)
            if state["lose_create_response"]:
                state["lose_create_response"] = False
                raise RuntimeError("POST succeeded but its response was lost")
            return copy.deepcopy(issue)
        if method == "PATCH" and "/pulls/42" in endpoint:
            if state["fail_link"]:
                state["fail_link"] = False
                raise RuntimeError("PR body update failed")
            state["pr"].update(payload)
            return copy.deepcopy(state["pr"])
        if method == "PATCH":
            number = int(endpoint.rsplit("/", 1)[1])
            issue = next((i for i in state["issues"] if i["number"] == number))
            issue.update(payload)
            return copy.deepcopy(issue)
        raise AssertionError((endpoint, method))

    state["api"] = api
    return state


def test_reruns_and_replaced_pr_body_reuse_issue(state):
    assert tracking.track(state["api"]) == 101
    tracking.track(state["api"])
    state["pr"].update(
        title="chore(deps): bump a new version", body="Updated by Dependabot"
    )
    assert tracking.track(state["api"]) == 101
    assert state["creates"] == 1
    assert "Updated by Dependabot" in state["pr"]["body"]
    assert state["pr"]["body"].count("Closes #101") == 1


def test_failed_pr_edit_recovers_without_creating_again(state):
    state["fail_link"] = True
    with pytest.raises(RuntimeError):
        tracking.track(state["api"])
    tracking.track(state["api"])
    assert state["creates"] == 1
    assert "Closes #101" in state["pr"]["body"]


def test_lost_create_response_recovers_persisted_marker(state):
    state["lose_create_response"] = True
    with pytest.raises(RuntimeError):
        tracking.track(state["api"])
    tracking.track(state["api"])
    assert state["creates"] == 1


def test_closed_legacy_issue_is_adopted_even_without_pr_marker(state):
    state["issues"].append(
        {
            "number": 88,
            "state": "closed",
            "title": "Old title",
            "body": "Filed by `dependabot-rewrite` for #42. The PR commits were signed.",
        }
    )
    assert tracking.track(state["api"]) == 88
    assert state["creates"] == 0
    assert state["issues"][0]["state"] == "closed"
    assert tracking.marker("owner/maestro", 42) in state["issues"][0]["body"]


def test_failed_listing_never_creates_issue(state):
    state["fail_list"] = True
    with pytest.raises(RuntimeError):
        tracking.track(state["api"])
    assert state["creates"] == 0


def test_merge_race_closes_issue(state):
    state["merge_after_create"] = True
    tracking.track(state["api"])
    assert state["issues"][0]["state"] == "closed"
    assert state["issues"][0]["state_reason"] == "completed"


def test_other_prs_and_spoofed_links_cannot_adopt_unrelated_issue(state):
    state["issues"].append(
        {"number": 88, "state": "open", "body": tracking.marker("owner/maestro", 43)}
    )
    state["pr"]["body"] = (
        "<!-- dependabot-tracking -->\nTracked by https://github.com/owner/maestro/issues/88.\n\nCloses #88"
    )
    assert tracking.track(state["api"]) == 101
    assert state["issues"][0]["body"] == tracking.marker("owner/maestro", 43)


def test_existing_duplicates_choose_oldest_and_never_create(state):
    state["issues"] = [
        {"number": n, "state": "open", "body": tracking.marker("owner/maestro", 42)}
        for n in [90, 80]
    ]
    assert tracking.track(state["api"]) == 80
    assert state["creates"] == 0


def test_closed_pr_without_issue_does_not_create(state):
    state["pr"]["state"] = "closed"
    assert tracking.track(state["api"]) is None
    assert state["creates"] == 0
