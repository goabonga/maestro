#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Trusted GitHub automation; checked-out PR files are data, never executed."""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import time
import tomllib
from urllib.parse import quote


def run(*args, **kwargs):
    return subprocess.check_output(args, **kwargs).decode().strip()


def git(*args):
    return run("git", *args)


def api(endpoint, method="GET", payload=None, paginate=False, token=None):
    command = ["gh", "api", endpoint, "--method", method]
    if paginate:
        command += ["--paginate"]
    options = {}
    if token is not None:
        if not token:
            raise ValueError("An explicit GitHub token must not be empty")
        options["env"] = {**os.environ, "GH_TOKEN": token}
    if payload is not None:
        command += ["--input", "-"]
        options["input"] = json.dumps(payload).encode()
    response = run(*command, **options)
    if paginate:
        # gh --paginate prints consecutive JSON documents. Decode each
        # complete page without depending on newer gh's --slurp flag.
        decoder = json.JSONDecoder()
        pages = []
        while response.strip():
            response = response.lstrip()
            page, offset = decoder.raw_decode(response)
            pages.append(page)
            response = response[offset:]
        return pages
    return json.loads(response) if response else None


def repository():
    value = os.environ["GH_REPO"]
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", value):
        raise ValueError("Invalid repository")
    return value


def pr_number():
    return int(os.environ.get("PR_NUMBER") or os.environ["PR"])


def pull_request():
    return api(f"repos/{repository()}/pulls/{pr_number()}")


def validate_head(sha, branch):
    if not re.fullmatch(r"[a-f0-9]{40}", sha):
        raise ValueError("Invalid commit SHA")
    subprocess.run(["git", "check-ref-format", f"refs/heads/{branch}"], check=True)


def output(name, value):
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as stream:
        print(f"{name}={value}", file=stream)


def validate_dependabot():
    sha, branch = os.environ["SIGNAL_SHA"], os.environ["SIGNAL_BRANCH"]
    validate_head(sha, branch)
    if not branch.startswith("dependabot/"):
        raise ValueError("Expected a Dependabot branch")
    owner = repository().split("/")[0]
    pages = api(f"repos/{repository()}/pulls?state=open&head={quote(owner + ':' + branch, safe='')}&per_page=100", paginate=True)
    matches = [p for page in pages for p in page
               if p["user"]["login"] == "dependabot[bot]"
               and (p["head"]["repo"] or {}).get("full_name") == repository()
               and p["head"]["sha"] == sha and p["head"]["ref"] == branch
               and p["base"]["ref"] == os.environ["BASE_BRANCH"]]
    if len(matches) != 1:
        raise ValueError("Expected one current same-repository Dependabot PR")
    output("number", matches[0]["number"])


def merge_authorized(pr, sha):
    return (pr["state"] == "open" and not pr["draft"]
            and (pr["head"]["repo"] or {}).get("full_name") == repository()
            and pr["head"]["sha"] == sha and pr["head"]["ref"] == os.environ["HEAD_REF"]
            and pr["base"]["ref"] == "main" and pr["head"]["ref"] != "main"
            and any(label["name"] == os.environ["MERGE_LABEL"] for label in pr["labels"]))


def check_labeller():
    permission = api(f"repos/{repository()}/collaborators/{quote(os.environ['LABELLER'], safe='')}/permission")["permission"]
    if permission not in {"admin", "maintain", "write"}:
        raise ValueError("The labeller no longer has write access")


def validate_merge():
    validate_head(os.environ["HEAD_SHA"], os.environ["HEAD_REF"])
    if not os.environ.get("GH_TOKEN") or not os.environ.get("GPG_PRIVATE_KEY"):
        raise ValueError("Merge token and signing key are required")
    if not merge_authorized(pull_request(), os.environ["HEAD_SHA"]):
        raise ValueError("Pull request changed or merge authorization was removed")
    check_labeller()


