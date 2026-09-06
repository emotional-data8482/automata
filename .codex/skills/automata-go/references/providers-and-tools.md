# Providers and First-Party Tools

Automata keeps provider SDKs and tool integrations in separate Go modules. Add only what the application uses and keep module releases compatible.

## Anthropic Claude

```bash
go get github.com/emotional-data8482/automata@v0.4.0 \
  github.com/emotional-data8482/automata/extensions/claude@v0.4.0
```

```go
provider := claude.New(model, apiKey).
    WithMaxTokens(16_000).
    WithSystemPromptCache().
    WithConversationCache()
agent := core.New(provider)
```

If `apiKey` is empty, the Anthropic SDK uses `ANTHROPIC_API_KEY`. The provider implements `core.StreamProvider` and preserves text, thinking/signatures, images, tool calls, rich text/image tool results, and supported raw blocks.

Notes:

- The provider default maximum is 16,000 tokens; a per-run `core.CallOptions.MaxTokens` overrides it.
- `ThinkingBudget`, temperature, stop sequences, and tool choice are supported.
- System-prompt caching covers the tools + system prefix.
- Conversation caching marks the growing message prefix. It can be combined with system caching.
- Anthropic cache minimums vary by model, so small prompts may produce no cache hit.
- Compaction rewrites the prefix and invalidates the conversation cache for that turn; later turns re-warm it.
- Extended thinking cannot be combined with a forced structured-output tool turn. `RunTyped` handles this fallback by disabling thinking for that forced turn.

## OpenAI-Compatible Chat Completions

```bash
go get github.com/emotional-data8482/automata@v0.4.0 \
  github.com/emotional-data8482/automata/extensions/openai@v0.4.0
```

```go
provider := openai.
    New(model, "https://api.openai.com/v1").
    WithAPIKey(os.Getenv("OPENAI_API_KEY")).
    WithStreamUsage()
agent := core.New(provider)
```

The stdlib-only provider works with OpenAI-compatible Chat Completions endpoints such as Ollama, vLLM, OpenRouter, or a compatible gateway by changing the base URL.

Notes:

- `WithStreamUsage` sends `stream_options.include_usage=true`. It is opt-in because some compatible backends reject that field.
- `ThinkingBudget` is ignored.
- Thinking and provider-raw blocks are dropped when sent because Chat Completions has no equivalent input.
- Images become `image_url` parts; inline data uses a data URL.
- Tool result errors are represented with an `error:` content prefix because Chat Completions lacks a native error flag.
- Rich tool result content is flattened for Chat Completions; non-text blocks degrade to placeholders such as `[non-text tool result block: image/png]` instead of being dropped.
- Temperature, max tokens, stop sequences, and tool choice are supported.

Do not switch an active provider-native transcript between providers casually. Prefer separate specialist sessions and typed handoff artifacts.

## Custom Provider

Implement `core.Provider` for non-streaming execution:

```go
type Provider interface {
    Invoke(ctx context.Context, req core.Request) (core.Response, error)
}
```

Implement `core.StreamProvider` to stream:

```go
type StreamProvider interface {
    core.Provider
    InvokeStream(ctx context.Context, req core.Request) (<-chan core.StreamChunk, error)
}
```

A provider must:

- Convert `req.Messages`, `req.Tools`, and supported `req.Options` without silently flattening core data it can preserve.
- Set both provider-neutral `Response.StopReason` and verbatim `RawStopReason`.
- Attach per-turn usage to `Response.Message.Usage`.
- Return retry-classifiable API errors when appropriate.
- For streaming, assemble provider deltas into indexed `core.BlockDelta` values and report terminal reason, usage, and errors via `core.StreamChunk`.

`RunStream` falls back to whole-turn events when a provider implements only `core.Provider`.

## Retry Policy

Provider invocations use the agent retry policy:

```go
agent.WithRetry(retry.Config{
    MaxAttempts:  4,
    InitialDelay: 500 * time.Millisecond,
    MaxDelay:     8 * time.Second,
    Multiplier:   2,
    RetryUnknown: false,
})
```

The default is four attempts with exponential backoff and jitter. By default, only errors implementing:

```go
type Retryable interface { Retryable() bool }
```

are retried. Context cancellation/deadlines never retry. Leave `RetryUnknown` false unless replaying every unknown provider failure is known to be safe.

Tools own a separate retry decision:

```go
agent.RegisterTool(core.WithToolRetry(idempotentTool, retry.DefaultConfig()))
```

Do not retry non-idempotent operations or whole sub-agent tools this way.

## First-Party Tools Module

```bash
go get github.com/emotional-data8482/automata/tools@v0.4.0
```

### Sandboxed File Access

```go
agent.RegisterTool(tools.ReadFile(workspaceRoot))
agent.RegisterTool(tools.WriteFile(workspaceRoot))
```

- `ReadFile` exposes `read_file`, uses `os.Root` to reject traversal/symlink escapes, and truncates after 256 KiB.
- `WriteFile` exposes `write_file`, uses `os.Root`, creates parent directories, and replaces complete file content.
- Give read and write tools only to roles that need them. An isolated root limits filesystem scope but does not provide per-file authorization, version preconditions, conflict detection, audit persistence, or rollback.

### Allow-Listed Shell

```go
agent.RegisterTool(tools.Shell(tools.ShellConfig{
    Allow:   []string{"go", "git"},
    Dir:     workspaceRoot,
    Timeout: 30 * time.Second,
}))
```

- Empty `Allow` denies every command.
- Program names must match exactly.
- Commands execute as argv via `exec.CommandContext`; pipes, redirections, globs, variable expansion, and command substitution do not run.
- Combined output is capped at 64 KiB.
- A tool-local timeout is a recoverable error; parent context cancellation aborts the run.

An allow-listed binary may still expose dangerous flags (`git`, interpreters, package managers, and build tools can mutate broadly or execute subprocesses). Validate argument policy in an application-owned tool when exact program allow-listing is insufficient.

### HTTP Fetch

```go
agent.RegisterTool(tools.HTTPFetch())
```

`http_fetch` accepts HTTP(S), extracts readable HTML or returns text/JSON/XML, times out after 30 seconds, and caps responses at 512 KiB. It does **not** prevent SSRF; production apps must enforce destination/redirect/DNS/egress policy with an approver, proxy, or custom tool.

### Vendor-Neutral Web Search with Tavily

```bash
go get github.com/emotional-data8482/automata/tools@v0.4.0 \
  github.com/emotional-data8482/automata/extensions/tavily@v0.4.0
```

```go
search := tavily.New(os.Getenv("TAVILY_API_KEY"))
search.Depth = "advanced" // optional; default is "basic"
agent.RegisterTool(tools.WebSearch(search))
```

`tools.WebSearch` accepts any implementation of:

```go
type Searcher interface {
    Search(ctx context.Context, query string, max int) ([]tools.Result, error)
}
```

Tavily defaults to a 30-second client and asks for raw results rather than a synthesized answer. The model remains responsible for synthesis and citation.
