# automata

Composable agent primitives in Go, built for traceability and developer
ergonomics in multi-agent systems — the kind of reliability you need when
agents run inside a web server, not a notebook.

- **Primitives, not a platform.** `core` is a small set of orthogonal pieces —
  `Agent`, `Tool`, `Provider`, stream events — that compose into orchestrators,
  sub-agents, and pipelines. No magic, no hidden state.
- **A block-based message model that never degrades a provider.** A `Message`
  is a list of typed `Block`s — text, thinking (with its signature), tool_use,
  tool_result (with an error flag), images — a superset of what any single
  provider exposes. Claude's thinking blocks round-trip through tool loops
  instead of being dropped; native `is_error` survives; nothing is flattened to
  a lowest common denominator. `RawBlock` carries provider-specific blocks so a
  new provider feature never blocks on a core release.
- **Traceable by construction.** Every run emits structured `slog` logs and
  `tracing` spans; every streaming run emits a documented, tested event
  contract (see [docs/streaming.md](docs/streaming.md)) you can fold into a
  UI, an SSE endpoint, or a log with `core.StreamAccumulator`.
- **Dependency-light core.** The root module depends only on
  `golang.org/x/sync`. Vendor SDKs and API keys live in isolated
  `extensions/*` modules.

## Layout

| Module / package | What it is |
| --- | --- |
| `core` | Agent, Runtime, Session, Tool, Provider, streaming, hooks, approval |
| `tools` (module) | First-party tools: `HTTPFetch`, `ReadFile`/`WriteFile` (sandboxed), `Shell` (allow-listed), `WebSearch` |
| `extensions/claude` (module) | Anthropic provider (`core.StreamProvider`); thinking, images, prompt caching |
| `extensions/openai` (module) | OpenAI Chat Completions provider (stdlib-only); any OpenAI-compatible base URL |
| `extensions/tavily` (module) | Tavily backend for `tools.WebSearch` |
| `extensions/sqlite` (module) | Optional persistent local store for `core.Runtime`; exclusive single-process ownership |
| `retry`, `tracing` | Backoff policy and span interfaces used by core |
| `examples/*` (modules) | Runnable demos, including a multi-agent deep-research TUI |

Extensions and examples are separate Go modules tied together by `go.work`,
so importing `core` never pulls a vendor SDK into your build.

### Releases

The root module, `tools`, and every extension are tagged independently
(Go multi-module tagging: `v0.4.0`, `tools/v0.4.0`, `extensions/openai/v0.4.0`,
…). One command does the whole dance:

```sh
scripts/release.sh minor --push # or: v0.4.0 / patch / major
```

It bumps every submodule's `automata` require line, builds and tests the full
workspace, commits, tags root + all published modules, and pushes. Because
`go mod tidy` in a submodule can only resolve the new core version after its
tag is on the remote, the script then refreshes the submodules' `go.sum` files
in a small follow-up commit and verifies each module still builds against the
published pins (`GOWORK=off`). Run it without `--push` to stop after tagging
for review. Note the deliberate module conventions: published modules
(`tools`, `extensions/*`) carry no `replace` directives — in-repo development
resolves through `go.work`, and `replace` in a dependency is ignored
downstream, so they must require real tagged versions — while `examples/*`
keep `replace` directives as dev conveniences and are never tagged.

## Quickstart

For restartable execution, use the explicit durable lifecycle described in
[docs/durable-runtime.md](docs/durable-runtime.md). Construct a
`core.Runtime` with `extensions/sqlite`, register an immutable `Agent` revision,
then submit and await a `RunHandle`. Use `core.NewEphemeralRuntime` only when
loss on process exit is intentional. Storage failure never falls back to
memory. Durable child runs (`core.DurableChildTool`) and serialized
conversation turns (`SubmitOptions.Conversation`) use the same lifecycle.

The direct `Agent.Run`, session, and typed helpers shown below are currently
process-local APIs; they are not persistent Runtime entry points. They are not
protected as legacy surfaces and will be replaced or routed through Runtime as
their durable equivalents land.

```go
package main

import (
 "context"
 "fmt"
 "os"

 "github.com/emotional-data8482/automata/core"
 "github.com/emotional-data8482/automata/extensions/claude"
 "github.com/emotional-data8482/automata/tools"
)

type weatherArgs struct {
 City string `json:"city" desc:"city to look up"`
}

func main() {
 agent := core.New(claude.New("claude-sonnet-4-6", os.Getenv("ANTHROPIC_API_KEY"))).
  WithSystemPrompt("You are a concise assistant.")

 // A typed tool: the JSON schema is derived from the struct fields.
 agent.RegisterTool(core.Func("weather", "Get the weather for a city",
  func(ctx context.Context, a weatherArgs) (string, error) {
   return "sunny in " + a.City, nil
  }))

 // A first-party tool: fetch a page as readable text.
 agent.RegisterTool(tools.HTTPFetch())

 res, err := agent.Run(context.Background(), "What's the weather in Paris?")
 if err != nil {
  panic(err)
 }
 fmt.Println(res.Output)
 fmt.Printf("(%d steps, %d output tokens)\n", res.Steps, res.Usage.OutputTokens)
}
```

