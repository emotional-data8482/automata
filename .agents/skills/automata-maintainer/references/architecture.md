# Automata maintenance map

Lifecycles, change impact, and verification for Automata maintenance. This is a
navigation and caution map; the linked repository docs and tests are
authoritative, and this file must not grow into a stale duplicate of them.

## Lifecycles

| Lifecycle | Entry points | Authoritative sources |
| --- | --- | --- |
| Process-local | `Agent.Run`, `RunStream`, `NewSession`/`ResumeSession`, `RunTyped`/`RunSessionTyped`, `RunBackground` | `core/loop.go`, `core/session.go`, `core/typed.go`, `README.md` |
| Durable | `core.Runtime`: `NewRuntime`/`NewEphemeralRuntime`, `Register`, `Submit`, `Run`, `RunStream`, `Recover`, `Close`; `RunHandle`: `Await`, `Cancel`, `Snapshot`, `Observe`; `CommittedRunHook`s; `core.Store` and `extensions/sqlite`; recovery and tool-effect reconciliation | `docs/durable-runtime.md`, `core/runtime*.go`, `extensions/AGENTS.md`, `extensions/sqlite/README.md` |
| Streaming (both lifecycles) | `RunStream`, `RunHandle.Observe`, `core.StreamAccumulator` | `docs/streaming.md`, `core/stream.go`, `core/accumulator.go` |

Facts to keep straight:

- The process-local APIs are transitional: `README.md` states they are not
  protected legacy surfaces and will be replaced or routed through `Runtime`.
  Do not freeze them as stable contract, and do not attach durable guarantees
  (persistence, recovery, store-backed transcripts) to them before those
  equivalents land.
- `loopMachine` is the durable runtime's internal turn driver, not a second
  public lifecycle.
- Runtime storage failure never falls back to memory; an unavailable or
  invalid persistent store fails construction or admission.
- A run's admission decouples its lifetime from the caller context: after
  admission, disconnecting a view context does not cancel the logical run;
  `RunHandle.Cancel` and persisted deadlines own logical cancellation.
- `ToolResult.IsError`, a returned Go error, and external effect certainty are
  separate signals; mutating tools declare effect policy and report
  `EffectApplied`/`EffectNotApplied`/`EffectUnknown`.
- Durable work tracked in active `planning/projects/` documents is design
  context, not shipped behavior; check each document's status before citing
  it as an implemented guarantee.

## Change impact

| Change | Inspect together |
| --- | --- |
| Message or block model | `core/blocks.go`, `core/types.go`, JSON round-trip tests, every provider adapter present under `extensions/` |
| Process-local run lifecycle or errors | `core/loop.go`, `core/agent.go`, `core/session.go`, hooks, `README.md` process-local sections |
| Tool execution | `core/tools.go`, `core/toolbatch.go`, approvals, policy, effect policy, rich results, nested agents |
| Streaming | `core/stream.go`, `core/emitter.go`, accumulator, every affected provider stream implementation, `docs/streaming.md` |
| Typed output | `core/typed.go`, schema derivation/validation, provider capability mapping |
| Provider options | `core/provider.go`, request builders of the affected adapters, docs/examples |
| Durable runtime or storage | `core/runtime*.go`, `core/waits.go`, `docs/durable-runtime.md`, `extensions/sqlite`, `internal/durabletest` |
| First-party tool API | `tools/`, affected backend extension, security bounds, consumer examples |
| Module/version change | every published `go.mod`, `go.work`, examples, README release contract |

Discover the provider set instead of assuming it: enumerate `extensions/*`
adapters and read each adapter's `AGENTS.md` when a shared contract changes.
Preserve provider-native content through `RawBlock`; keep streaming and
non-streaming response semantics aligned per adapter.

## Verification ladder

Apply the affected package's own `AGENTS.md` rules first; they override this
ladder. Then discover, classify, and test.

### Module classification

| Class | Members | Properties |
| --- | --- | --- |
| Published | root (`core`), `tools`, `extensions/claude`, `extensions/openai`, `extensions/tavily`, `extensions/sqlite` | tagged independently; release automation removes any temporary local Automata `replace` before tagging |
| Example | `examples/*` | `replace` directives as dev conveniences, never tagged, never released |
| Private fixture | `internal/durabletest` | local-only test fixture, gitignored and absent from `go.work`, never published |

Discover manifests directly, then use `go.work` and `scripts/release.sh` to
classify workspace and release membership; do not infer either from memory.

### Checks, narrowest first

```sh
go test ./<affected package>       # targeted behavior
go test -race ./core               # sessions, streams, tool batches, accumulators, hooks
go test ./...                      # from each affected nested module directory
GOWORK=off go test ./...           # published modules with dependencies or interfaces changed
go build ./...                     # affected example modules, without credentials
```

### Claiming results

- A workspace (`go.work`) pass and a `GOWORK=off` pass are separate claims;
  report them separately. `GOWORK=off` still resolves against the versions
  currently pinned in each module's `go.mod` — it does not by itself prove
  compatibility with previously tagged published releases. Verifying that
  requires the module's require lines to point at real tags (e.g. after a
  release refresh), which is release work and stays out of ordinary changes.
- Run race tests for changes to sessions, streams, tool batches, accumulators,
  hooks, or other concurrent state.
- Live provider calls are opt-in integration checks; never require them for
  ordinary unit validation, and never claim provider behavior from unit tests
  alone.
- Report commands actually run with results, failures, and deliberately
  skipped checks. Proposed validation is not executed validation.
