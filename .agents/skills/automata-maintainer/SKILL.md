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
  package (`core/`, `tools/`, `extensions/`, `extensions/<provider>/`,
  `examples/`, `retry/`, `tracing/`). Nested guidance narrows root guidance;
  apply the most specific rules first.
- Ground claims in current code, tests, `README.md`, and `docs/`. Treat
  `planning/projects/` status as history when marked completed and as design
  context only when proposed — never as implemented behavior.

## 3. Identify the lifecycle before applying invariants

State which lifecycle the change touches, because their invariants differ:

- **Process-local** — `Agent.Run`/`RunStream`, `Session`, `RunTyped`,
  `RunBackground`. `README.md` marks these as transitional toward `Runtime`;
  do not freeze their APIs as stable contract or attach Runtime guarantees to
  them prematurely.
- **Durable** — `core.Runtime` (`Register`, `Submit`, `Run`, `RunStream`,
  `Recover`, `Close`), `RunHandle` (`Await`, `Cancel`, `Snapshot`, `Observe`),
  committed-run hooks, the `core.Store` contract and its adapters, recovery,
  and tool-effect reconciliation. `docs/durable-runtime.md` is authoritative.
- **Shared** — the block model and transcript ordering, the streaming event
  contract (`docs/streaming.md`), and provider-neutral boundaries.

Preserve regardless of lifecycle: partial `RunResult` data on failure; tool
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
   as published (`core`, `tools`, `extensions/*` — tagged independently;
   release automation removes any temporary local Automata `replace`), example
   (`examples/*` — `replace` directives, never tagged), or private fixture
   (`internal/durabletest`, local-only and absent from `go.work`).
2. Run the narrowest useful checks first: targeted package tests, then
   `go test -race ./core` for concurrency-sensitive changes (sessions,
   streams, tool batches, accumulators, hooks).
3. For cross-module changes, test the root and every affected nested module.
   For published modules also run `GOWORK=off go test ./...`. Report workspace
   and workspace-off results separately — they are different claims, and
   neither alone proves compatibility with previously tagged published
   versions.
4. Build affected examples without credentials. Live provider calls are opt-in
   integration checks and are never required for ordinary validation.
5. Report commands actually run with results, failures, deliberately skipped
   checks, and residual risk. Never present proposed validation as executed.

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
