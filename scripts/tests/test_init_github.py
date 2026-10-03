# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Check is read-only; initialization is repeatable and never exposes secrets."""

import copy
import subprocess
import sys
from unittest.mock import Mock

import pytest

import init_github as setup


@pytest.fixture
def config():
    return {
        "settings": {"has_issues": True},
        "workflow_permissions": {"default_workflow_permissions": "read"},
        "labels": {
            "dependencies": {"color": "fc9513", "description": "Dependency updates"}
        },
        "variables": {"GIT_USER_NAME": "Chris"},
        "secrets": {"RELEASE_PAT": True},
        "rulesets": [
            {
                "name": "main-protection",
                "target": "branch",
                "enforcement": "active",
                "rules": [{"type": "required_signatures"}],
            }
        ],
    }


@pytest.fixture
def state(config):
    return {
        "settings": copy.deepcopy(config["settings"]),
        "workflow_permissions": copy.deepcopy(config["workflow_permissions"]),
        "labels": copy.deepcopy(config["labels"]),
        "variables": {"GIT_USER_NAME": {"name": "GIT_USER_NAME", "value": "Chris"}},
        "secrets": {"RELEASE_PAT"},
        "rulesets": [{"id": 10, **copy.deepcopy(config["rulesets"][0])}],
    }


def test_check_does_not_write_or_inspect_secret_values(config, state):
    client = Mock()
    repo = setup.Repository("owner/maestro", client)
    assert repo.check(config, state) == []
    client.assert_not_called()
    state["secrets"].clear()
    assert "secret RELEASE_PAT missing" in repo.check(config, state)[0]
    client.assert_not_called()


def test_existing_configuration_is_not_written_again(config, state):
    client, secrets = Mock(), Mock()
    setup.Repository("owner/maestro", client).init(
        config, state, env={}, secret_runner=secrets
    )
    client.assert_not_called()
    secrets.assert_not_called()


def test_missing_secret_aborts_before_any_writes(config, state):
    client = Mock()
    state["secrets"].clear()
    with pytest.raises(ValueError, match="Missing required GitHub secrets"):
        setup.Repository("owner/maestro", client).init(config, state, env={})
    client.assert_not_called()


def test_empty_missing_secret_aborts_before_any_writes(config, state):
    client, secrets = Mock(), Mock()
    state["secrets"].clear()
    with pytest.raises(ValueError, match="Missing required GitHub secrets"):
        setup.Repository("owner/maestro", client).init(
            config, state, env={"RELEASE_PAT": ""}, secret_runner=secrets
        )
    client.assert_not_called()
    secrets.assert_not_called()