def configure_signing():
    for name in ("GPG_PRIVATE_KEY", "PUSH_TOKEN", "GIT_USER_NAME", "GIT_USER_EMAIL", "GIT_SIGNING_KEY"):
        if not os.environ.get(name):
            raise ValueError(f"Missing {name}")
    home = Path(tempfile.mkdtemp(prefix="maestro-signing-", dir=os.environ["RUNNER_TEMP"]))
    home.chmod(0o700)
    os.environ["GNUPGHOME"] = str(home)
    # Register cleanup before any import or key validation can fail.
    with Path(os.environ["GITHUB_ENV"]).open("a") as stream:
        print(f"GNUPGHOME={home}", file=stream)
    subprocess.run(["gpg", "--batch", "--import"],
                   input=os.environ["GPG_PRIVATE_KEY"].encode(), check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["gpg", "--batch", "--list-secret-keys", os.environ["GIT_SIGNING_KEY"].rstrip("!")],
                   check=True, stdout=subprocess.DEVNULL)
    passphrase = home / "passphrase"
    passphrase.write_text(os.environ.get("GPG_PASSPHRASE", ""))
    passphrase.chmod(0o600)
    wrapper = home / "git-gpg"
    wrapper.write_text("#!/usr/bin/env python3\nimport os, sys\n"
                       "os.execvp('gpg', ['gpg', '--batch', '--pinentry-mode', 'loopback', "
                       "'--passphrase-file', os.path.join(os.environ['GNUPGHOME'], 'passphrase'), *sys.argv[1:]])\n")
    wrapper.chmod(0o700)
    for key, value in (("user.name", os.environ["GIT_USER_NAME"]),
                       ("user.email", os.environ["GIT_USER_EMAIL"]),
                       ("user.signingkey", os.environ["GIT_SIGNING_KEY"]),
                       ("gpg.program", str(wrapper)), ("commit.gpgsign", "true"),
                       ("core.hooksPath", "/dev/null")):
        git("config", key, value)


def cleanup():
    home = Path(os.environ.get("GNUPGHOME", "/nonexistent"))
    root = Path(os.environ["RUNNER_TEMP"]).resolve()
    if home.parent.resolve() == root and home.name.startswith("maestro-signing-") and not home.is_symlink():
        subprocess.run(["gpgconf", "--kill", "gpg-agent"], check=False)
        shutil.rmtree(home, ignore_errors=True)


def rebase(mode):
    git("config", "core.hooksPath", "/dev/null")
    if mode == "merge":
        git("fetch", "origin", "main")
        base = git("rev-parse", "origin/main")
        Path(os.environ["RUNNER_TEMP"], "merge-base-sha").write_text(base)
        script = Path(__file__).with_name("sign_commit.py")
    else:
        base = git("merge-base", f"origin/{os.environ['BASE_BRANCH']}", "HEAD")
        script = Path(__file__).with_name("rewrite_dependabot_commit.py")
    if git("rev-list", "--merges", f"{base}..HEAD"):
        raise ValueError("Merge commits cannot be automatically replayed")
    subprocess.run(["git", "rebase", "--force-rebase", "--empty=keep", base,
                    "--exec", shlex.join(["python3", str(script)])], check=True)


def push_signed(mode):
    sha = os.environ.get("EXPECTED_SHA") or os.environ["HEAD_SHA"]
    pr = pull_request()
    branch = os.environ["HEAD_REF"]
    validate_head(sha, branch)
    if mode == "merge":
        if not merge_authorized(pr, sha):
            raise ValueError("Pull request changed or merge authorization was removed")
        check_labeller()
    elif not (pr["state"] == "open" and pr["user"]["login"] == "dependabot[bot]"
              and (pr["head"]["repo"] or {}).get("full_name") == repository()
              and pr["head"]["sha"] == sha and pr["head"]["ref"] == branch):
        raise ValueError("Dependabot PR is stale")
    authenticated_git("push", f"--force-with-lease=refs/heads/{branch}:{sha}", "origin", f"HEAD:refs/heads/{branch}")


def authenticated_git(*args):
    subprocess.run(["git", "-c", "credential.helper=", "-c",
                    "credential.helper=!gh auth git-credential", *args], check=True)


def checks_ready(runs, statuses, required="validate"):
    # A rerun supersedes older attempts with the same app and check name.
    latest = {}
    for check in runs:
        key = (check.get("app", {}).get("id"), check["name"])
        if key not in latest or check["id"] > latest[key]["id"]:
            latest[key] = check
    checks = [c for c in latest.values() if c["name"] != "rebase, sign and fast-forward into main"]
    if any(c["status"] == "completed" and c["conclusion"] not in {"success", "neutral", "skipped"} for c in checks):
        raise ValueError("A check failed")
    if statuses.get("statuses") and statuses["state"] in {"failure", "error"}:
        raise ValueError("A commit status failed")
    return (any(c["name"] == required and c["conclusion"] == "success" for c in checks)
            and all(c["status"] == "completed" for c in checks)
            and (not statuses.get("statuses") or statuses["state"] == "success"))


