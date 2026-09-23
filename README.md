# automata

Durable, provider-neutral agent execution in Go: an agent's run is admitted
before it starts, every step is committed as it happens, and a restarted
process continues the same run without repeating work that already happened.
It is built for agents inside web servers and job workers, not notebooks.

- **One lifecycle.** An `Agent` is an immutable definition. A `Runtime` runs
  it, either on a persistent store (`extensions/sqlite`) or explicitly in
  memory. Sync calls, live streams, typed output, child agents,
  conversations, and approvals are all views of that one durable run.
- **Honest about side effects.** A tool call that was dispatched but never
  reported an outcome is surfaced as *uncertain*, and a provider request
  whose response was lost needs attention. Neither is replayed silently.
  You reconcile from the destination, and the run continues.
- **A block-based message model that never degrades a provider.** A `Message`
  is typed `Block`s: text, thinking with its signature, tool_use,
  tool_result with an error flag, and images. `RawBlock` carries
  provider-native content that must round-trip.
- **Observable.** Live streams deliver provisional deltas for UIs. Committed
  events are durable facts that an observer reads from a cursor and resumes
  after a disconnect or restart. Runs emit `slog` logs and `tracing` spans.
- **Dependency-free core.** The root module has no requirements. Vendor
  SDKs and database drivers live in separate `extensions/*` modules.

## Layout

