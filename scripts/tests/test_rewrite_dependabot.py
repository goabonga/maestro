# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Exercise dependency rewriting in disposable repositories and keyrings."""

import pytest

import os
from pathlib import Path
import subprocess

SCRIPT = Path(__file__).resolve().parents[1] / "rewrite_dependabot_commit.py"


@pytest.fixture(scope="module")
def signing_key(tmp_path_factory):
    keyring = tmp_path_factory.mktemp("rewrite-gpg")
    keyring.chmod(0o700)
    env = {**os.environ, "GNUPGHOME": str(keyring)}
    subprocess.run(
        ["gpg", "--batch", "--passphrase", "", "--quick-generate-key",
         "Rewrite Test <rewrite@example.test>", "ed25519", "sign", "0"],
        env=env, check=True, capture_output=True,
    )
    try:
        yield keyring
    finally:
        subprocess.run(["gpgconf", "--kill", "gpg-agent"], env=env, check=True)


@pytest.fixture
def repo(tmp_path, signing_key, monkeypatch):
    monkeypatch.setenv("GNUPGHOME", str(signing_key))
    repo = tmp_path
    git(repo, "init", "--initial-branch=main")
    git(repo, "config", "user.name", "Chris")
    git(repo, "config", "user.email", "goabonga@pm.me")
    git(repo, "config", "user.signingkey", "rewrite@example.test")
    git(repo, "config", "commit.gpgsign", "false")
    write(repo, "go.mod", "module example.test/maestro\n\ngo 1.26.0\n")
    commit(repo, "chore: initialize test repository", "go.mod")
    return repo


def test_signing_setup_wrapper_and_cleanup(repo, tmp_path):
    private_key = subprocess.check_output(
        [
            "gpg",
            "--batch",
            "--armor",
            "--export-secret-keys",
            "rewrite@example.test",
        ],
    ).decode()
    runtime = tmp_path / "runner"
    runtime.mkdir()
    github_env = tmp_path / "github.env"
    env = {
        **os.environ,
        "RUNNER_TEMP": str(runtime),
        "GITHUB_ENV": str(github_env),
        "GPG_PRIVATE_KEY": private_key,
        "GPG_PASSPHRASE": "",
        "PUSH_TOKEN": "test-token",
        "GIT_USER_NAME": "Chris",
        "GIT_USER_EMAIL": "goabonga@pm.me",
        "GIT_SIGNING_KEY": "rewrite@example.test",
    }
    automation = SCRIPT.with_name("ci_automation.py")
    subprocess.run(
        ["python3", str(automation), "configure-signing"],
        cwd=repo,
        env=env,
        check=True,
        capture_output=True,
    )
    home = Path(github_env.read_text().strip().split("=", 1)[1])
    assert home.is_dir()
    env["GNUPGHOME"] = str(home)
    try:
        write(repo, "feature.py", "print('feature')\n")
        commit(repo, "feat: add feature", "feature.py")
        subprocess.run(
            ["python3", str(SCRIPT.with_name("sign_commit.py"))],
            cwd=repo,
            env=env,
            check=True,
            capture_output=True,
        )
        assert subprocess.check_output(
            ["git", "log", "-1", "--format=%G?"], cwd=repo, env=env
        ).decode().strip() in {"G", "U"}
        subprocess.run(
            ["git", "verify-commit", "HEAD"],
            cwd=repo,
            env=env,
            check=True,
            capture_output=True,
        )
    finally:
        subprocess.run(["python3", str(automation), "cleanup"], env=env, check=True)
    assert not home.exists()


def git(repo, *args):
    return (
        subprocess.check_output(
            ["git", *args],
            cwd=repo,
            stderr=subprocess.DEVNULL,
        )
        .decode()
        .strip()
    )


def write(repo, path, content):
    target = Path(repo, path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content)


def commit(repo, message, path):
    git(repo, "add", path)
    git(
        repo,
        "-c",
        "user.name=dependabot[bot]",
        "-c",
        "user.email=dependabot[bot]@users.noreply.github.com",
        "commit",
        "--no-gpg-sign",
        "-m",
        message,
    )


def rewrite(repo, script=SCRIPT):
    return subprocess.run(
        ["python3", str(script)],
        cwd=repo,
        capture_output=True,
        text=True,
    )


def assert_rewritten(repo, subject, script=SCRIPT):
    result = rewrite(repo, script)
    assert result.returncode == 0, result.stderr
    assert git(repo, "log", "-1", "--format=%s") == subject
    assert git(repo, "log", "-1", "--format=%an <%ae>") == "Chris <goabonga@pm.me>"
    assert git(repo, "log", "-1", "--format=%cn <%ce>") == "Chris <goabonga@pm.me>"
    assert git(repo, "log", "-1", "--format=%G?") == "G"


