# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Multicz component detection is visible in CI output and job summaries."""

import json
from pathlib import Path
import sys
from unittest.mock import Mock

import pytest

import ci_automation as ci


@pytest.fixture
def output_files(tmp_path, monkeypatch):
    output = tmp_path / "outputs"
    summary = tmp_path / "summary"
    summary.write_text("Existing summary\n")
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    monkeypatch.setenv("JOB_STATUS", "success")
    return output, summary


def test_changed_components_are_exposed_in_logs_outputs_and_appended_summary(output_files, monkeypatch, capsys):
    command = Mock(return_value=json.dumps({"changed": ["maestro-docs"], "unchanged": []}))
    monkeypatch.setattr(ci, "run", command)
    ci.changed_components()
    command.assert_called_once_with("multicz", "changed", "--output", "json")
    assert output_files[0].read_text().splitlines()[0] + "\n" == 'changed=["maestro-docs"]\n'
    assert 'Changed components: ["maestro-docs"]' in capsys.readouterr().out
    text = output_files[1].read_text()
    assert text.startswith("Existing summary\n")
    assert "## Changed components" in text and "- `maestro-docs`" in text
    assert "**Result:** success" in text
    assert "latest tag" in text


def test_no_component_changes_are_reported_explicitly(output_files, monkeypatch):
    monkeypatch.setattr(ci, "run", Mock(return_value='{"changed": [], "unchanged": ["maestro-docs"]}'))
    ci.changed_components()
    assert output_files[0].read_text().splitlines()[0] + "\n" == "changed=[]\n"
    assert "No changed components." in output_files[1].read_text()


def test_cli_optional_reference_is_an_argument_instead_of_shell_code(output_files, monkeypatch):
    reference = "$(touch injected)"
    command = Mock(return_value='{"changed": [], "unchanged": []}')
    monkeypatch.setattr(ci, "run", command)
    monkeypatch.setattr(sys, "argv", ["ci_automation.py", "changed-components", "--since", reference])
    ci.main()
    command.assert_called_once_with("multicz", "changed", "--output", "json", "--since", reference)
    assert not Path("injected").exists()


def test_invalid_component_response_is_rejected_without_writing_outputs(output_files, monkeypatch):
    monkeypatch.setattr(ci, "run", Mock(return_value='{"changed": "maestro-docs", "unchanged": []}'))
    with pytest.raises(ValueError, match="Invalid multicz component list"):
        ci.changed_components()
    assert not output_files[0].exists()
    assert output_files[1].read_text() == "Existing summary\n"


def test_detection_follows_plumber_and_tool_installation_with_full_history():
    workflow = (Path(__file__).resolve().parents[2] / ".github/workflows/ci.yml").read_text()
    components = workflow.split("  components:")[1].split("  licenses:")[0]
    assert "needs: signatures" in components
    assert "!failure() && !cancelled()" in components
    signatures = workflow.split("  signatures:")[1].split("  components:")[0]
    assert "needs: plumber" in signatures
    assert workflow.index("  plumber:") < workflow.index("  signatures:") < workflow.index("  components:")
    assert components.index("uv tool install multicz") < components.index("List changed multicz components")
    assert "fetch-depth: 0" in components
    assert "changed-components --ci" in components
    scripts = workflow.split("  scripts:")[1].split("  documentation:")[0]
    assert "contains(fromJSON(needs.components.outputs.changed), 'maestro-scripts')" in scripts
    assert "script-checks --since" in scripts


@pytest.mark.parametrize("path,expected", [("docs/index.md", ["maestro-docs"]), ("scripts/tool.py", ["maestro-scripts"])])
def test_real_multicz_detects_only_the_event_changes(tmp_path, monkeypatch, output_files, path, expected):
    import subprocess
    root = Path(__file__).resolve().parents[2]
    monkeypatch.chdir(tmp_path)
    for command in [["git", "init", "-b", "main"], ["git", "config", "user.name", "Test"],
                    ["git", "config", "user.email", "test@example.test"], ["git", "config", "commit.gpgsign", "false"]]:
        subprocess.run(command, check=True, capture_output=True)
    Path("scripts").mkdir()
    Path("docs").mkdir()
    Path("multicz.toml").write_text((root / "multicz.toml").read_text())
    Path("scripts/pyproject.toml").write_text((root / "scripts/pyproject.toml").read_text())
    Path("zensical.toml").write_text((root / "zensical.toml").read_text())
    Path("docs/index.md").write_text("Initial docs")
    Path("scripts/tool.py").write_text("# Initial script")
    for directory in ("cli", "svc"):
        Path(f"cmd/{directory}").mkdir(parents=True)
        Path(f"cmd/{directory}/version.go").write_text('package main\nconst Version = "0.0.0"\n')
    subprocess.run(["git", "add", "multicz.toml", "scripts/pyproject.toml", "scripts/tool.py", "docs/index.md", "zensical.toml"], check=True)
    subprocess.run(["git", "commit", "-m", "chore: initialize project"], check=True, capture_output=True)
    base = ci.git("rev-parse", "HEAD")
    Path(path).write_text("Changed")
    subprocess.run(["git", "add", path], check=True)
    subprocess.run(["git", "commit", "-m", "fix: update component"], check=True, capture_output=True)
    monkeypatch.setenv("GITHUB_EVENT_NAME", "push")
    monkeypatch.setenv("PUSH_BEFORE_SHA", base)
    ci.changed_components(ci=True)
    assert output_files[0].read_text().splitlines()[0] + "\n" == "changed=" + json.dumps(expected, separators=(",", ":")) + "\n"