def wait_checks(signed=False):
    token = os.environ.get("CHECKS_TOKEN")
    if not token:
        raise ValueError("CHECKS_TOKEN is required to read CI with the workflow token")
    sha = git("rev-parse", "HEAD") if signed else os.environ["HEAD_SHA"]
    deadline = time.monotonic() + 25 * 60
    while time.monotonic() < deadline:
        if not merge_authorized(pull_request(), sha):
            raise ValueError("Pull request changed while waiting for checks")
        pages = api(f"repos/{repository()}/commits/{sha}/check-runs?per_page=100&filter=latest", paginate=True, token=token)
        runs = [c for page in pages for c in page["check_runs"]]
        statuses = api(f"repos/{repository()}/commits/{sha}/status", token=token)
        if checks_ready(runs, statuses):
            return
        time.sleep(30)
    raise TimeoutError("Timed out waiting for CI; re-apply the auto-merge label")


def merge():
    sha = git("rev-parse", "HEAD")
    if not merge_authorized(pull_request(), sha):
        raise ValueError("Pull request changed or merge authorization was removed")
    check_labeller()
    git("fetch", "origin", "main")
    if git("rev-parse", "origin/main") != Path(os.environ["RUNNER_TEMP"], "merge-base-sha").read_text():
        raise ValueError("Main advanced; re-apply the auto-merge label")
    authenticated_git("push", "origin", "HEAD:refs/heads/main")
    api(f"repos/{repository()}/issues/{pr_number()}/comments", "POST",
        {"body": f"Merged into `main` as `{sha}`, GPG-signed with the maintainer key. History stays linear."})


def summary(title, body=""):
    text = f"## {title}\n\n**Result:** {os.environ.get('JOB_STATUS', 'unknown')}\n"
    if "PR_NUMBER" in os.environ or "PR" in os.environ:
        number = os.environ.get("PR_NUMBER") or os.environ.get("PR") or "not validated"
        text += f"\nPull request: {number}\n"
    if os.environ.get("GIT_USER_NAME") or os.environ.get("GIT_USER_EMAIL"):
        text += (f"\nIdentity: {os.environ.get('GIT_USER_NAME', '')} "
                 f"<{os.environ.get('GIT_USER_EMAIL', '')}>\n")
    if body:
        text += f"\n{body}\n"
    with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a") as stream:
        stream.write(text)


def ci_comparison():
    event = os.environ.get("GITHUB_EVENT_NAME")
    reference = os.environ.get("PR_BASE_SHA" if event == "pull_request" else "PUSH_BEFORE_SHA", "")
    if event not in {"pull_request", "push"} or not re.fullmatch(r"[0-9a-f]{40}", reference) or reference == "0" * 40:
        return None
    try:
        git("merge-base", "--is-ancestor", reference, "HEAD")
    except subprocess.CalledProcessError:
        return None
    return reference


# Changes to these files affect every script test, so the whole suite runs.
SHARED_SCRIPT_FILES = {"scripts/ci_automation.py", "scripts/dependency_release.py",
                       "scripts/pyproject.toml", "scripts/uv.lock", "pytest.ini"}


def script_tests(since):
    """Test files to run for a change, or None when the whole suite must run."""
    if not since:
        return None
    files = git("diff", "--name-only", since, "HEAD").splitlines()
    selected = set()
    for path in files:
        if not path.startswith("scripts/") or path == "scripts/CHANGELOG.md":
            continue
        if path in SHARED_SCRIPT_FILES or path.startswith("scripts/tests/conftest"):
            return None
        if path.startswith("scripts/tests/test_") and path.endswith(".py"):
            selected.add(path)
        elif path.startswith("scripts/") and path.count("/") == 1 and path.endswith(".py"):
            module = Path(path).stem
            matches = [str(test) for test in sorted(Path("scripts/tests").glob("test_*.py"))
                       if re.search(rf"^\s*import {re.escape(module)}\b", test.read_text(), re.M)]
            if not matches:
                return None
            selected.update(matches)
        else:
            return None
    return sorted(selected)


