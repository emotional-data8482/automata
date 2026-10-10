---
name: automata-maintainer
description: Implement, review, or plan changes inside the Automata Go repository, including core runtime behavior, provider adapters, first-party tools, examples, and multi-module compatibility. Use for work on this repository itself; use automata-go for applications that consume the library.
---

# Maintain Automata

Work from the repository's `AGENTS.md` files and preserve the provider-neutral
boundaries and tested execution semantics. This skill serves three modes, and
the first action is always the same.

## 1. Classify the mode before anything else

Decide which mode the request is in, and state it:

- **Plan** — read, analyze, and recommend. No file edits, including planning
  and design documents; writing a plan into the repository is a mutation and
  needs explicit authorization.
- **Review** — report evidence-backed findings with file, symbol, and line
  references. No fixes unless the operator asks for a fix pass.
- **Implement** — mutate only within the explicitly granted scope, and only
  after the mode and scope are established.

If the mode or scope is ambiguous, ask before editing. If instructions,
documentation, tests, and code materially contradict each other, stop and
report the contradiction instead of silently choosing a side. Distinguish
proposed checks ("should run `go test ./core`") from executed checks ("ran,
passed") in every report.

## 2. Establish ground truth

- Run `git status` before any work. Never commit, stash, revert, or discard
  unrelated dirty state; work around it and report it in the result.
- Read the complete applicable instruction ancestry, not just the closest
  file: root `AGENTS.md` plus the `AGENTS.md` of every affected module and
  package (`core/`, `tools/`, `extensions/`, `extensions/<name>/`,
  `examples/`, `retry/`, `tracing/`). Nested guidance narrows root guidance;
  apply the most specific rules first.
- Ground claims in current code, tests, `README.md`, and `docs/`. Treat
  `planning/projects/` status as history when marked completed and as design
  context only when proposed — never as implemented behavior.

## 3. Identify the layer before applying invariants

`core.Runtime` is the only execution lifecycle; the direct `Agent.Run`,
`Session`, `RunTyped`, and `AsTool` entry points were removed in T09. State
which layer the change touches:

- **Runtime** — admission, transitions, recovery, waits, children,
  conversations, observation, retention, the `core.Store` contract and its
  adapters, and tool-effect reconciliation. `docs/durable-runtime.md` is
  authoritative.
- **Definition** — `Agent`/`AgentConfig`, tools and wrappers, tool policy,
  structured output, and the typed helpers (`ChildTool`, `OutputSchema`,
  `Decode`). Anything that changes execution must be pinned by a registered
  revision or persisted with the admission.
- **Shared** — the block model and transcript ordering, the live stream event
  contract (including provisional tool progress), and provider-neutral
  boundaries, including the MCP bridge in `extensions/mcp`.

Preserve everywhere: partial `RunResult` data on failure; tool
results committed in model request order; parent-run cancellation and
deadlines fatal, while policy and tool-owned timeouts stay model-visible and
recoverable; no implicit retries of side-effecting tools or sub-agent runs;
agent configuration immutable while runs are active; provider-native content
preserved through `RawBlock`. Vendor SDK types never cross into `core`.

## 4. Implement

State the observable contract and failure behavior the change must preserve,
then make the smallest coherent patch. Add table-driven tests at the layer
where the behavior is observable, using fake providers; live model calls never
belong in unit tests. Update docs and runnable examples when exported behavior
changes.

## 5. Validate proportionally to risk

First apply the affected package's own `AGENTS.md` validation rules; they
override the generic ladder below. Then:

1. Discover affected modules from `go.mod` files and `go.work`; classify each
   with the `scripts/modules.json` inventory as published (root, `tools`,
   `host`, `extensions/*` — tagged independently; release validation rejects
   replacements and exclusions), example (`examples/*` — `replace`
   directives, never tagged), or private fixture (`internal/durabletest`,
   local-only and absent from `go.work`).
2. Run the narrowest useful checks first: targeted package tests, then race
   tests for concurrency-sensitive changes (runtime, streams, tool progress,
   tool batches, children, accumulators, hooks). The inventory's race targets
   are the root, `extensions/sqlite`, `extensions/mcp`,
   `extensions/httpserver`, and `host`;
   `scripts/check-modules.sh race <module>` runs one as CI does.
3. For cross-module changes, test the root and every affected nested module.
   For published modules also run `GOWORK=off go test ./...`. Report workspace
   and workspace-off results separately — they are different claims, and
   neither alone proves compatibility with previously tagged published
   versions. A module that uses unreleased core passes the workspace but not
   `GOWORK=off` until core is released and its requirement is bumped.
4. Build affected examples without credentials. Live provider calls are opt-in
   integration checks and are never required for ordinary validation.
5. Report commands actually run with results, failures, deliberately skipped
   checks, and residual risk. Never present proposed validation as executed.

Publication happens only through the approved **Release module** GitHub
Actions workflow, one module per dispatch (see `scripts/README.md`).
`scripts/release.sh` is its CI-only entry point, never a local check. Without
an explicit release request, do not run it, dispatch the workflow, create
tags, or push. Internal dependency versions are minimum compatible versions,
not coordinated release numbers. Publish dependencies first, finalize each
consumer's requirements and checksums in an ordinary PR with
`GOWORK=off go mod tidy`, and pass `scripts/check-modules.sh release <module>` before dispatch.
Wait for public-proxy consumer verification before releasing the next module.

The v0.6 shipping baseline is core `v0.6.0`, SQLite `v0.5.3` (including the
cancellation/reopen fix), HTTP `v0.6.0`, and host `v0.6.0`. Other modules retain
their own tags; do not assume their current source has been published. Back up
version-9 stores before core upgrades them transactionally to version 10;
rollback requires restoring the backup (see `README.md`, "Upgrading to v0.6").

## 6. Delegate only when authorized and capable

Delegated specialist review is optional and never assumed:

- Repository guidance names intended read-only specialists — `core_explorer`,
  `core_expert`, `module_integrator`, and `simplify` (Codex agent definitions
  under `.codex/agents/`). Before relying on one, confirm delegation is
  authorized for this request and that the role exists in the current harness
  capability list.
- If a named specialist is unavailable, either use an available, suitably
  constrained read-only reviewer with the same review brief, or do the
  analysis directly. Disclose when material independent review was not
  available.
- Never install or configure agent infrastructure merely to satisfy this
  skill. When several advisory agents run, one writer still owns the shared
  worktree.

## Completion report

Adapt to the mode; keep every line factual.

```
Mode: plan | review | implement
Result: <behavior delivered, answer given, or findings>
Changed: <files and packages; empty for plan/review>
Validation: <commands run with results; then proposed-but-not-run checks>
Residual risks / open questions
```

## References

- Read [references/architecture.md](references/architecture.md) when a change
  crosses package or module boundaries or modifies runtime behavior. Skip it
  for small isolated documentation or test corrections.
- [references/evaluation.md](references/evaluation.md) is the specification
  for evaluating this skill itself; it is not task guidance. Do not load it
  for ordinary repository work.
