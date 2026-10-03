#!/usr/bin/env python3

# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Keep one durable tracking issue per Dependabot PR, including after a crash."""

import os
import re

from ci_automation import api, output, pr_number, repository


def marker(repo, number):
    return f"<!-- dependabot-tracking:{repo}#{number} -->"


def find_issue(issues, repo, number, pr_body):
    identity = marker(repo, number)
    # Recognize issues created by the source, even when its PR edit failed.
    legacy = f"Filed by `dependabot-rewrite` for #{number}."
    matches = [issue for issue in issues if "pull_request" not in issue
               and (identity in (issue.get("body") or "")
                    or legacy in (issue.get("body") or ""))]
    if len(matches) > 1:
        print("::warning::Existing duplicate tracking issues found; reusing the oldest without creating another")
    return min(matches, key=lambda item: item["number"]) if matches else None


def linked_body(body, repo, number, issue):
    identity = marker(repo, number)
    url = f"https://github.com/{repo}/issues/{issue['number']}"
    block = f"{identity}\nTracked by {url}.\n\nCloses #{issue['number']}"
    # Replace only our own block; preserve Dependabot and maintainer prose.
    pattern = re.escape(identity) + r"\nTracked by [^\n]+\n\nCloses #\d+"
    if re.search(pattern, body):
        return re.sub(pattern, lambda _: block, body)
    legacy = r"<!-- dependabot-tracking -->\nTracked by [^\n]+\n\nCloses #\d+"
    if re.search(legacy, body):
        return re.sub(legacy, lambda _: block, body)
    # A partially saved marker is repaired, not taken as proof of a link.
    body = body.replace(identity, "").rstrip()
    return f"{body}\n\n{block}".lstrip()


def track(client=api):
    repo, number = repository(), pr_number()
    endpoint = f"repos/{repo}/pulls/{number}"
    pr = client(endpoint)
    if (pr["user"]["login"] != "dependabot[bot]"
            or (pr["head"]["repo"] or {}).get("full_name") != repo):
        raise ValueError("Tracking requires a same-repository Dependabot PR")
    # No search index: scan every page, open AND closed issues. Missing a
    # page is an error, never a reason to assume the issue does not exist.
    pages = client(f"repos/{repo}/issues?state=all&sort=created&direction=asc&per_page=100", paginate=True)
    issue = find_issue([i for page in pages for i in page], repo, number, pr.get("body") or "")
    if issue is None:
        if pr["state"] != "open":
            return None
        rows = [line for line in (pr.get("body") or "").splitlines() if line.startswith("|")][:40]
        body = (f"{marker(repo, number)}\n\n## Dependabot update\n\n"
                f"Tracking https://github.com/{repo}/pull/{number}.\n\n"
                "The rewrite workflow re-authors and GPG-signs commits with the maintainer identity.\n\n"
                f"### Bump\n\n{pr['title']}\n")
        if rows:
            body += "\n<details><summary>Packages</summary>\n\n" + "\n".join(rows) + "\n\n</details>\n"
        # Never retry POST here. If its response is lost, the next serialized
        # run recovers the persisted marker instead of issuing a second POST.
        issue = client(f"repos/{repo}/issues", "POST",
                       {"title": f"track: {pr['title']}", "labels": ["dependencies"], "body": body})
    elif marker(repo, number) not in (issue.get("body") or ""):
        issue = client(f"repos/{repo}/issues/{issue['number']}", "PATCH",
                       {"body": f"{marker(repo, number)}\n\n{issue.get('body') or ''}"})
    # Fetch the latest description so a rerun/Dependabot edit does not cause
    # us to overwrite the older body captured before scanning issues.
    current = client(endpoint)
    body = linked_body(current.get("body") or "", repo, number, issue)
    if body != (current.get("body") or ""):
        client(endpoint, "PATCH", {"body": body})
    # If merging raced with link repair, the closing keyword arrived too
    # late for GitHub auto-close. Reconcile the issue state explicitly.
    current = client(endpoint)
    if current.get("merged") and issue["state"] == "open":
        client(f"repos/{repo}/issues/{issue['number']}", "PATCH",
               {"state": "closed", "state_reason": "completed"})
    return issue["number"]


if __name__ == "__main__":
    number = track()
    if number:
        output("issue", number)
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(f"\nTracking issue: https://github.com/{repository()}/issues/{number}\n")
