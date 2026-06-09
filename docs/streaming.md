# Streaming

`Agent.RunStream` runs an agent like `Run` while delivering a live event
stream to a callback. This document is the **supported contract** for that
stream — event kinds, ordering guarantees, sub-agent tagging, and the
`StreamAccumulator` that folds the stream into renderable state. Everything
here is pinned by tests in `core/stream_test.go` and
`core/accumulator_test.go`.

```go
out, err := agent.RunStream(ctx, task, func(ev core.StreamEvent) {
    switch ev.Kind {
    case core.StreamText:       // assistant text delta in ev.Text
    case core.StreamToolCall:   // assembled call in ev.ToolCall, about to execute
    case core.StreamToolResult: // ev.Result (and ev.Err) for ev.ToolCall
    case core.StreamUsage:      // per-turn token usage in ev.Usage
    }
})
```

## Event kinds

| Kind               | Populated fields                | Meaning |
|--------------------|---------------------------------|---------|
| `StreamText`       | `Text` (+ `Agent`)              | One assistant content delta. |
| `StreamToolCall`   | `ToolCall` (+ `Agent`)          | A tool call the model requested, fully assembled (name + arguments), emitted before execution. |
| `StreamToolResult` | `ToolCall`, `Result`, `Err` (+ `Agent`) | A finished tool call. `Result` is the exact string fed back to the model; `Err` is non-nil if the tool returned an error (the run still continues — see tool error semantics on `core.Func`). |
| `StreamUsage`      | `Usage` (+ `Agent`)             | Token usage for one completed provider turn. |

## Ordering guarantees

Within one turn of the loop, events arrive in this order:

1. **Text deltas**, in stream order.
2. **`StreamUsage`**, once per turn — after the turn's text, before its tool
   calls. Emitted only when the provider reports usage for the turn.
3. **`StreamToolCall`** events, serially, in the order the model issued the
   calls.
4. **`StreamToolResult`** events, in **completion order** — tools in a batch
   execute concurrently, so results may arrive in any order relative to each
   other (but always after every `StreamToolCall` of the batch).

Turns never interleave for a single agent: the next turn's text starts only
after the previous turn's results. Note that `Usage` totals must be **summed**
across turns (`Usage.Add`); `Usage.Merge` is a field-wise max for cumulative
chunk counts within a turn and would under-count.

The callback is **serialized**: tool results are produced concurrently, but
`RunStream` guards delivery with a mutex, so the callback needs no locking of
its own. Keep it fast — it runs on the loop's critical path.

### Fallback for non-streaming providers

If the provider implements only `core.Provider` (not `core.StreamProvider`),
`RunStream` falls back to one non-streaming invocation per turn and delivers
each turn's whole content as a single `StreamText` event. Tool-call,
tool-result, and usage events fire exactly as above.

## Sub-agent tagging (`StreamEvent.Agent`)

Agents registered as tools via `core.AsTool` / `core.AsToolFunc`
auto-stream: when the enclosing run is streaming, the sub-agent runs in
streaming mode too, and its events are forwarded into the parent's stream.

- `Agent == ""` — the event came from the top-level agent being streamed.
- `Agent == "<tool name>"` — the event came from that sub-agent.
- **Nesting: the innermost tag wins.** A wrapper stamps its tool name only
  when the tag is still empty, so events from `orchestrator → mid → leaf`
  arrive tagged `"leaf"` for leaf's events and `"mid"` for mid's own events.
- Sub-agent `StreamUsage` events are tagged like everything else, so token
  usage is attributable per agent.

Under a plain (non-streaming) `Run`, sub-agents run non-streaming — no events
are produced anywhere.

## StreamAccumulator: from deltas to state

Most consumers don't want raw deltas; they want "what has each agent said and
done so far." `core.StreamAccumulator` folds the stream into exactly that, so
a TUI, web handler, or logger renders a snapshot instead of bookkeeping:

```go
var acc core.StreamAccumulator
out, err := orchestrator.RunStream(ctx, topic, func(ev core.StreamEvent) {
    acc.Add(ev)
    render(acc.Views(), acc.Totals()) // or notify a render loop
})
```

- `Views()` returns one `AgentView` per agent — assembled `Text`, every tool
  call paired with its result (`ToolCalls []ToolCallView`), and per-agent
  summed `Usage` — top-level agent first, then sub-agents in first-seen order.
- `View(agent)` returns a single agent's view ("" for top-level).
- `Totals()` returns usage summed across all agents and turns.
- Views are **copies**: a snapshot taken now is immune to later events.
- All methods are safe for concurrent use, so a server can snapshot from
  another goroutine (e.g. an SSE handler) while the run streams.

The zero value is ready to use.
