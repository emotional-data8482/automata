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
|---|---|
| `core` | Agent, Loop, Session, Tool, Provider, streaming, hooks, approval |
| `tools` (module) | First-party tools: `HTTPFetch`, `ReadFile`/`WriteFile` (sandboxed), `Shell` (allow-listed), `WebSearch` |
| `extensions/claude` (module) | Anthropic provider (`core.StreamProvider`); thinking, images, prompt caching |
| `extensions/openai` (module) | OpenAI Chat Completions provider (stdlib-only); any OpenAI-compatible base URL |
| `extensions/tavily` (module) | Tavily backend for `tools.WebSearch` |
| `retry`, `tracing` | Backoff policy and span interfaces used by core |
| `examples/*` (modules) | Runnable demos, including a multi-agent deep-research TUI |

Extensions and examples are separate Go modules tied together by `go.work`,
so importing `core` never pulls a vendor SDK into your build.

## Quickstart

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

## Typed results

`core.RunTyped[T]` returns the agent's final answer decoded into a Go struct.
It injects a hidden tool whose JSON schema is derived from `T` and ends the run
when the model calls it; if the model answers in prose instead, it forces the
tool on one more turn. The agent's regular tools still work alongside it.

```go
type Person struct {
	Name string `json:"name"`
	Age  int    `json:"age" desc:"age in years"`
}

p, res, err := core.RunTyped[Person](ctx, agent, "Who is Ada Lovelace?")
// p.Name == "Ada Lovelace"; res carries usage/steps/transcript.
```

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
- `examples/deep_research` — orchestrator + researcher + writer with a live
  Bubble Tea TUI rendered entirely from a `StreamAccumulator`. Needs
  `ANTHROPIC_API_KEY` and `TAVILY_API_KEY`:

  ```sh
  go run ./examples/deep_research "the impact of GLP-1 drugs on US healthcare costs"
  ```

## Roadmap

See [docs/roadmap.md](docs/roadmap.md).
