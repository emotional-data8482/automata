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
| `agents` | Typed shorthand over `core`: `Define[I, O]` builds and registers a definition, then runs it or exposes it as a child tool |
| `core/storetest` | Conformance suite for `core.Store` adapters |
| `core/coretest` | Test kit: a scripted provider (`NewProvider`, `NewStreamProvider`) that replays turns and records requests, plus `Runtime` fixtures |
| `tools` (module) | First-party tools: `HTTPFetch`, `ReadFile`/`WriteFile` (rooted), `Shell` (allow-listed), with pluggable `FileSystem` and `Executor` backends for sandboxes, `WebSearch`; `LoadAgentsMD` and `LoadSkills` for AGENTS.md instructions and Agent Skills |
| `extensions/sqlite` (module) | The supported persistent store: one exclusive local owner, WAL, `synchronous=FULL` |
| `extensions/claude` (module) | Anthropic provider; thinking, images, prompt caching, native structured output |
| `extensions/openai` (module) | OpenAI Chat Completions provider (stdlib-only); any OpenAI-compatible base URL |
| `extensions/openrouter` (module) | OpenRouter Chat Completions provider (official SDK); text, reasoning, images, tools, and structured output |
| `extensions/tavily` (module) | Tavily backend for `tools.WebSearch` |
| `extensions/mcp` (module) | Model Context Protocol through the official Go SDK: `mcpclient` turns a server's tools into `core.Tool`s, `mcpserver` serves `core.Tool`s |
| `extensions/httpserver` (module) | `net/http` and SSE API over a `Runtime`: admission, listing, status, resumable event and change streams, cancellation, wait answers, manifests |
| `host` (module) | `host.Main`: opens the store, registers definitions, recovers, applies retention, and serves the HTTP API |
| `retry`, `tracing` | Backoff policy and span interfaces used by core |
| `examples/*` (modules) | Runnable demos (see [Examples](#examples)) |

Extensions and examples are separate Go modules tied together by `go.work`,
so importing `core` never pulls a vendor SDK into your build.

### Releases

The root module, `tools`, and every extension release independently, with tags
such as `v0.5.3`, `tools/v0.5.3`, and `extensions/openai/v0.5.3`. Internal
requirements retain their minimum compatible versions: installing the latest
adapter does not necessarily install the latest core.

Use the **Release module** GitHub Actions workflow to select one module, an exact
version, and optionally a commit on `main`. It validates the exact commit against
both the workspace and published dependencies, waits for approval in the protected
`release` environment, then creates only that module's immutable tag and GitHub
Release. A fresh external consumer verifies installation after publication.
There are no automatic dependency bumps, release commits, or checksum follow-ups.

Dependency changes and finalized `go.mod`/`go.sum` files belong in ordinary PRs.
Release dependencies first when an adapter needs a new core or tools API.
Published modules must contain no replacements or exclusions; the release gate
rejects them rather than editing them. `examples/*` retain development
replacements and are never tagged.

See [release setup, validation, and recovery](scripts/README.md). Local publication
through the old `scripts/release.sh ... --push` interface is disabled.

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
- **Labels and listing.** `WithLabels` attaches bounded host annotations
  (schedule, actor, request IDs) that are not part of admission identity and
  are inherited by child runs. `Runtime.Runs` pages through runs in admission
  order, filtered by definition, idempotency scope, state, attention kind,
  labels, and admission time.
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
are properties, fields without `omitempty` are required, a `desc` tag
describes a field, and an `enum:"approve,reject"` tag limits its values
(each element's, for a slice). `core.FuncResult` returns a `core.ToolResult`
for rich content (text and image blocks); `core.JSONResult(v)` returns `v`
encoded as JSON text:

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

### Sandboxed file and shell tools

`tools.ReadFile(root)`, `tools.WriteFile(root)` and `tools.Shell(cfg)` act on
the host by default. To run the same tools inside a container, a VM or a
remote workspace, implement `tools.FileSystem` and `tools.Executor` for the
sandbox and pass them in:

```go
// Fragment
fsys := sandboxFS{}       // implements tools.FileSystem
executor := sandboxExec{}    // implements tools.Executor
agentTools := []core.Tool{
	tools.ReadFileFS(fsys),
	tools.WriteFileFS(fsys),
	tools.Shell(tools.ShellConfig{Allow: []string{"go", "git"}, Executor: executor}),
}
```

The tools keep their validation, limits and effect reports whatever the
backend: paths are checked before they reach the filesystem, the allow-list
is checked before a command reaches the executor, output and reads stay
bounded, and the shell timeout stays recoverable. Both interfaces receive
the tool call's context, so one durable definition can resolve each run's
sandbox from `core.ToolOperationFromContext`. A `WriteFile` error that wraps
`tools.ErrNotWritten` reports `EffectNotApplied`; any other error reports
`EffectUnknown`. A filesystem with an `EffectScope() string` method sets the
write tool's effect scope.

### MCP servers

`extensions/mcp` wraps the official MCP Go SDK. `mcpclient` connects over any
SDK transport and returns the server's tools as ordinary `core.Tool`s:

```go
// Fragment
client, err := mcpclient.Connect(ctx,
	&mcp.CommandTransport{Command: exec.Command("mcp-server-fetch")},
	mcpclient.Options{})
defer client.Close()
remote, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "fetch_"})
agent, err := core.New(provider, core.AgentConfig{Tools: remote})
```

A result the server marks `isError`, or a JSON-RPC error it returns, is a
recoverable tool result. A broken connection is fatal to the run. Progress
notifications from the server arrive as `StreamToolProgress` events. Each input
schema is rewritten into the subset core validates. Assertions core cannot
express are dropped, and the server still enforces them. Remote tools are
unclassified and need no approval by default. Wrap them with
`WithToolEffectPolicy` or `WithDurableWait` to change that.

`mcpserver.AddTools(server, tools...)` goes the other way and registers core
tools on an SDK `*mcp.Server`. It validates arguments against each schema
before `Execute`, and sends the tool's progress reports to clients that
request progress. Runtime policies (timeouts, budgets, approvals) do not
apply to those calls.

### AGENTS.md instructions

`tools.LoadAgentsMD` collects the [AGENTS.md](https://agents.md) files on the
path from a workspace root down to a working directory. The files are read
through `os.Root`, ordered shallowest first so the closest instructions come
last, and capped at 32 KiB combined (an error, not a truncation). Append the
result to the system prompt:

```go
// Fragment
instructions, err := tools.LoadAgentsMD(repoRoot, tools.AgentsMDOptions{Dir: "core"})
if err != nil {
	return err
}
agent, err := core.New(provider, core.AgentConfig{
	SystemPrompt: basePrompt + "\n\n" + instructions,
	Tools:        []core.Tool{tools.ReadFile(repoRoot)},
})
```

The instructions are frozen with the agent, so a registered revision pins the
text it ran with. Load again and register a new revision to pick up edits.
Existing conversations keep the system message they started with.

### Agent Skills

`tools.LoadSkills` loads [Agent Skills](https://agentskills.io): each
subdirectory with a `SKILL.md` under the given directories. Its `Catalog`
lists each skill's name and description for the system prompt, and its
read-only `load_skill` tool returns a skill's instructions, or a file the skill
references, when the model asks for one:

```go
// Fragment
skills, err := tools.LoadSkills(".agents/skills", filepath.Join(home, ".agents/skills"))
if err != nil {
	return err
}
agent, err := core.New(provider, core.AgentConfig{
	SystemPrompt: basePrompt + "\n\n" + skills.Catalog(),
	Tools:        []core.Tool{skills.Tool()},
})
```

Loading validates each skill's `name` (it must match the directory) and
`description`. A name defined twice is an error. `SKILL.md` bodies are read
once and frozen with the agent. Referenced files are read on demand, and only
inside the skill's directory. Scripts a skill ships are never run. Running one
needs a tool such as `tools.Shell`. `allowed-tools` is parsed onto `Skill` but
not enforced; bound tools with `ToolPolicy` and approvals.

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
hidden `automata_structured_output` tool (`core.StructuredOutputToolName`). Schemas share one supported subset.
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
typed output from its run: `handle.Child(ctx, callID)` returns the child
run's handle for a tool call, and `core.Decode` reads its awaited result.
`RunSnapshot.ToolCall(callID)` returns any call's invocation, and
`RunSnapshot.AppliedEffects(tool)` returns the receipts a tool reported.

### Typed child agents

The `agents` package states a definition's input and output types once.
`agents.Define[I, O]` derives the output contract from `O` (or none when `O`
is `string`), builds the agent, and registers it. The returned value exposes
the definition as a child tool with an input schema from `I`, runs it directly,
and decodes its results:

```go
// Fragment
reviewer, err := agents.Define[ReviewRequest, ReviewVerdict](runtime, agents.Spec{
	ID: "reviewer", Revision: "v1", Provider: provider,
	Config: core.AgentConfig{SystemPrompt: "Review the named artifact."},
})
lead, err := agents.Register(runtime, agents.Spec{
	ID: "lead", Revision: "v1", Provider: provider,
	Config: core.AgentConfig{Tools: []core.Tool{
		reviewer.Tool("review", "Ask the reviewer for a verdict on one artifact."),
	}},
})

// Direct run: the task is the input as JSON, exactly as a child call sends it.
verdict, result, err := reviewer.Run(ctx, runtime, ReviewRequest{Artifact: "report.md"})
// A child run's result from its snapshot decodes the same way.
verdict, err = reviewer.Output(childSnapshot.Result)
```

A derived contract allows `agents.DefaultMaxCorrections` correction turns;
set `Config.StructuredOutput` without a `Schema` to choose `Native` or another
budget.

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

## Scheduled runs

Automata has no built-in scheduler. A host supplies the timer and uses
idempotent admission to deduplicate each occurrence (a slot), such as the
nightly report for 2026-10-11. Use the slot name as the run's idempotency key.

For restart deduplication, use a persistent runtime and reopen the same
store; the ephemeral runtime in the compiled example is only for testing.
SQLite supports one exclusive owner process, not competing runtime owners.
Multiple scheduler processes should submit through that owner's HTTP API,
using the same idempotency scope and key. Independent stores do not deduplicate
one another. Register stored definition revisions and call `Recover` before
catching up; admission deduplication does not guarantee a run completed.

```go
// ExampleWithIdempotencyKey_schedule in core/example_test.go
report, err := runtime.Register("nightly-report", "v1", must(core.New(provider, core.AgentConfig{})))
if err != nil {
	panic(err)
}
// submitSlot admits the nightly report for one UTC day. Everything it
// submits derives from the slot, so a retry is exact, not a conflict.
submitSlot := func(day time.Time) (*core.RunHandle, error) {
	slot := day.UTC().Format("2006-01-02")
	return runtime.Submit(ctx, report, "Send the report for "+slot+".",
		core.WithIdempotencyKey("schedule/nightly-report", slot),
		core.WithLabels(map[string]string{"schedule": "nightly-report", "slot": slot}))
}
// A tick that fires twice admits the slot once.
first, err := submitSlot(today)
if err != nil {
	panic(err)
}
again, err := submitSlot(today)
if err != nil {
	panic(err)
}
fmt.Println(first.ID() == again.ID())
// After downtime, submit every slot since the last one you know was
// admitted. Report conflicts: a deploy may explain them, but drift may too.
onConflict := func(day time.Time, err error) {
	slog.Warn("schedule slot admission conflict; verify revision or input drift",
		"slot", day.UTC().Format("2006-01-02"), "err", err)
}
if err := catchUpSlots(today.AddDate(0, 0, -2), today, submitSlot, onConflict); err != nil {
	panic(err)
}
// The schedule's history, oldest admission first.
page, err := runtime.Runs(ctx, core.RunFilter{Labels: map[string]string{"schedule": "nightly-report"}})
if err != nil {
	panic(err)
}
fmt.Println(len(page.Runs), "nightly reports")
```

The host-side helper below continues past retained admission identities,
including pruned runs and conflicts, so an old slot does not block later
missing slots. Every conflict is reported: verify whether it is explained
by a known revision change, and investigate unexpected task/deadline drift.
Do not change a slot's key to bypass a conflict. Other admission errors stop
catch-up and must be retried or surfaced by the host.

```go
// Fragment
func catchUpSlots(first, last time.Time, submitSlot func(time.Time) (*core.RunHandle, error), onConflict func(time.Time, error)) error {
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		_, err := submitSlot(day)
		switch {
		case errors.Is(err, core.ErrRunPruned):
			continue // The admission survives retention; never resubmit with a new key.
		case errors.Is(err, core.ErrAdmissionConflict):
			onConflict(day, err)
			continue // This identity already exists; still admit later missing slots.
		case err != nil:
			return err
		}
	}
	return nil
}
```

Drive `submitSlot` from any timer, using the same helper for each tick. Run
catch-up at startup from the last slot you know was admitted (or from a fixed
look-back window):

```go
// Fragment
for now := range time.Tick(time.Minute) {
	if err := catchUpSlots(now, now, submitSlot, onConflict); err != nil {
		slog.Error("schedule tick failed", "err", err)
	}
}
```

Choose slots with care:

- **Derive everything from the slot.** An exact retry must repeat the task,
  definition revision, and deadline. A task built from `time.Now()` instead
  of the slot makes the second tick return `ErrAdmissionConflict`.
- **Use UTC and one slot width per schedule.** The slot's format is its
  width: a date for a daily job, `2006-01-02T15` for an hourly one. In local
  time, a daylight-saving change merges two hourly slots into one or skips
  one.
- **A deploy does not re-run a slot.** A slot admitted under the old
  revision returns `ErrAdmissionConflict` when the new revision submits it,
  and a slot whose run retention pruned returns `ErrRunPruned`, because
  admission records outlive their runs. Both mean the slot already has a
  run. The new revision starts with the next slot. A conflict on a slot you
  have not redeployed means the submission drifted (see the first point).

Labels make retained schedule history a `Runtime.Runs` query; follow
`RunPage.Next` while `More` is true and use `RootsOnly: true` if reports have
child runs. Retention can remove entries from this query, but not their
admission identities. Keep a separate audit log if permanent history is
required. `httpserver` serves the query at
`GET /v1/runs?label=schedule:nightly-report&roots=true`.

## Observing runs

`Runtime.RunStream` and `RunHandle.Observe` deliver provisional live events:
text and thinking deltas, tool calls, tool progress, tool results, and usage.
Child-run events are included, tagged with `StreamEvent.Agent` (the child
tool's name) and `InvocationID` (the call that started it).
`core.StreamAccumulator` folds them into per-agent views for rendering. Live
views are bounded and drop events rather than slow the run.

A long-running tool can report progress from inside `Execute`:

```go
// Fragment
for i, file := range files {
	core.ReportToolProgress(ctx, core.ToolProgress{Progress: float64(i), Total: float64(len(files)), Message: file})
	// … index file …
}
```

Each report arrives as a `StreamToolProgress` event before the call's result.
Progress is never persisted, and a report made after `Execute` returns is
dropped. To execute a tool outside a Runtime and still receive its progress,
for example in a test or behind another protocol, install a receiver with
`ctx, stop := core.WithToolProgress(ctx, receive)` and call `stop` before
using the result.

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

A host that indexes or exports every run follows one runtime-wide cursor
instead of one per run. `Runtime.WaitChanges(ctx, cursor, limit)` returns
`RunChange` entries in commit order, each naming the run and the range of its
committed events that commit appended; `core.WithChangeEvents()` attaches
those events with their transcript messages. Set
`RetentionPolicy.HoldUnsynced` and `SyncedThrough` to the cursor you have
durably stored, and `Prune` never removes history you have not exported.

## Long conversations

`core.Compactor` is a pre-send hook that summarizes older turns to stay
within a token budget. It keeps the system prompt and recent turns intact,
never separates a tool call from its result, and only changes what is sent,
never the committed transcript. The Claude provider's
`WithConversationCache()` caches the message prefix.

A model has no clock. `core.CurrentTime(nil)` is a pre-send hook that
prefixes the latest user message with the current time. Within each Runtime
worker segment, a canonical user message keeps its first stamp across tool
turns, so prompt caches stay warm. New runs, resumed segments, and direct hook
calls get fresh stamps; timestamps are a rendered hint, not a durable clock.

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

## Upgrading to v0.6

v0.6 adds what a self-hosted platform needs on top of `Runtime`, and no
existing API is removed.

New:

- **Run listing and labels.** `core.WithLabels` annotates a run at
  admission; `Runtime.Runs(filter)` pages through runs by definition, scope,
  state, attention, labels, and admission time.
- **Change feed.** `Runtime.Changes` and `WaitChanges` read one global
  cursor of committed run changes. `RetentionPolicy.HoldUnsynced` keeps
  history until your consumer's `SyncedThrough` cursor has passed it.
- **Manifests.** `Agent.Describe` and `Runtime.Describe` return a JSON
  manifest of a definition for deploy checks and consoles.
- **HTTP and host.** `extensions/httpserver` serves a `Runtime` over HTTP and
  SSE, and the `host` module's `host.Main` runs a durable agent server.
- **Testing kit.** `core/coretest` has a scripted provider and runtime
  fixtures.
- **Ergonomics.** `core.JSONResult`, the `enum` struct tag,
  `core.StructuredOutputToolName`, `RunHandle.Child`,
  `RunSnapshot.ToolCall` and `AppliedEffects`, and the `core.CurrentTime`
  pre-send hook.

Upgrade steps:

1. **Back up the store.** Stop the process and copy the SQLite database (see
   [storage upgrades](docs/durable-runtime.md#storage-upgrades-and-backups)).
2. **Open it with v0.6.** The first `NewRuntime` on a version-9 store
   upgrades it in place, in one transaction: every existing run enters the
   run list, and each run gets one entry in the change feed so an exporter
   sees it. The store is then version 10.
3. **Do not roll back without the backup.** Releases before v0.6 refuse a
   version-10 store. Restore the backup to roll back.

Behavior changes to expect:

- **Tool argument decoding.** Arguments that pass the schema but cannot
  decode into a `Func` parameter (an integer overflowing its field) return a
  recoverable error result instead of failing the run. Integral numbers such
  as `2.0` now decode into Go integer fields.
- **Streaming tool calls without arguments** receive `{}`, as on the
  non-streaming path.
- **`tools.WriteFile`** allows every new model call to replace a file,
  including restoring previous content. Durable admission retries and
  committed tool outcomes prevent replay; there is no permanent path/content
  semantic guard. Pre-write failures report `EffectNotApplied`.
- **Recovery.** A run stopped by a graceful `Close` after its final answer
  was committed is finished by `Recover`, as after a crash. A run whose
  structured-output corrections ran out stays failed across a crash.
- **Effect guards.** Reconcile and late tool completions change a semantic
  guard only while their operation still owns it.
- **Wrapped policies.** An invalid wait or effect policy under
  `WithToolRetry`, `WithLegacyToolErrors`, or `WithToolEffectPolicy` is now
  rejected by `core.New`.

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
| `core.AsTool[P]`, `AsToolFunc[P]` | `core.ChildTool[P](name, description, childRef)`, or `agents.Define[P, O](...)` then `.Tool(name, description)` (the child receives JSON) |
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
  [Upgrading to v0.6](#upgrading-to-v06) for the version-10 store upgrade. See
  [storage upgrades](docs/durable-runtime.md#storage-upgrades-and-backups).

## Agent skill

This repository includes an [Agent Skills](https://agentskills.io) guide for
building Automata applications at
[`.codex/skills/automata-go/`](.codex/skills/automata-go/SKILL.md). Agents that
discover project skills can load it directly. To use it globally in other
projects, copy that directory to `~/.agents/skills/automata-go/`.