def test_cli_missing_secrets_prints_actionable_commands_without_traceback(config, state, monkeypatch, capsys):
    config["secrets"] = {"DEPENDABOT_PAT": True, "AUTO_MERGE_PAT": True}
    declared = setup.load_config(setup.DEFAULT_CONFIG)
    config["tokens"] = {name: declared["tokens"][name] for name in config["secrets"]}
    monkeypatch.delenv("DEPENDABOT_PAT", raising=False)
    monkeypatch.delenv("AUTO_MERGE_PAT", raising=False)
    monkeypatch.setattr(sys, "argv", ["init_github.py", "init", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(return_value=state)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 1
    captured = capsys.readouterr()
    assert not captured.out
    for name in config["secrets"]:
        assert f"gh secret set {name} --repo owner/maestro" in captured.err
    assert "Branch protection was applied; no other configuration was changed" in captured.err
    assert "Traceback" not in captured.err
    assert "docs/development/github.md" in captured.err
    assert "Resource owner: owner" in captured.err
    assert "Only select repositories -> maestro" in captured.err
    assert "Contents: Read and write" in captured.err
    assert "Pull requests: Read-only" in captured.err
    assert "Workflows: Read and write" in captured.err
    assert "Issues: Read and write" in captured.err
    assert "Repository administrator" in captured.err
    assert "RELEASE_PAT" not in captured.err
    assert "Checks:" not in captured.err
    client.assert_not_called()
    assert remote.snapshot.call_count == 2


def test_read_only_check_explains_missing_pat_permissions(config, state, monkeypatch, capsys):
    config["tokens"] = {"RELEASE_PAT": setup.load_config(setup.DEFAULT_CONFIG)["tokens"]["RELEASE_PAT"]}
    state["secrets"].clear()
    monkeypatch.setattr(sys, "argv", ["init_github.py", "check", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(return_value=state)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 1
    text = capsys.readouterr().out
    assert "RELEASE_PAT" in text
    assert "Contents: Read and write" in text
    assert "Metadata: Read-only (automatic)" in text
    client.assert_not_called()


def test_gpg_secrets_are_not_described_as_personal_access_tokens():
    config = setup.load_config(setup.DEFAULT_CONFIG)
    assert setup.token_instructions("owner/maestro", config, ["GPG_PRIVATE_KEY", "GPG_PASSPHRASE"]) == ""
    assert setup.token_instructions("owner/maestro", config, []) == ""


@pytest.mark.parametrize("body", [
    '[tokens.UNKNOWN.permissions]\nContents="write"\n',
    '[secrets]\nRELEASE_PAT=true\n[tokens.RELEASE_PAT]\npurpose="Release token"\n[tokens.RELEASE_PAT.permissions]\nContents="all"\n',
])
def test_invalid_pat_guidance_is_rejected_before_remote_access(tmp_path, body):
    path = tmp_path / "invalid.toml"
    path.write_text(body)
    with pytest.raises(ValueError, match="Token"):
        setup.load_config(path)


def test_cli_github_failures_do_not_expose_command_output(monkeypatch, capsys):
    private = "private-token-example"
    monkeypatch.setattr(sys, "argv", ["init_github.py", "check"])
    monkeypatch.setattr(setup, "run", Mock(side_effect=subprocess.CalledProcessError(1, private, output=private)))
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 1
    captured = capsys.readouterr()
    assert "check authentication" in captured.err
    assert private not in captured.err
    assert "Traceback" not in captured.err


def test_missing_labels_variables_and_policy_are_created_once(config, state):
    state.update(labels={}, variables={}, rulesets=[])
    writes = []

    def client(endpoint, method, payload):
        writes.append((endpoint, method, payload))
        if endpoint.endswith("/labels"):
            state["labels"][payload["name"]] = copy.deepcopy(payload)
        elif endpoint.endswith("/variables"):
            state["variables"][payload["name"]] = copy.deepcopy(payload)
        elif endpoint.endswith("/rulesets"):
            state["rulesets"].append({"id": 10, **copy.deepcopy(payload)})
        else:
            raise AssertionError(endpoint)

    repo = setup.Repository("owner/maestro", client)
    repo.init(config, state, env={})
    assert len(writes) == 3
    assert all(method == "POST" for _, method, _ in writes)
    assert repo.check(config, state) == []
    repo.init(config, state, env={})
    assert len(writes) == 3


def test_drift_updates_existing_policy_instead_of_creating_another(config, state):
    client = Mock()
    state["rulesets"][0]["enforcement"] = "disabled"
    setup.Repository("owner/maestro", client).init(config, state, env={})
    client.assert_called_once_with(
        "repos/owner/maestro/rulesets/10", "PUT", config["rulesets"][0]
    )


def test_ambiguous_policies_abort_before_writes(config, state):
    client = Mock()
    state["rulesets"].append(copy.deepcopy(state["rulesets"][0]))
    with pytest.raises(ValueError, match="Ambiguous ruleset"):
        setup.Repository("owner/maestro", client).init(config, state, env={})
    client.assert_not_called()


def test_secret_values_go_only_to_stdin(config, state):
    secret_runner = Mock()
    value = "private-token-example"
    setup.Repository("owner/maestro", Mock()).init(
        config, state, env={"RELEASE_PAT": value}, secret_runner=secret_runner
    )
    args, kwargs = secret_runner.call_args
    assert value not in str(args)
    assert kwargs["input"] == value.encode()
    assert kwargs["stdout"] == subprocess.DEVNULL
    assert kwargs["stderr"] == subprocess.DEVNULL


def test_failed_secret_upload_redacts_value(config, state):
    value = "private-token-example"
    secret_runner = Mock(
        side_effect=subprocess.CalledProcessError(1, "gh", output=value)
    )
    with pytest.raises(ValueError) as error:
        setup.Repository("owner/maestro", Mock()).init(
            config, state, env={"RELEASE_PAT": value}, secret_runner=secret_runner
        )
    assert value not in str(error.value)


def test_managed_config_ignores_api_defaults_and_rule_order():
    assert setup.includes(
        {"id": 10, "rules": [{"type": "b"}, {"type": "a"}]},
        {"rules": [{"type": "a"}, {"type": "b"}]},
    )


def test_repository_configuration_refers_to_existing_ci():
    config = setup.load_config(setup.DEFAULT_CONFIG)
    workflow = (setup.DEFAULT_CONFIG.parent / "workflows/ci.yml").read_text()
    checks = next(
        rule["parameters"]["required_status_checks"]
        for rule in config["rulesets"][0]["rules"]
        if rule["type"] == "required_status_checks"
    )
    for check in checks:
        assert f"name: {check['context']}" in workflow


def test_automation_tokens_are_separated_and_declared():
    config = setup.load_config(setup.DEFAULT_CONFIG)
    workflows = setup.DEFAULT_CONFIG.parent / "workflows"
    for filename, secret, forbidden in [
        ("dependabot-rewrite.yml", "DEPENDABOT_PAT", "AUTO_MERGE_PAT"),
        ("auto-merge-signed.yml", "AUTO_MERGE_PAT", "DEPENDABOT_PAT"),
    ]:
        workflow = (workflows / filename).read_text()
        assert f"secrets.{secret}" in workflow
        assert f"secrets.{forbidden}" not in workflow
        assert "secrets.RELEASE_PAT" not in workflow
        assert config["secrets"][secret] is True
    assert config["secrets"]["RELEASE_PAT"] is True
    assert "secrets.RELEASE_PAT" in (workflows / "ci.yml").read_text()


def test_invalid_repo_and_secret_names_fail_before_network(tmp_path):
    with pytest.raises(ValueError, match="Invalid repository"):
        setup.Repository("owner/maestro/../../other")
    file = tmp_path / "invalid.toml"
    file.write_text('[secrets]\n"bad/name" = true\n')
    with pytest.raises(ValueError, match="Invalid variable/secret name"):
        setup.load_config(file)


def test_protect_applies_only_branch_protection(config, state, monkeypatch, capsys):
    before = copy.deepcopy(state)
    before.update(rulesets=[], labels={}, variables={})
    before["settings"]["allow_merge_commit"] = True
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(side_effect=[before, copy.deepcopy(state)])
    monkeypatch.setattr(sys, "argv", ["init_github.py", "protect", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 0
    client.assert_called_once_with("repos/owner/maestro/rulesets", "POST", config["rulesets"][0])
    captured = capsys.readouterr()
    assert "GitHub repository configuration matches." in captured.out
    assert "allow_merge_commit" not in captured.out


def test_protect_is_not_blocked_by_missing_secrets(config, state, monkeypatch):
    state["secrets"].clear()
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(return_value=state)
    monkeypatch.setattr(sys, "argv", ["init_github.py", "protect", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 0
    client.assert_not_called()


def test_protect_reports_drifted_ruleset(config, state, monkeypatch, capsys):
    state["rulesets"][0]["enforcement"] = "disabled"
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(return_value=state)
    monkeypatch.setattr(sys, "argv", ["init_github.py", "check", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 1
    assert "ruleset main-protection missing, duplicated or differs" in capsys.readouterr().out


def test_init_applies_branch_protection_before_missing_secrets_fail(config, state, monkeypatch):
    state["rulesets"] = []
    state["secrets"].clear()
    client = Mock()
    remote = setup.Repository("owner/maestro", client)
    remote.snapshot = Mock(return_value=state)
    monkeypatch.setattr(sys, "argv", ["init_github.py", "init", "--repo", "owner/maestro"])
    monkeypatch.setattr(setup, "load_config", lambda path: config)
    monkeypatch.setattr(setup, "Repository", lambda repo: remote)
    with pytest.raises(SystemExit) as error:
        setup.main()
    assert error.value.code == 1
    client.assert_called_once_with("repos/owner/maestro/rulesets", "POST", config["rulesets"][0])
