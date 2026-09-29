# CI and module releases

Automata publishes Go modules, not application binaries. Versions come from
immutable Git tags. A release never bumps dependencies, edits source, commits
files, or pushes a branch.

## One-time GitHub setup

1. Merge `.github/workflows/ci.yml` and `release.yml` onto `main`.
2. In **Settings → Environments**, create an environment named **release**:
   - Configure at least one **required reviewer**. Publication fails closed if
     the environment is missing or has no required-reviewer rule.
   - Restrict deployment branches to `main`.
   - Choose whether self-review is allowed. A solo maintainer must be able to
     approve their own dispatch if they are the only reviewer.
3. Protect `main` with the required **CI result** check, which aggregates every
   module check. Consider tag rules preventing updates and deletion of published
   version tags; allow the release job to create them.
4. Enable Actions. Only the approved publication job has `contents: write`; it
   also has `actions: read` to verify environment protection. No PAT, GitHub App,
   provider credentials, or repository write secret is needed.

GitHub environment protection availability depends on repository visibility and
plan. If required reviewers are unavailable, this pipeline intentionally refuses
publication rather than silently skipping approval.

Third-party actions are pinned to commit SHAs. Go is selected from `go.mod`.
The Ubuntu runner supplies Bash, jq, Python 3, Git, and the GitHub CLI.

## Normal development

`scripts/modules.json` is the shared inventory for CI, release eligibility,
race targets, and consumer smoke imports. Keep it aligned with `go.work` when
adding modules. Examples are build-only and cannot be released.

CI tests every published module against the current workspace, builds examples
without credentials, and runs the inventory's race targets (root, SQLite, and
MCP). It checks module
identity, the dependency-free root, Go formatting, and release-tool tests.
Example executables and consumer projects are built outside the checkout.

Workspace success proves integration of the current source; it does **not**
prove that an adapter builds against its published dependency requirements.
An adapter using unreleased core may pass workspace CI but is not release-ready.

## Dependency policy

Internal requirements are minimum compatible versions, not coordinated release
numbers. Leave them unchanged unless a newer API or runtime fix is needed.
For example, `extensions/openai/v0.5.3` may still require core `v0.5.2`.
Installing newer core explicitly raises the selected version through Go's
minimal version selection.

If an adapter needs new core functionality:

1. Release core first.
2. In a normal PR, update the adapter's requirement to that published version,
   remove any development replacements, run `GOWORK=off go mod tidy`, and commit
   its finalized `go.mod` and `go.sum`.
3. Test and merge the PR, then release the adapter.

For a change spanning core, tools, and Tavily, publish in that dependency order.
Other modules and examples do not receive automatic version bumps. The release
check rejects **all** replacements and dependency exclusions: consumers ignore
those directives in a dependency's `go.mod`.

## Requesting a release

In **Actions → Release module → Run workflow**, choose branch `main` and supply:

- **module**: an inventory path, such as `.`, `tools`, or `extensions/openai`.
- **version**: an exact stable version, such as `v0.5.3`.
- **commit**: optionally a full lowercase SHA on `main`. Empty means the commit
  captured when dispatching, not whichever commit is newest after approval.

The selected commit must contain the pipeline tooling. Release one module per
workflow. Prereleases and v2+ path migrations are deliberately outside this
initial workflow.

| Module | Tag |
| --- | --- |
| Root (`core`, `retry`, `tracing`) | `v0.5.3` |
| Tools | `tools/v0.5.3` |
| Provider/store/backend extension | `extensions/<name>/v0.5.3` |

The read-only job checks the exact source SHA, version progression, existing
tags, formatting, tooling tests, all workspace integration/examples, and the
candidate with `GOWORK=off`: `go mod tidy -diff`, tests, vet, and race tests where
configured. Dependency and checksum files must already be final.

Review the candidate SHA/tag in the workflow summary and approve the **release**
environment. The publication job pushes only the selected tag, without force,
and creates module-filtered GitHub release notes. New root releases are marked
as the repository-wide **Latest**; `tools` and extension releases are not.
Completing an older partial root release does not displace a newer root version.

**The tag publishes the Go module.** GitHub Release creation is accompanying
metadata, not a way to keep a tagged version private. The consumer job therefore
runs in this same workflow after publication; it does not depend on a tag push
triggering another workflow through `GITHUB_TOKEN`.

Consumer verification uses a temporary module, a fresh module cache,
`GOWORK=off`, `proxy.golang.org`, and `sum.golang.org`. It requests the exact
version, rejects replacements/version drift, and compiles an import smoke test.
It makes no live provider calls. Proxy/checksum propagation receives six bounded
attempts with exponential delays; no direct-fetch fallback masks a proxy failure.

## Recovery

Publication is serialized and an active release is never canceled by a later
request. GitHub's default concurrency behavior retains at most one pending
request, so dispatch additional modules after the active release completes.

- **Validation fails:** fix the issue in a normal PR; nothing was published.
- **Tag pushed, metadata creation fails:** rerun the workflow for the **same
  module, version, and SHA**. A matching tag is reused; a conflicting tag fails.
- **Consumer check fails:** the version is already public. Rerun the failed
  consumer job for propagation failures. For a real code/dependency problem,
  publish a new corrected version; never move or delete the old tag.
- **Completed workflow rerun:** matching tags/releases are left untouched and
  consumer verification runs again.

Local publication through the old `scripts/release.sh patch --push` interface
has been removed. `release.sh` is a CI-only publication entry point and requires
both workflow approval and the protected release environment. Do not execute it
as a local validation command.

## Local validation (no publication)

```sh
scripts/check-modules.sh inventory
scripts/check-modules.sh format
scripts/check-modules.sh workspace
scripts/check-modules.sh race .
scripts/check-modules.sh race extensions/sqlite
scripts/check-modules.sh race extensions/mcp
scripts/check-modules.sh release tools # isolated release-readiness check
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/tests -v
```

The tooling tests source shared definitions in temporary fixtures with mocked
Git, Go, and GitHub commands. They never execute `release.sh`, create real tags,
push refs, publish releases, or contact providers.
