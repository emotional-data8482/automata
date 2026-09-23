# Automata maintenance map

Lifecycles, change impact, and verification for Automata maintenance. This is a
navigation and caution map; the linked repository docs and tests are
authoritative, and this file must not grow into a stale duplicate of them.

## Lifecycles

| Lifecycle | Entry points | Authoritative sources |
| --- | --- | --- |
| Runtime (the only lifecycle) | `core.Runtime`: `NewRuntime`/`NewEphemeralRuntime`, `Register` (returns `DefinitionRef`), `Submit`/`Run`/`RunStream` with `WithIdempotencyKey`/`WithDeadline`/`WithConversation`, `Recover`, `Prune`, `Conversation`, `Close`; `RunHandle`: `Await`, `Cancel`, `Snapshot`, `Observe`, `Events`/`WaitEvents`, `ResolveWait`, `Reconcile`, `AcknowledgeHooks`; `CommittedRunHook`s; `core.Store` and `extensions/sqlite` | `docs/durable-runtime.md`, `core/runtime*.go`, `core/example_test.go`, `extensions/AGENTS.md`, `extensions/sqlite/README.md` |
| Definitions and typed helpers | `core.New`/`AgentConfig`, `Func`/`FuncResult`, `WithToolEffectPolicy`, `WithDurableWait`, `ChildTool`/`NewChildTool`, `OutputSchema`, `Decode` | `core/agent.go`, `core/tools.go`, `core/children.go`, `core/typed.go`, `README.md` |
| Live streaming | `Runtime.RunStream`, `RunHandle.Observe`, child-event forwarding, `core.StreamAccumulator` | `core/stream.go`, `core/runtime.go` (`publish`), `core/accumulator.go`, `docs/durable-runtime.md#observation` |

Facts to keep straight:

- There is no process-local execution path. Tests run agents through an
  ephemeral Runtime (`runAgent` in `core/testfixture_test.go`), and a tool
  that runs another agent itself is an opaque host tool, outside the parent's
  durable guarantees; composition is `ChildTool`.
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
| Run loop or errors | `core/loop.go`, `core/loopmachine.go`, `core/runtime.go` (failure persistence and `snapshotError`), `README.md` |
| Tool execution | `core/tools.go`, `core/runtime_tools.go`, waits, policy, effect policy, rich results, child tools |
| Streaming | `core/stream.go`, `core/runtime.go` (`publish`, `attachAncestors`), accumulator, every affected provider stream implementation |
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
go test -race ./core               # runtime, streams, tool batches, children, accumulators, hooks
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
- Run race tests for changes to the runtime, streams, tool batches, children,
  accumulators, hooks, or other concurrent state.
- Live provider calls are opt-in integration checks; never require them for
  ordinary unit validation, and never claim provider behavior from unit tests
  alone.
- Report commands actually run with results, failures, and deliberately
  skipped checks. Proposed validation is not executed validation.
