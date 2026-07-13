# Roadmap

**Guiding principle (unchanged):** `core` stays primitive-only and
dependency-light (today: just `golang.org/x/sync`). Anything that pulls a vendor
SDK or an API key lives in its own module under `extensions/<name>/`, wired via
`go.work` — the same pattern as `extensions/claude`.

## v0.2.0 — shipped

A milestone focused on not degrading top-end provider quality and on running
agents in long-lived, multi-turn settings.

- **Block-based message model.** `Message` is now a list of typed `Block`s
  (`TextBlock`, `ThinkingBlock` with signature, `ToolUseBlock`,
  `ToolResultBlock` with `IsError`, `ImageBlock`, and a `RawBlock` escape hatch),
  a type-tagged JSON envelope that round-trips through persistence. Claude's
  thinking blocks now survive tool loops; tool-result errors use the native
  `is_error`; images are representable.
- **Structured run results.** `Run`/`RunStream` (and the `Session` variants)
  return a `RunResult` — output text, final message, transcript, summed usage,
  step count, and stop reason — populated even on error (max-steps returns
  partials).
- **Per-call provider options.** `Provider` takes a `Request` with `CallOptions`
  (temperature, max tokens, stop sequences, tool choice, thinking budget), set
  per-agent (`WithDefaultCallOptions`) or per-run (`WithCallOptions`).
- **Typed output.** `RunTyped[T]` returns the final answer decoded into a Go
  struct via a forced schema-derived tool.
- **Context management.** `core.Compactor` (a pre-send hook that summarizes older
  turns) and the Claude provider's `WithConversationCache()`; see
  [context.md](context.md).
- **OpenAI extension.** `extensions/openai`, a stdlib-only Chat Completions
  provider that works against any OpenAI-compatible base URL.
- **Fixes.** Sub-agent double-retry removed (tools own retries; opt in with
  `WithToolRetry`); stream events carry an `InvocationID` so parallel same-name
  sub-agents no longer garble the accumulator; `retry.Do` no longer silently
  succeeds on a non-positive `MaxAttempts`.

## Earlier: the v0.1.0 tools/streaming proposals

The three proposals below were prompted by building `examples/deep_research` and
all shipped in v0.1.0. Retained here for context.

---

## 1. A first-party tools library

**Problem.** Every tool is hand-rolled today. A *research* agent's most basic
need — search the web, fetch a page — meant writing a Tavily client from
`net/http` up (`examples/deep_research/tools.go`). Newcomers have to build
integrations before they can orchestrate anything, which is the slowest path to
a working agent.

**Proposal.** Ship reusable `core.Tool` constructors, split by dependency weight:

- **`tools/` (new module, stdlib-only):** zero-key, no-SDK tools that can ship
  broadly — `tools.HTTPFetch()` (GET + readability-ish text extraction),
  `tools.ReadFile()/WriteFile()` (sandboxed to a root dir), `tools.Shell()`
  (opt-in, allow-listed). These compose with the existing `core.Func[P]` schema
  machinery; no new core API needed.
- **Pluggable web search** behind a small interface so the *tool* is vendor-neutral
  and the *backend* is swappable:

  ```go
  // package tools
  type Searcher interface {
      Search(ctx context.Context, query string, max int) ([]Result, error)
  }
  type Result struct{ Title, URL, Content string }

  // WebSearch adapts any Searcher into a core.Tool named "web_search".
  func WebSearch(s Searcher) core.Tool
  ```

  Keyed backends live as their own modules: `extensions/tavily`,
  `extensions/brave`, … each implementing `tools.Searcher`. The deep_research
  Tavily client moves here largely as-is.

**Open questions.** Whether `tools` is one module or split per concern
(`tools/web`, `tools/fs`); how far to take HTML→text extraction before it earns
its own dependency (and therefore a module move).

**Status:** Proposed. The Tavily client in `examples/deep_research` is the seed
for `extensions/tavily`.

---

## 2. First-class streaming-sub-agent observability