`Run` returns a `RunResult` — the final `Output` text, the full `FinalMessage`
(blocks included), the run's `Messages` transcript, summed `Usage`, `Steps`, and
a `StopReason`. It is populated as far as the run got even when `err` is
non-nil, so a run that exhausts its step budget still hands back its partial
transcript and usage. Per-call provider options (temperature, max tokens, stop
sequences, tool choice, thinking budget) are set with
`agent.WithDefaultCallOptions(...)` or per run with
`agent.Run(ctx, task, core.WithCallOptions(...))`.

## Bounded tool execution

Use `ToolPolicy` to enforce deadlines, call budgets, rate limits, and bounded
parallelism outside model prompts:

```go
agent.WithToolPolicy(core.ToolPolicy{
 Timeout:     10 * time.Second,
 MaxCalls:    50,
 MaxParallel: 4,
 PerTool: map[string]core.ToolLimits{
  "http_fetch": {Timeout: 3 * time.Second, MaxCalls: 10, RateLimiter: limiter},
 },
})

// A per-run policy replaces the agent default.
res, err := agent.Run(ctx, task,
 core.WithToolPolicy(core.ToolPolicy{MaxCalls: 10, MaxParallel: 2}))
```

Policy-created tool timeouts are recoverable error results; cancellation of the
parent run remains fatal. Call budgets reserve known requests in model order
before approval, overflow calls receive explicit transcript results, and total
budgets are shared atomically through nested `AsTool` runs (durable children
share them as persisted subtree caps instead). Tools and limiters
must honor context cancellation—Go cannot forcibly stop a function that ignores
its context. The zero policy preserves existing behavior.

Call reservations happen before `Approver`; approved calls then apply timeout,
rate-limit wait, and execution (including any internal `WithToolRetry` attempts).
A child may add stricter local limits, while timeouts/rate limiters/parallelism
otherwise remain agent-local. See the
[tool-execution project notes](planning/archive/tool-execution-safety/README.md)
for the complete accounting and composition decisions.

## Sessions and transcripts

`Agent.Run` is one-shot. For multi-turn conversations — and for the audit
trail — use a `Session`: every run continues the same conversation, and the
full transcript (system prompt, tasks, replies, tool calls and results) is
plain data you can persist and resume. The transcript is recorded even when a
run fails, so you can always see what happened.

```go
sess := agent.NewSession()
draft, _ := sess.Run(ctx, "Draft a refund policy for our SaaS")
final, _ := sess.Run(ctx, "Make it friendlier and add a 30-day clause")
fmt.Println(final.Output) // each Run returns a RunResult

// Persist anywhere; resume later, even in another process. Every block type —
// text, thinking (with signature), tool calls and results, images — round-trips
// through JSON, so the resumed conversation is byte-for-byte the same.
blob, _ := json.Marshal(sess.Messages())
var transcript []core.Message
_ = json.Unmarshal(blob, &transcript)
sess = agent.ResumeSession(transcript)
_ = draft
```

