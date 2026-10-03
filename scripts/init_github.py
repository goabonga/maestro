#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Check or reconcile repository labels, secrets, variables and policies."""

import argparse
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import tomllib
from urllib.parse import quote

from ci_automation import api, run

DEFAULT_CONFIG = Path(__file__).resolve().parent.parent / ".github/repository.toml"


def includes(actual, expected):
    """Compare managed fields; GitHub adds IDs/defaults and may reorder rules."""
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(key in actual and includes(actual[key], value)
                                                 for key, value in expected.items())
    if isinstance(expected, list):
        return isinstance(actual, list) and len(actual) == len(expected) and all(
            any(includes(candidate, item) for candidate in actual) for item in expected)
    return actual == expected


def load_config(path):
    with Path(path).open("rb") as stream:
        config = tomllib.load(stream)
    for name in [*config.get("variables", {}), *config.get("secrets", {})]:
        if not re.fullmatch(r"[A-Z_][A-Z0-9_]*", name):
            raise ValueError("Invalid variable/secret name in configuration")
    for label in config.get("labels", {}).values():
        if not re.fullmatch(r"[a-fA-F0-9]{6}", label["color"]):
            raise ValueError("Invalid label color")
    names = [rule["name"] for rule in config.get("rulesets", [])]
    if len(names) != len(set(names)):
        raise ValueError("Duplicate managed ruleset names")
    for name, token in config.get("tokens", {}).items():
        if name not in config.get("secrets", {}):
            raise ValueError(f"Token {name} must refer to a declared secret")
        if not isinstance(token.get("purpose"), str) or not token["purpose"].strip():
            raise ValueError(f"Token {name} must describe its purpose")
        if not token.get("permissions") or any(level not in {"read", "write"} for level in token["permissions"].values()):
            raise ValueError(f"Token {name} permissions must use read or write")
    return config


def token_instructions(repo, config, names):
    tokens = [(name, config.get("tokens", {})[name]) for name in names if name in config.get("tokens", {})]
    if not tokens:
        return ""
    owner, project = repo.split("/")
    lines = ["Fine-grained PAT setup:", "  Create: https://github.com/settings/personal-access-tokens/new",
             f"  Resource owner: {owner}", f"  Repository access: Only select repositories -> {project}",
             "  Create a separate token for each responsibility."]
    for name, token in tokens:
        lines.extend(["", f"  {name}: {token['purpose']}", "  Repository permissions:"])
        for permission, level in token["permissions"].items():
            lines.append(f"    {permission}: {'Read and write' if level == 'write' else 'Read-only'}")
        lines.append("    Metadata: Read-only (automatic)")
        if token.get("account_role"):
            lines.append(f"  Token owner's role: {token['account_role']}")
    return "\n".join(lines)


