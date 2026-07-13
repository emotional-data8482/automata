# Context management for multi-turn loops

Long agent runs and multi-turn sessions grow their transcript every turn. Two
first-party pieces keep that growth from blowing the context window or the token
bill: a **compactor** that summarizes older turns, and **prompt caching** that
re-reads the stable prefix cheaply.

## Compaction: `core.Compactor`

`core.Compactor` is a [pre-send hook](../core/hooks.go): it transforms the view
sent to the provider without touching the canonical transcript, so
`Session.Messages()` still returns the full history for audit and persistence.

```go
agent.WithPreSendHook(core.Compactor(summarizerProvider, core.CompactorConfig{
    TriggerTokens: 120_000, // start compacting when the estimate crosses this
    KeepRecent:    8,       // keep the last 8 messages verbatim
}))
```

When the estimated context size crosses `TriggerTokens`, the hook replaces the
messages between the system prompt and the recent window with an
LLM-generated summary, producing:

```
[system prompt] [summary of older turns] [last KeepRecent messages]
```

Guarantees and behavior:

- **The canonical transcript is never mutated** — only the per-turn send view.
- **Tool pairs stay together.** The summarize/keep boundary is never placed
  between an assistant `tool_use` and its `tool_result`; the cut advances past
  trailing tool results so a pair is always summarized or kept as a unit.
- **Roles stay valid.** The summary is a user message; if the kept window would
  start with another user message, the summary is folded into it so no two user
  turns land in a row.
- **The summary is memoized.** It is regenerated only every
  `CompactorConfig.MinRecompute` messages (default `KeepRecent`); in between, the
  few newer messages are carried verbatim. Memoization is keyed by the content
  it covers, so a compactor shared across concurrent sessions stays correct.
- **Token estimation is a heuristic.** It uses the most recent reported `Usage`
  when available, else ~4 characters per token over the serialized transcript.
  Treat `TriggerTokens` as approximate.

The summarizer is any `core.Provider` — typically a cheap, fast model. A
summarization failure aborts the run (like any pre-send hook error).

## Prompt caching (Anthropic)

The `extensions/claude` provider exposes two cache breakpoints, which compose
(two of Anthropic's four):

- `WithSystemPromptCache()` caches the **tools + system** prefix — stable across
  every turn of a run.
- `WithConversationCache()` caches the **messages** prefix by stamping a
  breakpoint on the last block of the last message. In a tool loop the prefix is
  stable and grows by one turn per request, so each turn re-reads the prior
  prefix from cache instead of re-billing it. The breakpoint is placed on the
  last cacheable block, walking back past thinking blocks (which reject
  `cache_control`).

```go
provider := claude.New(model, key).
    WithSystemPromptCache().
    WithConversationCache()
```

### Compaction vs. conversation cache

Compaction rewrites the message prefix when it fires, which **invalidates the
conversation cache for that turn** — the new (shorter) prefix has to be written
to cache once, then re-reads cheaply on the turns that follow. This is the
expected trade-off: you pay one cache-write at each compaction boundary to cap
the prefix size. The minimum cacheable prefix is model-dependent (~1–4k tokens),
so very small conversations may not cache at all.