def test_script_component_bump_refreshes_version_changelog_and_uv_lock(tmp_path, monkeypatch):
    import subprocess
    import tomllib
    root = Path(__file__).resolve().parents[2]
    monkeypatch.chdir(tmp_path)
    for filename in ("multicz.toml", "zensical.toml", "scripts/pyproject.toml", "scripts/uv.lock", "scripts/CHANGELOG.md"):
        destination = tmp_path / filename
        destination.parent.mkdir(exist_ok=True)
        destination.write_bytes((root / filename).read_bytes())
    for command in [["git", "init", "-b", "main"], ["git", "config", "user.name", "Test"],
                    ["git", "config", "user.email", "test@example.test"], ["git", "config", "commit.gpgsign", "false"],
                    ["git", "add", "multicz.toml", "zensical.toml", "scripts/pyproject.toml", "scripts/uv.lock", "scripts/CHANGELOG.md"],
                    ["git", "commit", "-m", "chore: initialize project"],
                    ["multicz", "bump", "--component", "maestro-scripts", "--force", "maestro-scripts:patch"],
                    ["uv", "lock", "--project", "scripts", "--check"]]:
        subprocess.run(command, check=True, capture_output=True)
    assert tomllib.loads(Path("scripts/pyproject.toml").read_text())["project"]["version"] == "0.0.1"
    locked = tomllib.loads(Path("scripts/uv.lock").read_text())["package"]
    assert next(package for package in locked if package["name"] == "maestro-scripts")["version"] == "0.0.1"
    assert "0.0.1" in Path("scripts/CHANGELOG.md").read_text()


@pytest.mark.parametrize("path,expected", [
    ("internal/transport/http.go", {"maestro", "maestro-svc"}),
    ("internal/cli/cli.go", {"maestro"}),
])
def test_go_plugin_and_frontend_dependency_select_actual_consumers(tmp_path, monkeypatch, output_files, path, expected):
    import shutil
    import subprocess
    root = Path(__file__).resolve().parents[2]
    monkeypatch.chdir(tmp_path)
    for directory in ("cmd", "internal"):
        shutil.copytree(root / directory, tmp_path / directory)
    for filename in ("go.mod", "go.sum", "multicz.toml", "zensical.toml", "scripts/pyproject.toml"):
        target = tmp_path / filename
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes((root / filename).read_bytes())
    for command in [["git", "init", "-b", "main"], ["git", "config", "user.name", "Test"],
                    ["git", "config", "user.email", "test@example.test"], ["git", "config", "commit.gpgsign", "false"],
                    ["git", "add", "cmd", "internal", "go.mod", "go.sum", "multicz.toml", "zensical.toml", "scripts/pyproject.toml"],
                    ["git", "commit", "-m", "chore: initialize project"]]:
        subprocess.run(command, check=True, capture_output=True)
    base = ci.git("rev-parse", "HEAD")
    target = Path(path)
    target.write_text(target.read_text() + "\n// Change component\n")
    subprocess.run(["git", "add", path], check=True)
    subprocess.run(["git", "commit", "-m", "fix: update component"], check=True, capture_output=True)
    monkeypatch.setenv("GITHUB_EVENT_NAME", "push")
    monkeypatch.setenv("PUSH_BEFORE_SHA", base)
    ci.changed_components(ci=True)
    result = dict(line.split("=", 1) for line in output_files[0].read_text().splitlines())
    assert set(json.loads(result["changed"])) == expected
    assert set(json.loads(result["go"])) == expected.intersection({"maestro", "maestro-svc"})


@pytest.mark.parametrize("event,key", [("pull_request", "PR_BASE_SHA"), ("push", "PUSH_BEFORE_SHA")])
def test_ci_detection_is_multicz_only_and_base_only_selects_script_tests(event, key, monkeypatch, output_files):
    base = "a" * 40
    monkeypatch.setenv("GITHUB_EVENT_NAME", event)
    monkeypatch.setenv(key, base)
    git = Mock()
    command = Mock(return_value='{"changed": ["maestro-scripts"], "unchanged": ["maestro-docs"]}')
    monkeypatch.setattr(ci, "git", git)
    monkeypatch.setattr(ci, "run", command)
    ci.changed_components(ci=True)
    git.assert_called_once_with("merge-base", "--is-ancestor", base, "HEAD")
    command.assert_called_once_with("multicz", "changed", "--output", "json")
    lines = dict(line.split("=", 1) for line in output_files[0].read_text().splitlines())
    assert lines["changed"] == '["maestro-scripts"]'
    assert lines["base"] == base


def test_chore_without_component_changes_outputs_nothing_to_run(output_files, monkeypatch):
    monkeypatch.setenv("GITHUB_EVENT_NAME", "push")
    monkeypatch.setenv("PUSH_BEFORE_SHA", "a" * 40)
    monkeypatch.setattr(ci, "run", Mock(return_value='{"changed": [], "unchanged": ["maestro", "maestro-docs"]}'))
    ci.changed_components(ci=True)
    lines = dict(line.split("=", 1) for line in output_files[0].read_text().splitlines())
    assert lines == {"changed": "[]", "go": "[]", "base": "a" * 40}
    assert "No changed components." in output_files[1].read_text()
