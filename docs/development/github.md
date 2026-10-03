# GitHub automation

Repository configuration is declared in `.github/repository.toml`. It covers
the labels used by issue templates and Dependabot, signing identity variables,
required signing secrets, workflow token permissions and the `main` ruleset.

## Check the repository

Authenticate `gh` with an account that can read repository settings and Actions
secrets, then run:

```console
python3 scripts/init_github.py check --repo goabonga/maestro
```

`check` performs reads only. It exits with status 1 when managed configuration
is missing or differs. GitHub exposes secret names, not secret values: presence
checks cannot prove that a token has the right scopes or that a private key
matches the configured signing identity.

## Tokens by responsibility

Create separate fine-grained PATs for the owner `goabonga`, restricted to the
`maestro` repository. Save them as **Actions repository secrets**, not Dependabot
secrets: these workflows execute in a trusted Actions context.

| Secret | Responsibility | Repository permissions |
| --- | --- | --- |
| `DEPENDABOT_PAT` | Push rewritten dependency commits and read the current PR | Contents: write; Pull requests: read; Workflows: write |
| `AUTO_MERGE_PAT` | Check authorization, push signed commits, fast-forward `main`, report failures | Contents: write; Pull requests: read; Workflows: write; Issues: write |
| `RELEASE_PAT` | Push signed documentation version commits and tags; create GitHub releases | Contents: write |

PAT setup instructions are declared under `[tokens]` in
`.github/repository.toml`. Both `check` and `init` display the instructions for
missing PATs: creation URL, resource owner, selected repository, permission
levels and the token owner's required repository role. These declarations are
guidance; the script cannot inspect permissions inside an existing secret.

