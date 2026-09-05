# Automata repository guidance

Automata is a multi-module Go library for provider-neutral agent execution. Keep
the root module dependency-light and preserve provider-specific behavior in
`extensions/*`.

## Start here

- Read the closest nested `AGENTS.md` before editing a package.
- Use `README.md` for the public product contract and `docs/` for behavioral
  design. Check the status in each `planning/projects/` document: completed
  projects are implementation history, while proposed projects are design
  context only. Current code and tests are authoritative.
- Search for an existing test that states the invariant before changing a
  public type or execution path.
- Treat generated binaries, `.env` files, reports, and other example outputs as
  local artifacts. Do not edit or commit them unless the task names them.

## Architecture boundaries

- `core` owns provider-neutral messages, agents, sessions, tool execution,
  streaming, typed output, approvals, and hooks.
- `retry` and `tracing` are small provider-neutral support packages.
- `tools` and every directory under `extensions/` are separate Go modules.
- Vendor SDK types must not cross into `core`. Translate them at the extension
  boundary.
- Examples demonstrate public APIs. They must not become dependencies of
  library modules.

## Cross-cutting invariants

- Preserve partial `RunResult` data on failures.
- Keep committed tool results in model request order even when execution or
  stream events complete out of order.
- Treat parent run cancellation and deadlines as fatal run signals. Policy or
  tool-owned timeouts remain model-visible and recoverable unless the parent
  context is canceled.
- Do not retry side-effecting tools or sub-agent runs implicitly.
- Keep agent configuration immutable while runs are active, and protect shared
  state used by concurrent tool calls.
- Preserve provider-native content through `RawBlock` when `core` has no
  provider-neutral representation.

## Working method

- Make the smallest coherent change. Avoid unrelated public API growth or
  dependency additions.
- For work spanning packages, use read-only subagents for independent mapping
  or review when that will reduce context noise. Keep one agent responsible for
  edits to a shared worktree.
- Useful specialists are `core_explorer`, `core_expert`, `module_integrator`,
  and `simplify`. Give each a bounded question and ask for file-backed evidence.
- Update docs and runnable examples when exported behavior changes.

## Validation

- Format changed Go files with `gofmt`.
- Run the closest package tests while iterating.
- Before finishing a cross-module change, run `go test ./...` from the root and
  from every affected nested module.
- For dependency-boundary or release work, also test affected published modules
  with `GOWORK=off go test ./...`.
- Run race tests for changes to sessions, streams, tool batches, accumulators,
  hooks, or other concurrent state.

Never run `scripts/release.sh`, create tags, push, or publish unless the user
explicitly requests a release.
