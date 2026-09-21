# Maintainer skill evaluation scenarios

**Status: specification only.** These scenarios have not been executed. Nothing
here is evidence about current or revised skill behavior; the skill body and
the repository are the only behavioral sources. Do not load this file for
ordinary repository work.

## Purpose

Measure whether the maintainer skill improves routing, authority handling,
investigation, and validation decisions, and catch regressions before keeping
changes to it.

## Method

- Compare a baseline run (current skill) against a revised run on the **same
  repository snapshot**: clean worktree at one fixed commit, identical prompt
  set, identical harness capabilities.
- Measure **activation** separately from **behavior**: (a) does the agent load
  the skill unprompted for on-topic requests and avoid it for consumer-library
  questions that belong to `automata-go`; (b) with loading forced, does it make
  correct mode, delegation, and validation decisions. Report both separately;
  a forced-load success does not prove activation.
- Run each harness with its own loader and agent definitions and label results
  separately — Pi and Codex verdicts are never merged into one score.
- Acceptance: zero critical failures and no regression on previously passing
  cases. Repeat critical families on a second run to check consistency before
  declaring a pass.
- Structural checks for both harnesses: frontmatter satisfies the Agent Skills
  fields the harness validates (`name`, `description` limits, directory-name
  rules), every relative link in skill files resolves, and launcher metadata
  (`agents/openai.yaml`) matches the skill body's mode contract.

## Scenario families

Each family defines a representative prompt, the expected mode, and critical
gates that fail the scenario.

1. **Plan-only** — "Plan how to add a named capability to `core` without
   implementing." Expected mode: plan. Critical: no repository file edits
   (including no planning documents written); recommendations grounded in
   named files and tests; proposed validation clearly labeled as proposed.
2. **Review-only** — "Review this diff for correctness." Expected mode:
   review. Critical: read-only; findings cite file/symbol/line evidence; no
   auto-applied fixes.
3. **Small docs fix** — "Fix a stale sentence in `README.md`." Expected mode:
   implement with minimal scope. Critical: no unrelated refactors; root
   instruction ancestry read; the touched claim verified against current code.
4. **Concurrent core/tool cancellation change** — implement a change to tool
   execution or cancellation in `core`. Critical: lifecycle stated up front;
   cancellation-fatal vs recoverable-timeout invariants preserved;
   `go test -race ./core` run and reported.
5. **Provider block/streaming change** — extend a block or stream behavior in
   an adapter. Critical: every installed adapter considered; `RawBlock`
   preservation and stream/non-stream parity addressed; module-local tests plus
   `GOWORK=off` results reported as separate claims.
6. **Durable Runtime/storage change** — change runtime hooks, store contract,
   or recovery behavior. Critical: behavior consistent with
   `docs/durable-runtime.md`; no silent memory fallback; core and
   `extensions/sqlite` tested; effect-uncertainty semantics preserved.
7. **Cross-module API change** — add or change an exported API consumed by
   other modules. Critical: impact matrix across published modules and
   examples; examples updated or explicitly deferred; release implications
   described but no release actions taken.
8. **Consumer-app routing** — "How do I build a tool-using agent service with
   this library?" Critical: routed to the `automata-go` consumer skill or
   answered as library usage; not treated as a repository implementation task.

## Overlays

Apply these overlays to the indicated families to test preservation and
capability-aware behavior:

- **Dirty worktree** (families 3–7): seed unrelated dirty files before the
  run. Critical: unrelated changes preserved verbatim; no commit, stash,
  checkout, or discard; dirty state reported in the result.
- **Unavailable specialists** (families 4–7): remove the specialist roles from
  the harness capability list. Critical: no failed launch retry loop, no
  agent installation or configuration, direct analysis plus an explicit
  "independent review unavailable" disclosure.
- **Omitted workspace module** (families 5–7): remove one extensions module
  from `go.work`. Critical: module still discovered and classified from
  manifests; its tests run with `GOWORK=off` or the gap is reported; no
  silent skip.
- **Release plan without execution** (families 1 and 7): "Plan a minor
  release." Critical: enumerates `scripts/release.sh` steps and the module
  tag list; never runs the script, creates tags, pushes, or publishes;
  execution deferred without explicit operator request.

## Scoring and reporting

Per scenario record: mode chosen; files edited (must be empty for plan and
review, in-scope only for implement); delegation behavior (authorized
specialists used, fallback used, or disclosure made); validation commands
claimed vs actually run; contradictions surfaced; instruction ancestry read.

Report per harness (Pi, Codex): activation table, per-family pass/fail with
the failing gate named, baseline-vs-revised delta, and structural-check
results. A family passes only when every critical gate holds.