For durable execution, use `Runtime`. The old callback-based checkpoint hook
was removed because callback success is not proof of durable commit. Runtime
instead supports named, timeout-bounded `CommittedRunHook`s that run after the
execution result commits; their outcomes are persisted in `RunSnapshot` and do
not rewrite the execution result. `Session` remains a process-local
conversation API. For durable, serialized turns that survive restarts, submit
Runtime runs with `SubmitOptions.Conversation`; see
[Conversations](docs/durable-runtime.md#conversations).

## Typed results

`core.RunTyped[T]` returns the agent's final answer decoded into a Go struct.
The typed helpers use the same structured-output engine as
`AgentConfig.StructuredOutput`: the accepted payload is validated, stored on
`RunResult.StructuredOutput`, and then decoded into `T`. For an agent without a
declared output contract, the helper derives a JSON schema from `T` and uses the
hidden `automata_structured_output` tool; if the model answers in prose, core
first tries to parse an embedded payload and only forces the tool on one more
turn as a last resort. The agent's regular tools still work alongside it.

```go
type Person struct {
 Name string `json:"name"`
 Age  int    `json:"age" desc:"age in years"`
}

p, res, err := core.RunTyped[Person](ctx, agent, "Who is Ada Lovelace?")
// p.Name == "Ada Lovelace"; res carries usage/steps/transcript.
```

If the agent already declares `AgentConfig.StructuredOutput`, typed helpers
reuse that pinned schema instead of injecting another terminal tool; choose a Go
result type compatible with the declaration. Use `RunSessionTyped` to keep a
process-local conversation across typed decisions:

```go
sess := agent.NewSession()
first, _, err := core.RunSessionTyped[Person](ctx, sess, "Choose the first action")

blob, _ := json.Marshal(sess.Messages())
var transcript []core.Message
_ = json.Unmarshal(blob, &transcript)
sess = agent.ResumeSession(transcript)

next, res, err := core.RunSessionTyped[Person](ctx, sess, "Choose the next action")
_, _, _, _ = first, next, res, err
```

### Validation guarantee

The returned value is validated against the same schema the model was shown
before it is returned: required fields (exported fields without `omitempty`)
are present, types match (`int` fields get integral numbers, and so on), and
nested structs, slices, and string-keyed maps are checked recursively. Unknown
JSON fields are ignored, matching `json.Unmarshal`. A payload that fails
validation is never returned as a zero-filled `T` — this is a deliberate
behavior change: models that previously "succeeded" while omitting fields now
produce a typed error after correction.

When validation fails, the violations are fed back to the model as a new user
turn on the same session and it is asked to call the tool again. The default
budget is one correction turn; `core.WithMaxCorrectionTurns(n)` changes it (0
disables correction). When attempts are exhausted — or the final forced turn
still produces an invalid payload — the run returns an error matching
`core.ErrInvalidStructuredOutput` via `errors.Is`, with the per-field
violations available via `errors.As(*core.InvalidStructuredOutputError)`. The
`RunResult` is still populated as far as the run got.

Raw declared schemas, typed helper schemas, tool input schemas, and native
output schemas share one supported contract. Core enforces object properties,
required fields, arrays, enum, nullable single-type unions, and selected string
and numeric bounds; unsupported assertion keywords such as `$ref`, `oneOf`, and
`const` are rejected instead of ignored. Provider-facing annotations and native
metadata such as `title`, `description`, `format`, and OpenAI `strict` are
preserved for adapters while core enforces its documented subset.

The full sequence per typed call is bounded: 1 (initial) + correction budget +
1 (forced fallback) provider turns at worst. Prose answers that already
contain valid JSON (a fenced ```json block or a bare object) are parsed and
validated with no extra provider turn. Each phase commits its canonical
transcript to the owning process-local Session. For durable correction and
effect preservation, declare the output contract on the Agent and run it through
`Runtime`; direct typed helpers remain process-local transitional APIs.

### Provider-native structured output

By default the hidden tool is the provider-neutral mechanism. Pass
`core.WithNativeStructuredOutput()` or set `StructuredOutput.Native` to opt into
provider-native schema enforcement when the provider supports it — the OpenAI
Chat Completions extension maps the schema onto `response_format: json_schema`
(strict variant when the schema has no free-form objects), and the Claude
extension maps it onto `output_config.format`. Providers without native support
silently use the hidden-tool path, so the option is safe to set
unconditionally. Native responses are parsed from the reply text and validated
by the same validator; an unusable native payload corrects within the run's
bounded correction budget.

### Tool name

The hidden tool is named `automata_structured_output` (namespaced so a user
tool called `structured_output` cannot collide with it). Registering a tool
with that exact name makes typed runs fail fast with an explicit error. The
name appears in persisted transcripts (as plain history — resumed sessions are
unaffected by the 0.3-era rename from `structured_output`).

## Rich tool results

Tools can return block-based content — mixed text and images — instead of only
a string. `core.FuncResult[P]` is `core.Func` for rich outputs: the same typed
schema generation, but the handler returns a `core.ToolResult`, which the run
loop records as block-based `ToolResultBlock` content in the transcript:

```go
type shotArgs struct {
    Target string `json:"target" desc:"what to capture"`
}

agent.RegisterTool(core.FuncResult("screenshot", "Capture an image",
    func(ctx context.Context, a shotArgs) (core.ToolResult, error) {
        png, err := capture(ctx, a.Target)
        if err != nil {
            return core.ToolResult{}, err // recoverable: the model sees the error
        }
        return core.BlockResult(
            core.TextBlock{Text: "captured " + a.Target},
            core.ImageBlock{MediaType: "image/png", Data: png},
        ), nil
    }))
```

Result constructors: `TextResult`, `BlockResult`, `ErrorResult`, `ImageResult`
(inline base64), and `URLImageResult` (by reference). Existing string tools
(`Tool`, `Func`, `AsTool`, `WithToolRetry`) are unaffected and keep working —
`FuncResult` returns a `Tool` and registers through the same paths, and a tool
may implement the optional `core.ResultTool` interface (`ExecuteResult`) to opt
in while keeping `Execute` for text-only consumers.

Streaming consumers keep reading `StreamEvent.Result` (the text view); richer
consumers can inspect `StreamEvent.ResultBlocks`, mirrored on
`ToolCallView.ResultBlocks` in `StreamAccumulator` views.

Provider support differs: Anthropic passes text and image tool-result content
natively (order preserved); OpenAI Chat Completions is text-only, so non-text
blocks degrade to a documented placeholder (`[non-text tool result block:
image/png]`) rather than being dropped. Keep rich results small — image data
lands in the transcript base64-encoded and, where supported, is sent to the
provider verbatim.

## Multi-agent: sub-agents are just tools

An `Agent` becomes a tool on another agent with `core.AsTool` — the type
parameter defines the JSON schema the orchestrator's model fills in:

```go
orch.RegisterTool(core.AsTool[researchParams](researcher, "researcher",
 "Delegate a focused research assignment."))
```

`AsTool` forwards the raw JSON arguments as the sub-agent's task. When you'd
rather hand the sub-agent natural language (no "you will receive JSON…"
boilerplate in its prompt), use `AsToolFunc` with a renderer:

```go
orch.RegisterTool(core.AsToolFunc[researchParams](researcher, "researcher",
 "Delegate a focused research assignment.",
 func(p researchParams) string {
  return fmt.Sprintf("Research: %s\nQuestions:\n- %s",
   p.Topic, strings.Join(p.Questions, "\n- "))
 }))
```

`AsTool` and `AsToolFunc` run the sub-agent inside the parent's tool call, in
process; the child is lost on restart, and `Runtime` rejects them. For durable
composition, register the child as its own definition and declare it with
`core.DurableChildTool`. Each call then becomes a linked child run with its own
record, and the child shares the parent's persisted caps and cancellation. See
[Durable children](docs/durable-runtime.md#durable-children).

## Watch every agent work

`RunStream` delivers a live event stream — including events from nested
sub-agents, tagged with the sub-agent's name. `StreamAccumulator` folds the
deltas into per-agent state so rendering is a snapshot, not bookkeeping:

```go
var acc core.StreamAccumulator
res, err := orch.RunStream(ctx, topic, func(ev core.StreamEvent) {
 acc.Add(ev)
 for _, v := range acc.Views() { // top-level first, then sub-agents
  fmt.Printf("[%s] %d tool calls, %d tokens\n",
   v.Agent, len(v.ToolCalls), v.Usage.OutputTokens)
 }
})
_ = res // RunStream returns the same RunResult as Run
```

The full ordering and tagging contract is documented in
[docs/streaming.md](docs/streaming.md).

## Long conversations

For multi-turn sessions and long tool loops, `core.Compactor` is a pre-send hook
that summarizes older turns to stay within a token budget (keeping the system
prompt and recent turns intact, never splitting a tool call from its result),
and the Claude provider's `WithConversationCache()` caches the message prefix so
each turn re-reads it cheaply. See [docs/context.md](docs/context.md).

## Web search

`tools.WebSearch` is vendor-neutral; backends implement `tools.Searcher` in
their own modules:

```go
researcher.RegisterTool(tools.WebSearch(tavily.New(os.Getenv("TAVILY_API_KEY"))))
```

## Examples

- `examples/claude` — minimal tool-using agent.
- `examples/durable_typed` — credential-free fake-provider Runtime demo showing
  a mutating tool, an invalid structured payload, correction to a typed domain
  value, and one external write; then a durable child with typed output inside
  a two-turn Runtime conversation:

  ```sh
  go run ./examples/durable_typed
  ```

- `examples/typed_agents` — sub-agents registered as typed tools (`AsToolFunc`,
  and `RunTyped` wrapped in a `Func` for a typed-in/typed-out child) driving a
  `RunSessionTyped` triage session that checkpoints to JSON and resumes between
  turns. Needs `ANTHROPIC_API_KEY`:

  ```sh
  go run ./examples/typed_agents
  ```

- `examples/deep_research` — orchestrator + researcher + writer with a live
  Bubble Tea TUI rendered entirely from a `StreamAccumulator`. Needs
  `ANTHROPIC_API_KEY` and `TAVILY_API_KEY`:

  ```sh
  go run ./examples/deep_research "the impact of GLP-1 drugs on US healthcare costs"
  ```

## Agent skill

This repository includes an [Agent Skills](https://agentskills.io) guide for
building Automata applications at
[`.codex/skills/automata-go/`](.codex/skills/automata-go/SKILL.md). Agents that
discover project skills can load it directly. To use it globally in other
projects, copy that directory to `~/.agents/skills/automata-go/`.

## Roadmap

See [docs/roadmap.md](docs/roadmap.md).