**Problem.** "Watch all the agents work" is the whole point of a multi-agent
demo, but the enabling pieces only landed *while* building deep_research:
`AsTool` now auto-streams sub-agents (events tagged with `StreamEvent.Agent`),
and the loop emits `StreamUsage`. Those primitives ship — but every consumer
still hand-writes the bookkeeping to turn the flat event stream into a
per-agent view (see the `handleEvent`/`appendText`/`openText` dance in
`examples/deep_research/tui.go`). That ~80 lines will be re-invented by everyone.

**Proposal.** Two parts:

- **Document + test the contract** now that it works: `StreamEvent.Agent` tagging
  (empty = top-level; innermost tag wins on nesting), `StreamUsage` semantics
  (once per turn, after text and before tool calls), and the `RunStream`
  ordering guarantees. Promote these from "emergent" to "supported."
- **A framework-agnostic accumulator** that folds the stream into structured
  state, so UIs (TUI, web, plain log) consume a snapshot instead of raw deltas:

  ```go
  // package core (or a thin core/streamagg)
  type AgentView struct {
      Agent     string
      Text      string        // assembled streamed text
      ToolCalls []ToolCall
      Usage     Usage         // summed for this agent
  }
  type StreamAccumulator struct{ /* ... */ }
  func (a *StreamAccumulator) Add(ev StreamEvent)        // fold one event
  func (a *StreamAccumulator) Views() []AgentView        // current per-agent snapshot
  func (a *StreamAccumulator) Totals() Usage
  ```

  The deep_research TUI collapses to "`acc.Add(ev)` then render `acc.Views()`."
  No bubbletea dependency in core — the accumulator is pure data.

**Open questions.** Whether the accumulator belongs in `core` or a sibling
package; how to model interleaving when sibling sub-agents stream concurrently
(group by `Agent`, which the example already relies on).

**Status:** Enabling primitives shipped (`StreamEvent.Agent`, `StreamUsage`,
streaming `AsTool`). The accumulator + docs are the remaining work.

---

## 3. Structured sub-agent handoff

**Problem.** `AsTool[P]` marshals `P` to JSON and forwards it *verbatim as the
task string*. It works, but it's stringly-typed at the seam: the sub-agent
receives raw JSON as its user message and must be *prompted* to parse it (note
how the researcher/writer system prompts in `examples/deep_research/agents.go`
spell out the JSON shape). That couples the orchestrator's schema to prose in
the sub-agent's prompt.

**Proposal.** Keep `AsTool[P]` (verbatim JSON) as the zero-config default, and
add a renderer variant so the caller controls how typed params become the task:

  ```go
  // AsToolFunc renders the typed params into the sub-agent's task, instead of
  // forwarding raw JSON. The schema advertised to the model is still derived
  // from P.
  func AsToolFunc[P any](a *Agent, name, desc string, render func(P) string) Tool
  ```

  Then a research assignment hands off as natural language the sub-agent already
  understands, with no "you will receive JSON…" boilerplate in its prompt:

  ```go
  core.AsToolFunc[researchParams](researcher, "researcher", "...",
      func(p researchParams) string {
          return fmt.Sprintf("Research: %s\nQuestions:\n- %s",
              p.Topic, strings.Join(p.Questions, "\n- "))
      })
  ```

**Open questions.** Whether to also thread the typed value through to the
sub-agent via context (for tools that want the struct, not the rendered text);
whether `render` returning `(string, error)` is worth the ergonomic cost.

**Status:** Proposed. Backwards-compatible — `AsTool[P]` stays as-is.

---

## Non-goals

- A built-in UI. The framework's boundary is a clean `StreamEvent` stream;
  rendering stays the application's job (proposal #2 only removes the
  *bookkeeping*, not the rendering).
- Pulling vendor SDKs or keys into `core`. Those remain extension modules.
- Implicit shared agent memory. Cross-agent state stays explicit and
  caller-owned (e.g. the `todoStore` in deep_research), which keeps data flow
  legible.