def changed_components(since=None, ci=False):
    """Components with commits since their latest tag, as multicz computes them.

    The same rule drives every job, so a commit that changes no component
    (a chore, for example) runs no validation and no release.
    """
    if ci and since:
        raise ValueError("Use either --ci or --since")
    command = ["multicz", "changed", "--output", "json"]
    if since:
        command += ["--since", since]
    changed = json.loads(run(*command))["changed"]
    if not isinstance(changed, list) or not all(isinstance(name, str) for name in changed):
        raise ValueError("Invalid multicz component list")
    encoded = json.dumps(changed, separators=(",", ":"))
    print(f"Changed components: {encoded}")
    output("changed", encoded)
    if ci:
        config = tomllib.loads(Path("multicz.toml").read_text())
        registered = config.get("plugins", {}).get("go-deps", {}).get("packages", {})
        output("go", json.dumps([name for name in changed if name in registered], separators=(",", ":")))
        # Base for selecting script test files only; it does not decide which components changed.
        output("base", ci_comparison() or "")
    listed = "\n".join(f"- `{name}`" for name in changed) if changed else "No changed components."
    summary("Changed components", f"Each component's commits since its latest tag.\n\n{listed}")


def verify_signatures():
    """Every commit of the pull request must carry a signature GitHub verifies."""
    pages = api(f"repos/{repository()}/pulls/{pr_number()}/commits?per_page=100", paginate=True)
    commits = [commit for page in pages for commit in page]
    if not commits:
        raise ValueError("No commits found for this pull request")
    failures = []
    for commit in commits:
        verification = commit["commit"].get("verification") or {}
        subject = commit["commit"]["message"].splitlines()[0]
        if verification.get("verified") is True:
            print(f"ok {commit['sha'][:10]} {subject}")
        else:
            reason = verification.get("reason", "unknown")
            failures.append(f"{commit['sha'][:10]} ({reason}) {subject}")
            print(f"UNSIGNED {commit['sha'][:10]} ({reason}) {subject}")
    summary("Commit signatures", f"{len(commits)} commits checked.\n\n"
            + ("\n".join(f"- `{failure}`" for failure in failures) or "All signatures verified."))
    if failures:
        raise ValueError("Unverified commit signatures: " + ", ".join(failures))


def script_checks(since):
    """Byte-compile every script, then run the pytest files the change affects."""
    subprocess.run(["python3", "-m", "compileall", "-q", "scripts", "-x", "/\\.venv/"], check=True)
    tests = script_tests(since)
    if tests is not None and not tests:
        print("No script tests affected by this change.")
        return
    command = ["uv", "run", "--project", "scripts", "--locked", "pytest"]
    print("Script tests: " + ("full suite" if tests is None else ", ".join(tests)))
    subprocess.run(command + (tests or []), check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["validate-dependabot", "validate-merge", "configure-signing",
                        "cleanup", "rebase", "push", "wait", "merge", "summary", "changed-components",
                        "script-checks", "verify-signatures"])
    parser.add_argument("--mode", choices=["merge", "dependabot"], default="dependabot")
    parser.add_argument("--signed", action="store_true")
    parser.add_argument("--title", default="GitHub automation")
    parser.add_argument("--body", default="", help="Additional job summary text")
    parser.add_argument("--since", help="Git reference for multicz component comparison")
    parser.add_argument("--ci", action="store_true", help="Compare the current CI event; validate all when no safe base exists")
    args = parser.parse_args()
    actions = {"validate-dependabot": validate_dependabot, "validate-merge": validate_merge,
               "configure-signing": configure_signing, "cleanup": cleanup,
               "rebase": lambda: rebase(args.mode), "push": lambda: push_signed(args.mode),
               "wait": lambda: wait_checks(args.signed), "merge": merge,
               "summary": lambda: summary(args.title, args.body),
               "changed-components": lambda: changed_components(args.since, args.ci),
               "script-checks": lambda: script_checks(args.since),
               "verify-signatures": verify_signatures}
    try:
        actions[args.command]()
    except (ValueError, TimeoutError, subprocess.CalledProcessError) as error:
        if args.mode == "merge" and args.command in {"validate-merge", "wait", "rebase", "push", "merge"}:
            # Retrying is explicit: fix the problem, then re-apply the label.
            # No PR code runs here, including when a rebase fails.
            try:
                api(f"repos/{repository()}/issues/{pr_number()}/comments", "POST",
                    {"body": f"auto-merge-signed: {error}. Fix the problem and re-apply the `{os.environ['MERGE_LABEL']}` label."})
                run("gh", "api", "--method", "DELETE",
                    f"repos/{repository()}/issues/{pr_number()}/labels/{quote(os.environ['MERGE_LABEL'], safe='')}")
            except subprocess.CalledProcessError:
                pass
        raise


if __name__ == "__main__":
    main()