| Module / package | What it is |
| --- | --- |
| `core` | `Agent` definitions, `Runtime`, tools, child agents, conversations, approvals, streaming, and the storage port |
| `core/storetest` | Conformance suite for `core.Store` adapters |
| `tools` (module) | First-party tools: `HTTPFetch`, `ReadFile`/`WriteFile` (sandboxed), `Shell` (allow-listed), `WebSearch` |
| `extensions/sqlite` (module) | The supported persistent store: one exclusive local owner, WAL, `synchronous=FULL` |
| `extensions/claude` (module) | Anthropic provider; thinking, images, prompt caching, native structured output |
| `extensions/openai` (module) | OpenAI Chat Completions provider (stdlib-only); any OpenAI-compatible base URL |
| `extensions/tavily` (module) | Tavily backend for `tools.WebSearch` |
| `retry`, `tracing` | Backoff policy and span interfaces used by core |
| `examples/*` (modules) | Runnable demos (see [Examples](#examples)) |

Extensions and examples are separate Go modules tied together by `go.work`,
so importing `core` never pulls a vendor SDK into your build.

### Releases

The root module, `tools`, and every extension are tagged independently
(Go multi-module tagging: `v0.5.0`, `tools/v0.5.0`, `extensions/openai/v0.5.0`,
…). One command does the whole dance:

```sh
scripts/release.sh minor --push # or: an explicit version / patch / major
```

It bumps every submodule's `automata` require line, builds and tests the full
workspace, commits, tags root + all published modules, and pushes. Because
`go mod tidy` in a submodule can only resolve the new core version after its
tag is on the remote, the script then refreshes the submodules' `go.sum` files
in a small follow-up commit and verifies each module still builds against the
published pins (`GOWORK=off`). Run it without `--push` to stop after tagging
for review. Published modules must require real tagged versions and carry
no `replace` directives when tagged: a dependency's `replace` is ignored
downstream. In-repo development uses `go.work`; a published module may
carry a temporary local core replacement while depending on unreleased core,
but the release script drops it before tagging. The script also bumps Tavily's
`tools` dependency to the new tag. `examples/*` keep `replace` directives
as dev conveniences and are never tagged.

## Quickstart

Define an agent, register it with a runtime, and run a task. `provider` is
any `core.Provider`, for example `claude.New(model, apiKey)` from
`extensions/claude`.

```go
// ExampleRuntime in core/example_test.go
runtime, err := core.NewEphemeralRuntime()
if err != nil {
	panic(err)
}
defer runtime.Close()

agent, err := core.New(provider, core.AgentConfig{
	SystemPrompt: "You are a concise assistant.",
	MaxTurns:     5,
	Tools: []core.Tool{core.Func("weather", "Get the weather for a city",
		func(ctx context.Context, in WeatherArgs) (string, error) {
			return "sunny in " + in.City, nil
		})},
})
if err != nil {
	panic(err)
}
assistant, err := runtime.Register("assistant", "v1", agent)
if err != nil {
	panic(err)
}

ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()
result, err := runtime.Run(ctx, assistant, "What's the weather in Paris?")
if err != nil {
	// result still holds the transcript, usage, and turns so far.
	panic(fmt.Sprintf("failed after %d turns: %v", result.Turns, err))
}
fmt.Println(result.Output)
fmt.Println(result.Turns, "turns")
```

`Run` returns a `RunResult` with the final `Output` text, the `FinalMessage`
(blocks included), the run's `Messages` transcript, summed `Usage`, `Turns`,
and a `StopReason`. It is populated as far as the run got even when `err` is
non-nil. `NewEphemeralRuntime` keeps runs in memory, which suits tests,
scripts, and short-lived work. Use a persistent store when a run must survive
the process.

## Durable execution

```go
// ExampleNewRuntime in core/example_test.go
// In production: store, err := sqlite.Open(ctx, "automata.db")
store := core.NewMemoryStore()
runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
if err != nil {
	panic(err)
}
defer runtime.Close()

// Register every definition stored runs may pin, then adopt their work.
summarizer, err := runtime.Register("summarizer", "2026-09-23", agent)
if err != nil {
	panic(err)
}
if err := runtime.Recover(ctx); err != nil {
	panic(err)
}

// The external task ID makes admission idempotent. An exact retry repeats
// the same task and options, deadline included.
admission := []core.SubmitOption{
	core.WithIdempotencyKey("tickets", "42"),
	core.WithDeadline(time.Now().Add(10 * time.Minute)),
}
handle, err := runtime.Submit(ctx, summarizer, "Summarize ticket 42", admission...)
if err != nil {
	panic(err)
}
result, err := handle.Await(ctx)
if err != nil {
	panic(err)
}
fmt.Println(result.Output)

retry, err := runtime.Submit(ctx, summarizer, "Summarize ticket 42", admission...)
if err != nil {
	panic(err)
}
fmt.Println(retry.ID() == handle.ID())
```

- **Three lifetimes.** The context passed to `Submit`, `Run`, `Await`, or a
  stream bounds only that call. Once admission commits, disconnecting does
  not cancel the run. `Runtime.Close` stops workers at a safe boundary.
  `RunHandle.Cancel` and a `WithDeadline` deadline are the only logical
  cancellations, and both reach every child run.
- **Identity.** `WithIdempotencyKey(scope, key)` maps your task or workflow
  ID to exactly one run. An exact retry, even after a lost response or a
  restart, returns the original run. A changed task, definition, deadline, or
  conversation returns `ErrAdmissionConflict`.
- **Restart.** A new process opens the same store, registers the same
  definition revisions, and calls `Recover`. Admitted work starts, accepted
  turns and completed tool calls are not repeated, suspended runs keep
  waiting, and anything uncertain needs attention.
- **Definitions are pinned.** A run pins the definition ID and revision it
  was admitted with. Register a new revision when an agent's behavior
  changes; registering a different `Agent` under an existing revision is
  rejected.

[docs/durable-runtime.md](docs/durable-runtime.md) is the full lifecycle
reference: recovery, provider attempts, tool effects and reconciliation,
waits, children, conversations, observation, retention, operating bounds,
and the operations runbook.

## Tools

`core.Func` derives a tool's JSON schema from a Go struct. Exported fields
are properties, fields without `omitempty` are required, and a `desc` tag
describes a field. `core.FuncResult` returns a `core.ToolResult` for rich
content (text and image blocks):

```go
// Fragment
screenshot := core.FuncResult("screenshot", "Capture an image",
	func(ctx context.Context, in ShotArgs) (core.ToolResult, error) {
		png, err := capture(ctx, in.Target)
		if err != nil {
			return core.ErrorResult("capture failed: " + err.Error()), nil // model-visible
		}
		return core.BlockResult(
			core.TextBlock{Text: "captured " + in.Target},
			core.ImageBlock{MediaType: "image/png", Data: png},
		), nil
	})
```

An `ErrorResult`, an unknown tool name, and invalid arguments are
recoverable, because the model sees them. A Go error returned by a tool is
fatal to the run, and so is logical cancellation. Only the error's message
survives persistence, and awaited errors are rebuilt from it: `errors.Is`
still matches the runtime's sentinels, but not a tool's own error types.

Declare what a tool does to the outside world so recovery can be honest
about it:

```go
// Fragment
publish = core.WithToolEffectPolicy(publish, core.ToolEffectPolicy{
	Kind:  core.ToolEffectMutating, // every return must report EffectApplied, EffectNotApplied, or EffectUnknown
	Scope: "reports",
	SemanticKey: func(raw json.RawMessage) (string, error) { // rejects a second write of the same report
		var in PublishInput
		err := json.Unmarshal(raw, &in)
		return in.Path, err
	},
})
```

Inside a tool, `core.ToolOperationFromContext(ctx)` returns a stable
operation ID to send as the destination's idempotency key. A crash between
dispatch and the committed outcome leaves the call `ToolInvocationUncertain`.
`RunHandle.Reconcile` records the authoritative outcome without running the
tool again.

`ToolPolicy` enforces deadlines, call budgets, rate limits, and bounded
parallelism outside the prompt:

```go
// Fragment
core.AgentConfig{ToolPolicy: core.ToolPolicy{
	Timeout:     10 * time.Second,
	MaxCalls:    50, // shared by the whole run tree, persisted across restarts
	MaxParallel: 4,
	PerTool: map[string]core.ToolLimits{
		"http_fetch": {Timeout: 3 * time.Second, MaxCalls: 10, RateLimiter: limiter},
	},
}}
```

Policy timeouts and budget denials are recoverable tool results. Tools and
limiters must honor context cancellation, because Go cannot stop a function
that ignores its context.

## Typed output

A definition can require validated structured output. `core.OutputSchema[T]`
derives the schema, and `core.Decode[T]` validates and decodes the accepted
payload:

```go
// ExampleDecode in core/example_test.go
agent, err := core.New(provider, core.AgentConfig{
	StructuredOutput: &core.StructuredOutputConfig{
		Schema:         core.OutputSchema[Person](),
		MaxCorrections: 1,
	},
})
if err != nil {
	panic(err)
}
biographer, err := runtime.Register("biographer", "v1", agent)
if err != nil {
	panic(err)
}
result, err := runtime.Run(ctx, biographer, "Who wrote the first program?")
if err != nil {
	panic(err)
}
person, err := core.Decode[Person](result)
if err != nil {
	panic(err)
}
fmt.Printf("%s, %d (%d turns)\n", person.Name, person.Age, result.Turns)
```

Invalid output is corrected inside the same run, within its turn budget:
the violations go back to the model, and tool calls that already happened
are not repeated. The accepted payload is `RunResult.StructuredOutput`,
separate from the model-facing `Output` text. When correction is exhausted,
the run fails with an error that matches `core.ErrInvalidStructuredOutput`;
`errors.As` extracts the per-field violations from
`*core.InvalidStructuredOutputError`. Set `StructuredOutputConfig.Native` to
use provider-native schema enforcement when the provider supports it (Claude
`output_config`, OpenAI `response_format`). Other providers fall back to a
hidden `automata_structured_output` tool. Schemas share one supported subset.
Unsupported assertion keywords such as `$ref`, `oneOf`, and `const` are
rejected at `core.New`.

## Child agents

`core.ChildTool[P]` delegates to another registered definition. Every call
becomes a child run of its own, linked to the parent's call:

```go
// ExampleChildTool in core/example_test.go
researcher, err := runtime.Register("researcher", "v1", must(core.New(researcherProvider, core.AgentConfig{
	SystemPrompt: `You receive {"topic": ...}. Research it and reply with notes.`,
})))
if err != nil {
	panic(err)
}
lead, err := runtime.Register("lead", "v1", must(core.New(leadProvider, core.AgentConfig{
	Tools: []core.Tool{
		core.ChildTool[ResearchRequest]("research", "Research one topic.", researcher),
	},
	// Call caps are shared by the whole run tree.
	ToolPolicy: core.ToolPolicy{MaxCalls: 10},
})))
if err != nil {
	panic(err)
}

var views core.StreamAccumulator
result, err := runtime.RunStream(ctx, lead, "Write a report on tides.", views.Add)
if err != nil {
	panic(err)
}
fmt.Println(result.Output)
for _, view := range views.Views() {
	fmt.Printf("%q (call %q): %s\n", view.Agent, view.InvocationID, view.Text)
}
```

The parent's worker is released while children run. Children share the
parent's call caps, deadline, and cancellation, are never retried
implicitly, and keep their own transcripts, effects, and receipts. The
child's task is the model's validated arguments as JSON, and the parent
receives the child's structured output or final message. Read a child's
typed output with `core.Decode` on the child run's snapshot
(`ToolInvocationSnapshot.ChildRunID` links down).

## Conversations

```go
// ExampleWithConversation in core/example_test.go
writer, err := runtime.Register("writer", "v1", must(core.New(provider, core.AgentConfig{})))
if err != nil {
	panic(err)
}
thread := core.ConversationRef{Scope: "tenant-1", ID: "refund-policy"}
first, err := runtime.Run(ctx, writer, "Draft a refund policy.", core.WithConversation(thread, ""))
if err != nil {
	panic(err)
}
// The next turn names the head it continues; a stale or concurrent turn
// is rejected rather than merged.
second, err := runtime.Run(ctx, writer, "Make it 30 days.", core.WithConversation(thread, first.RunID))
if err != nil {
	panic(err)
}
fmt.Println(second.Output)
fmt.Println(len(second.Messages), "messages in the conversation")
```

A conversation has at most one active turn: a competing turn returns
`ErrConversationBusy`. Each turn references the committed history of the
previous one instead of copying it.

## Approvals and questions

`core.WithDurableWait` suspends a run on a tool call until the host answers.
The run holds no worker while it waits, and the wait survives restarts:

```go
// ExampleWithDurableWait in core/example_test.go
refund := core.WithDurableWait(
	core.Func("refund", "Refund an order.", func(ctx context.Context, in RefundInput) (string, error) {
		return fmt.Sprintf("refunded %d to %s", in.Amount, in.Order), nil
	}),
	core.DurableWaitPolicy{
		Kind:   core.WaitApproval,
		Target: func(raw json.RawMessage) (string, error) { return string(raw), nil },
	})
runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
	Store: core.NewMemoryStore(),
	// Consulted when an approval is accepted and again before dispatch.
	Authorizer: core.ApprovalAuthorizerFunc(func(ctx context.Context, check core.ApprovalAuthorization) error {
		if check.Actor != "support-lead" {
			return errors.New("not allowed")
		}
		return nil
	}),
})
if err != nil {
	panic(err)
}
defer runtime.Close()
support, err := runtime.Register("support", "v1", must(core.New(provider, core.AgentConfig{Tools: []core.Tool{refund}})))
if err != nil {
	panic(err)
}

handle, err := runtime.Submit(ctx, support, "Refund order A-7.")
if err != nil {
	panic(err)
}
wait := pendingWait(ctx, handle)
fmt.Println("approve?", wait.Tool, wait.Target)
err = handle.ResolveWait(ctx, wait.ID, core.WaitResolution{
	Decision:     core.Allow,
	Actor:        "support-lead",
	ActionDigest: wait.ActionDigest, // binds the approval to this exact call
})
if err != nil {
	panic(err)
}
result, err := handle.Await(ctx)
if err != nil {
	panic(err)
}
fmt.Println(result.Output)
```

An approval binds the exact call, arguments, target, definition revision,
and policy context into its `ActionDigest`. The configured `Authorizer` is
consulted when the approval is accepted and again immediately before
dispatch. Repeating the same resolution returns the stored outcome, while a
different one returns `ErrWaitConflict`. A `WaitQuestion` wait makes the
host's JSON answer the tool result.

## Observing runs

`Runtime.RunStream` and `RunHandle.Observe` deliver provisional live events:
text and thinking deltas, tool calls, tool results, and usage. Child-run
events are included, tagged with `StreamEvent.Agent` (the child tool's name)
and `InvocationID` (the call that started it). `core.StreamAccumulator` folds
them into per-agent views for rendering. Live views are bounded and drop
events rather than slow the run.

Committed events are the durable record. Read them in pages from a cursor:

```go
// ExampleRunHandle_Events in core/example_test.go
var cursor uint64 // persist this to resume after a restart
for {
	page, err := handle.WaitEvents(ctx, cursor, 256)
	if errors.Is(err, core.ErrEventGap) {
		// Retention removed events behind the cursor: resynchronize.
		snapshot, err := handle.Snapshot(ctx)
		if err != nil {
			panic(err)
		}
		cursor = snapshot.EventSequence
		continue
	}
	if err != nil {
		panic(err)
	}
	for _, event := range page.Events {
		if event.Kind == core.CommittedRunState {
			fmt.Println("state:", event.State)
		}
	}
	cursor = page.Next
	if last := page.Events[len(page.Events)-1]; last.Kind == core.CommittedRunState && last.State == core.RuntimeTerminal {
		break
	}
}
```

`RunHandle.Snapshot` is the authoritative full view of a run, with its
transcript, tool batches and effects, waits, attention, failure, and
accounting across child runs.

## Long conversations

`core.Compactor` is a pre-send hook that summarizes older turns to stay
within a token budget. It keeps the system prompt and recent turns intact,
never separates a tool call from its result, and only changes what is sent,
never the committed transcript. The Claude provider's
`WithConversationCache()` caches the message prefix.

## Examples

Examples that call a model need credentials; the others run offline.

- `examples/durable_host` — offline. A small platform host where each command
  runs in its own process. It stops a run at an approval, approves it from
  a later process, crashes one process right after an external write,
  reconciles from the destination, and resumes committed events from a
  saved cursor. It verifies exactly one write per ticket:

  ```sh
  go run ./examples/durable_host demo
  ```

- `examples/durable_typed` — offline, on a temporary SQLite store. It
  corrects invalid structured output without repeating an accepted write,
  then delegates to a typed child inside a two-turn conversation:

  ```sh
  go run ./examples/durable_typed
  ```

- `examples/claude`, `examples/openai` — a minimal streaming tool-using
  agent (`ANTHROPIC_API_KEY`, or `OPENAI_API_KEY` for any OpenAI-compatible
  endpoint).
- `examples/deep_research` — an orchestrator that delegates to researcher
  and writer child runs, rendered live in a Bubble Tea TUI from a
  `StreamAccumulator`. Needs `ANTHROPIC_API_KEY` and `TAVILY_API_KEY`:

  ```sh
  go run ./examples/deep_research "the impact of GLP-1 drugs on US healthcare costs"
  ```

## Upgrading from v0.4

This release makes `Runtime` the only way to run an agent. The direct entry
points, which were process-local and lost on restart, are removed:

| v0.4 | Now |
| --- | --- |
| `agent.Run(ctx, task, opts...)` | `ref, _ := rt.Register(id, rev, agent)`, then `rt.Run(ctx, ref, task)` |
| `agent.RunStream(ctx, task, onEvent)` | `rt.RunStream(ctx, ref, task, onEvent)` |
| `agent.RunBackground(...)` | `rt.Submit(...)`, then `handle.Await(ctx)` |
| `agent.NewSession()`, `ResumeSession`, `Session.Run` | `rt.Run(ctx, ref, task, core.WithConversation(thread, head))` |
| `core.RunTyped[T]`, `RunSessionTyped[T]` | `AgentConfig.StructuredOutput{Schema: core.OutputSchema[T]()}`, then `core.Decode[T](result)` |
| `core.AsTool[P]`, `AsToolFunc[P]` | `core.ChildTool[P](name, description, childRef)` (the child receives JSON) |
| `core.DurableChildTool(def, DurableChildPolicy{...})` | `core.NewChildTool(def, ref)` |
| `WithCallOptions`, `WithMaxTurns`, `WithToolPolicy`, `WithTools`, `WithMaxCorrectionTurns`, `WithNativeStructuredOutput` | Set them on `AgentConfig` and register a new revision. |
| `AgentConfig.DefaultCallOptions` | `AgentConfig.CallOptions` |
| `AgentConfig.Approver`, `ApproverFunc`, `AllowAll` | `core.WithDurableWait` approvals with `RuntimeConfig.Authorizer` |
| `AgentConfig.Observers`, `WithObserver`, `RunEvent`, `Checkpoint` | Live views (`RunStream`, `Observe`) and committed events (`RunHandle.Events`) |
| `SubmitOptions{Scope, Key, Deadline, Conversation}` | `core.WithIdempotencyKey`, `core.WithDeadline`, `core.WithConversation` |
| `Register(...) error` | `Register(...) (core.DefinitionRef, error)` |
| `Conversation(ctx, scope, id)` | `Conversation(ctx, core.ConversationRef{...})` |
| `RunResult.Steps`, `RawProviderStopReason` | `RunResult.Turns`, `RawStopReason` |
| `ErrMaxStepsExceeded`, `ErrInvalidMaxSteps`, `StopMaxSteps`, `StopNormal` | `ErrMaxTurnsExceeded`, `ErrInvalidMaxTurns`, `StopMaxTurns`, `StopEndTurn` |
| `WaitResolution{Decision: core.Modify}` | Not supported: a changed action needs a new model call and approval. |

Behavior changes to expect:

- **Streaming.** Runs stream from providers that support it, even through
  `Run`.
- **Errors after `Await`.** They are rebuilt from the persisted failure.
  Sentinel errors, `*CompletionError`, and `*InvalidStructuredOutputError`
  still match with `errors.Is` or `errors.As`. A tool's or provider's own
  error types keep only their message.
- **Nesting.** A tool that runs another agent inside itself is not part of
  the parent's durable run. Use `ChildTool` for composition.
- **Storage.** v0.5.0 is the first released Runtime version, using encoding 9.
  Earlier pre-release stores are rejected, not migrated. See
  [storage upgrades](docs/durable-runtime.md#storage-upgrades-and-backups).

## Agent skill

This repository includes an [Agent Skills](https://agentskills.io) guide for
building Automata applications at
[`.codex/skills/automata-go/`](.codex/skills/automata-go/SKILL.md). Agents that
discover project skills can load it directly. To use it globally in other
projects, copy that directory to `~/.agents/skills/automata-go/`.