class Repository:
    def __init__(self, repo, client=api):
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", repo):
            raise ValueError("Invalid repository")
        self.repo, self.client = repo, client
        self.base = f"repos/{repo}"

    def pages(self, path, key=None):
        pages = self.client(f"{self.base}/{path}", paginate=True)
        return [item for page in pages for item in (page[key] if key else page)]

    def snapshot(self):
        # Any read failure aborts the operation before writes. Inherited
        # organization rulesets are never adopted or overwritten.
        rulesets = self.pages("rulesets?includes_parents=false&per_page=100")
        return {
            "settings": self.client(self.base),
            "workflow_permissions": self.client(f"{self.base}/actions/permissions/workflow"),
            "labels": {label["name"]: label for label in self.pages("labels?per_page=100")},
            "variables": {var["name"]: var for var in self.pages("actions/variables?per_page=100", "variables")},
            "secrets": {secret["name"] for secret in self.pages("actions/secrets?per_page=100", "secrets")},
            "rulesets": [self.client(f"{self.base}/rulesets/{rule['id']}") for rule in rulesets],
        }

    def check(self, config, state):
        problems = []
        for section in ("settings", "workflow_permissions"):
            for key, value in config.get(section, {}).items():
                if not includes(state[section].get(key), value):
                    problems.append(f"{section}.{key} differs")
        for name, label in config.get("labels", {}).items():
            if not includes(state["labels"].get(name), label):
                problems.append(f"label {name} missing or differs")
        for name, value in config.get("variables", {}).items():
            if state["variables"].get(name, {}).get("value") != value:
                problems.append(f"variable {name} missing or differs")
        for name, required in config.get("secrets", {}).items():
            if required and name not in state["secrets"]:
                problems.append(f"secret {name} missing (value cannot be inspected)")
        for expected in config.get("rulesets", []):
            matches = [r for r in state["rulesets"] if r["name"] == expected["name"]]
            if len(matches) != 1 or not includes(matches[0], expected):
                problems.append(f"ruleset {expected['name']} missing, duplicated or differs")
        return problems

    def init(self, config, state, env=None, secret_runner=subprocess.run):
        env = os.environ if env is None else env
        missing = [name for name, required in config.get("secrets", {}).items()
                   if required and name not in state["secrets"] and not env.get(name)]
        if missing:
            commands = "\n".join(f"  gh secret set {name} --repo {self.repo}" for name in missing)
            raise ValueError("Missing required GitHub secrets: " + ", ".join(missing)
                             + "\n" + token_instructions(self.repo, config, missing)
                             + "\nPrepare the secret values described in docs/development/github.md,"
                             + "\nthen configure each missing secret using the interactive prompt:\n"
                             + commands
                             + "\nAlternatively, supply their values through matching environment variables."
                             + "\nRerun init after configuring the secrets. Branch protection was applied; "
                             + "no other configuration was changed.")
        for expected in config.get("rulesets", []):
            if sum(r["name"] == expected["name"] for r in state["rulesets"]) > 1:
                raise ValueError(f"Ambiguous ruleset {expected['name']}; refusing to modify policies")
        # Never delete unmanaged labels, variables, secrets or rulesets.
        for section, endpoint in (("settings", self.base),
                                  ("workflow_permissions", f"{self.base}/actions/permissions/workflow")):
            if not includes(state[section], config.get(section, {})):
                self.client(endpoint, "PUT" if section == "workflow_permissions" else "PATCH", config[section])
        for name, expected in config.get("labels", {}).items():
            if not includes(state["labels"].get(name), expected):
                exists = name in state["labels"]
                endpoint = f"{self.base}/labels" + (f"/{quote(name, safe='')}" if exists else "")
                self.client(endpoint, "PATCH" if exists else "POST", {"name": name, **expected})
        for name, value in config.get("variables", {}).items():
            if state["variables"].get(name, {}).get("value") != value:
                exists = name in state["variables"]
                endpoint = f"{self.base}/actions/variables" + (f"/{name}" if exists else "")
                self.client(endpoint, "PATCH" if exists else "POST", {"name": name, "value": value})
        for name in config.get("secrets", {}):
            if name in env:
                # gh handles public-key encryption. Secret values go through
                # stdin, never argv, stdout, diagnostics or committed files.
                try:
                    secret_runner(["gh", "secret", "set", name, "--repo", self.repo],
                                  input=env[name].encode(), check=True,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                except subprocess.CalledProcessError:
                    raise ValueError(f"Failed to set secret {name}; value withheld") from None
        for expected in config.get("rulesets", []):
            existing = next((r for r in state["rulesets"] if r["name"] == expected["name"]), None)
            if not includes(existing, expected):
                endpoint = f"{self.base}/rulesets" + (f"/{existing['id']}" if existing else "")
                self.client(endpoint, "PUT" if existing else "POST", copy.deepcopy(expected))


def protection_only(config):
    """Scope a configuration to branch protection, which needs no secrets."""
    return {"rulesets": config.get("rulesets", [])}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["check", "init", "protect"])
    parser.add_argument("--repo", help="owner/repository; defaults to gh's current repository")
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    args = parser.parse_args()
    try:
        config = load_config(args.config)
        repo = args.repo or json.loads(run("gh", "repo", "view", "--json", "nameWithOwner"))["nameWithOwner"]
        remote = Repository(repo)
        state = remote.snapshot()
        if args.command == "protect":
            # Branch protection is independent of secrets, so it can be applied
            # on a fresh repository before any token exists.
            config = protection_only(config)
        if args.command in ("init", "protect"):
            if args.command == "init":
                # Protection comes first: a missing secret must not leave main unprotected.
                remote.init(protection_only(config), state)
                state = remote.snapshot()
            remote.init(config, state)
            state = remote.snapshot()
        problems = remote.check(config, state)
    except ValueError as error:
        parser.exit(1, f"error: {error}\n")
    except subprocess.CalledProcessError:
        parser.exit(1, "error: GitHub CLI command failed; check authentication and repository permissions.\n")
    for problem in problems:
        print(f"FAIL: {problem}")
    missing = [name for name, required in config.get("secrets", {}).items()
               if required and name not in state["secrets"]]
    instructions = token_instructions(repo, config, missing)
    if instructions:
        print(instructions)
    print("Secret values and token scopes cannot be verified through the repository secret API.")
    if not problems:
        print("GitHub repository configuration matches.")
    raise SystemExit(bool(problems))


if __name__ == "__main__":
    main()