def test_action_update_is_signed_as_maintainer(repo):
    write(repo, ".github/workflows/check.yml", "name: check\n")
    commit(repo, "chore: Bump checkout", ".github/workflows/check.yml")
    assert_rewritten(repo, "ci: bump checkout")


def test_go_update_without_binaries_remains_chore(repo):
    write(repo, "go.mod", "module example.test/maestro\n\ngo 1.26.1\n")
    commit(repo, "Bump Go", "go.mod")
    assert_rewritten(repo, "chore(deps): bump Go")


def test_trailers_are_removed_and_dependency_metadata_preserved(repo):
    write(repo, ".github/workflows/check.yaml", "name: check\n")
    commit(
        repo,
        "fix(deps)!: Bump checkout\n\nupdated-dependencies:\n- dependency-name: actions/checkout\n\nco-authored-by: Bot <bot@example.test>\nSigned-off-by: Bot <bot@example.test>\nBREAKING_CHANGE: update\nGenerated with a tool",
        ".github/workflows/check.yaml",
    )
    assert_rewritten(repo, "ci: bump checkout")
    body = git(repo, "log", "-1", "--format=%B")
    assert "- dependency-name: actions/checkout" in body
    for forbidden in (
        "co-authored-by",
        "signed-off-by",
        "breaking_change",
        "generated with",
    ):
        assert forbidden not in body.lower()


def test_unsupported_paths_leave_head_unchanged(repo):
    write(repo, "untrusted.sh", "exit 99\n")
    commit(repo, "Bump unexpected code", "untrusted.sh")
    before = git(repo, "rev-parse", "HEAD")
    assert rewrite(repo).returncode != 0
    assert git(repo, "rev-parse", "HEAD") == before


def test_subject_is_bounded_and_treated_as_data(repo):
    write(repo, ".github/workflows/check.yml", "name: check\n")
    commit(
        repo,
        "Bump $(touch injected) " + "x" * 100, ".github/workflows/check.yml"
    )
    result = rewrite(repo)
    assert result.returncode == 0, result.stderr
    assert len(git(repo, "log", "-1", "--format=%s")) <= 72
    assert not Path(repo, "injected").exists()


def test_rebase_rewrites_every_dependency_commit(repo):
    base = git(repo, "rev-parse", "HEAD")
    write(repo, ".github/workflows/check.yml", "name: check\n")
    commit(repo, "Bump checkout", ".github/workflows/check.yml")
    write(repo, "go.mod", "module example.test/maestro\n\ngo 1.26.1\n")
    commit(repo, "Bump Go", "go.mod")
    git(repo, "rebase", "--force-rebase", base, "--exec", f"python3 '{SCRIPT}'")
    assert git(repo, "rev-list", "--count", f"{base}..HEAD") == "2"
    for commit_sha in git(repo, "rev-list", f"{base}..HEAD").splitlines():
        assert (
            git(repo, "show", "-s", "--format=%an <%ae>", commit_sha)
            == "Chris <goabonga@pm.me>"
        )
        assert git(repo, "show", "-s", "--format=%G?", commit_sha) == "G"


def test_signed_merge_preserves_type_and_removes_forbidden_trailers(repo):
    script = SCRIPT.with_name("sign_commit.py")
    write(repo, "feature.sh", "touch injected\n")
    commit(
        repo,
        "fix!: repair startup\n\nReason for the fix.\n\nCo-Authored-By: Bot <bot@example.test>\nBREAKING CHANGE: old marker",
        "feature.sh",
    )
    assert_rewritten(repo, "fix: repair startup", script)
    message = git(repo, "log", "-1", "--format=%B")
    assert "Reason for the fix." in message
    assert "Co-Authored-By" not in message
    assert "BREAKING CHANGE" not in message
    assert not Path(repo, "injected").exists()


def test_signed_merge_does_not_execute_git_hooks(repo):
    script = SCRIPT.with_name("sign_commit.py")
    write(repo, "feature.py", "print('feature')\n")
    commit(repo, "feat: add feature", "feature.py")
    hook = Path(repo, ".git/hooks/pre-commit")
    hook.write_text("#!/bin/sh\ntouch injected\nexit 1\n")
    hook.chmod(493)
    assert_rewritten(repo, "feat: add feature", script)
    assert not Path(repo, "injected").exists()


def test_signed_merge_rejects_invalid_subject(repo):
    write(repo, "feature.py", "print('feature')\n")
    commit(repo, "invalid commit message", "feature.py")
    before = git(repo, "rev-parse", "HEAD")
    assert rewrite(repo, SCRIPT.with_name("sign_commit.py")).returncode != 0
    assert git(repo, "rev-parse", "HEAD") == before