Metadata read access is implicit. The merge and release tokens must belong to the maintainer
whose repository role is allowed by the ruleset's administrator bypass. They do
not need permission to change policies. Tracking issue creation and PR linking
use the workflow's own `GITHUB_TOKEN`, with explicitly declared permissions.
The dependency token is used only for the signed push and its PR validation.
The merge workflow reads check runs and commit statuses using its own
`GITHUB_TOKEN`, with `checks: read` and `statuses: read`. Its dedicated PAT handles
PR authorization, Git pushes and issue comments. Select only the PAT permissions
in the table; the workflow token supplies the CI read permissions.
See GitHub's [PAT permission reference](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens),
[check-run permissions](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference)
and [collaborator permission checks](https://docs.github.com/en/rest/collaborators/collaborators#get-repository-permissions-for-a-user).

For local initialization, use a separate administrative `gh` login. `check` needs
Administration, Secrets, Variables and Issues read access; `init` needs
write access for those settings. No automation PAT needs Secrets or Variables
access. GitHub's [PAT setup guide](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
explains creation, repository selection and expiration.

## Initialize or repair configuration

Review `.github/repository.toml`, then apply it:

```console
python3 scripts/init_github.py init --repo goabonga/maestro
```

To apply only the branch protection, which needs no secrets and can run on
a fresh repository, use `protect`. It reconciles the `main-protection` ruleset —
covering `main` and `develop` — and leaves settings, labels, variables and
secrets untouched:

```console
python3 scripts/init_github.py protect --repo goabonga/maestro
```

Existing secrets are preserved unless their names are explicitly supplied as
environment variables. Missing required secrets must be provided this way before
initialization can proceed. Values are passed to `gh secret set` through standard
input; they are never stored in the configuration or printed. Initialization
requires write access to repository administration, Actions secrets and variables.

`DEPENDABOT_PAT`, `AUTO_MERGE_PAT` and `RELEASE_PAT` must be configured before
enabling their workflows on `main`. Each workflow uses only its dedicated token. Supply each value through its matching environment variable when running
`init`, or store it directly with `gh secret set NAME --repo goabonga/maestro`.
Creating a PAT is a separate GitHub account operation; the script stores supplied
values but cannot generate tokens or recover existing secret values.

`init` always applies the `main-protection` ruleset first. If required secrets are
missing, it then exits with status 1 without changing the other configuration and lists the
commands needed to supply them. Create
each dedicated PAT with the permissions listed above, then paste its value into
the GitHub CLI's interactive prompt:

```console
gh secret set DEPENDABOT_PAT --repo goabonga/maestro
gh secret set AUTO_MERGE_PAT --repo goabonga/maestro
python3 scripts/init_github.py init --repo goabonga/maestro
python3 scripts/init_github.py check --repo goabonga/maestro
```

See the [GitHub CLI secret command](https://cli.github.com/manual/gh_secret_set).
The initializer preserves existing secrets; it cannot retrieve their values or
automatically create personal access tokens.

The command updates existing managed labels, variables and rulesets rather than
creating duplicates. It leaves unrelated labels, variables, secrets and rulesets
alone. A failed run can be retried: it reads the actual remote configuration again
and applies the remaining differences. The named managed ruleset is authoritative;
review its complete definition before applying changes to an existing repository.

The administrator bypass is retained from the source so the signed merge and
release workflows can fast-forward `main`. The merge workflow verifies the
maintainer's authorization, the current PR head and successful CI before and
after signing. The release workflow runs after successful CI validation
and refuses to release an unvalidated newer commit.

## Dependabot tracking issues

The rewrite workflow creates one tracking issue per PR. Its durable identity
includes the repository and PR number and is stored in the issue body. It scans
all pages of open and closed issues before creating anything, including legacy
tracking issues from the source. Changing the PR title or replacing its description
does not create another issue.

Issue creation and PR linking happen before pushing signed commits. If a response
is lost or linking fails, rerunning the workflow reuses the persisted issue and
repairs the link. `Closes #…` in the PR description closes the issue on merge;
the script also repairs closure if merging races with link repair. Existing
duplicates are reported and the oldest issue is reused; they are not deleted.

## Changed components in CI

Each validation job runs only for what the change touches. Jobs are:

| Job | Runs when | Checks |
| --- | --- | --- |
| `audit` | always | Plumber compliance |
| `detect changed components` | always, after `audit` | multicz configuration, changed components |
| `check license headers` | at least one component changed, after detection | SPDX headers |
| `check commit signatures` | every pull request | each PR commit carries a signature verified by GitHub |
| `validate scripts` | `maestro-scripts` changed | byte-compilation, and the tests the change affects |
| `validate go` | a registered Go component changed, through its own paths or its imports | formatting, vet, race tests, build and gosec |
| `validate documentation` | `maestro-docs` changed (`docs/**`, `zensical.toml`, `assets/maestro.svg`) | generated branding, documentation build |

Detection runs `multicz changed` and nothing else: a component changed when it has
commits since its latest tag, exactly as in the reference pipeline. A commit that changes
no component, such as a `chore`, therefore runs no validation, no release and no
documentation publication. Only `audit` and detection itself always run.
Scripts and their tests are selected from the diff against the PR base or the previous
push, which only narrows which test files run.

Every job selects its work from the `changed` output of `multicz changed`, which is the
only source of component changes. Its inputs are:

- each component's `paths` globs;
- the `go-deps` plugin, which adds the files a Go component imports (its command
  packages are registered under `plugins.go-deps.packages`). A change in
  `internal/transport` therefore selects `maestro` and `maestro-svc`;
- `overlap_policy = "all"` for shared module or workflow changes.

`maestro-docs` declares no `depends_on`, so only changes under its own paths select it.
The version table in `zensical.toml` is still refreshed on every release, because each
component bumps its own key there.

Script validation compiles every script and runs the test files that import a changed
module. Changes to `scripts/ci_automation.py`, `scripts/dependency_release.py`,
`scripts/pyproject.toml`, `scripts/uv.lock` or `pytest.ini` run the whole suite, as
does any script without a test that imports it.

Go validation is a single job rather than a matrix. Matrix job names depend on the
components that changed, so they cannot be required checks.

### Required checks

The branch ruleset requires the six job names above. A job skipped because its
component did not change counts as passing. A failed or cancelled job blocks the
merge, and a skipped job is still allowed only if its `if` condition was false.

The `release-bump` job runs only when some component changed and no validation job failed
or was cancelled (`!failure() && !cancelled()`), so a failing check stops the release.

## Component releases and documentation publication

Releases are driven entirely by multicz. Nothing in the repository reads version
files or computes tags.

1. `release-bump` runs only on `main`, after every validation job succeeded or was
   skipped, and never on a `chore(release):` commit. It runs `multicz plan`. If the
   plan is empty, the job stops: no commit, no tag, no release. Otherwise it runs
   `multicz bump --commit --tag --sign --push`, which writes the version files and
   changelogs, creates one signed tag per bumped component, and pushes them.
2. `release` runs one job per bumped tag. It checks out the tag, builds the assets
   with steps in the job, and creates the GitHub release with
   `multicz release-notes --tag` as notes.
3. `pages` builds and deploys the documentation from the bump commit, which contains the
   refreshed version table.

Assets are linux `amd64` and `arm64` binaries for `maestro` and `maestro-svc`, a
`tar.gz` of the built site for `maestro-docs`, and a `-checksums.txt` file with SHA-256
digests. `maestro-scripts` ships no asset.

A component is first released by its first `feat` or `fix` commit, as `0.1.0` or
`0.0.1` respectively. A `chore` commit never bumps a component.

The release PAT pushes the bump commit and creates the releases. The Pages job uses the
workflow's `GITHUB_TOKEN` with `pages: write` and `id-token: write`, independently of the
release PAT. Configure the repository's Pages publishing source as **GitHub Actions**,
with the `github-pages` environment permitting `main`. See GitHub's
[custom Pages workflow requirements](https://docs.github.com/en/pages/getting-started-with-github-pages/using-custom-workflows-with-github-pages).

With no planned bump, no untagged new component and no pending recovery, no new commit, tag, release or
deployment is created.
The signed release commit's follow-up CI validates it but skips the release job
so it cannot start a release loop. Runs on `main` are not cancelled by newer pushes.

If release creation fails after the atomic push, rerun the workflow: it reuses
the existing signed tag and commit and repairs a missing or draft GitHub release.
An already persisted release is reused even if its creation response was lost.
If Pages fails, rerun the failed job. A manual **Run workflow** on `main` can also
republish the current tagged documentation version without bumping it. Manual
runs on other branches cannot publish. A run whose validated commit has been
superseded leaves publishing to a workflow validating the newer `main`.

## Test the scripts

Every workflow writes its job summary through `ci_automation.py summary` in an
`always()` step. The signing workflows and Dependabot signal use the script from
the trusted default-branch checkout. Summaries append to existing output and
include the result, PR and signing identity when that context is available.

```console
uv tool install multicz --with multicz-go-deps-plugin
make check
# Run only the script checks:
make scripts-check
```

Tests use pytest functions and fixtures. The suite uses disposable repositories
and test GPG keys for signing,
fake GitHub clients for mutations and real SVG conversion for favicon validation.
It never changes repository settings, creates GitHub issues or merges a PR.
