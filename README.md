# automata

Composable agent primitives in Go, built for traceability and developer
ergonomics in multi-agent systems — the kind of reliability you need when
agents run inside a web server, not a notebook.

- **Primitives, not a platform.** `core` is a small set of orthogonal pieces —
  `Agent`, `Tool`, `Provider`, stream events — that compose into orchestrators,
  sub-agents, and pipelines. No magic, no hidden state.
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
| `core` | Agent, Loop, Tool, Provider, streaming, hooks, approval |
| `tools` (module) | First-party tools: `HTTPFetch`, `ReadFile`/`WriteFile` (sandboxed), `Shell` (allow-listed), `WebSearch` |
| `extensions/claude` (module) | Anthropic provider (`core.StreamProvider`) |
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

	"github.com/emotional-data/automata/core"
	"github.com/emotional-data/automata/extensions/claude"
	"github.com/emotional-data/automata/tools"
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

	out, err := agent.Run(context.Background(), "What's the weather in Paris?")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
}
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
out, err := orch.RunStream(ctx, topic, func(ev core.StreamEvent) {
	acc.Add(ev)
	for _, v := range acc.Views() { // top-level first, then sub-agents
		fmt.Printf("[%s] %d tool calls, %d tokens\n",
			v.Agent, len(v.ToolCalls), v.Usage.OutputTokens)
	}
})
```

The full ordering and tagging contract is documented in
[docs/streaming.md](docs/streaming.md).

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
