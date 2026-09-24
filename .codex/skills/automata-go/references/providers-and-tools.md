# Providers and First-Party Tools

Automata keeps provider SDKs, the SQLite store, and tool integrations in separate Go modules. Add only what the application uses and keep module releases compatible (pin the same release across `automata` and its extensions).

## Anthropic Claude

```bash
go get github.com/emotional-data8482/automata@latest \
  github.com/emotional-data8482/automata/extensions/claude@latest
```

```go
provider := claude.New(model, apiKey).
    WithMaxTokens(16_000).
    WithSystemPromptCache().
    WithConversationCache()
agent, err := core.New(provider, core.AgentConfig{SystemPrompt: prompt})
```

If `apiKey` is empty, the Anthropic SDK uses `ANTHROPIC_API_KEY`. The provider implements `core.StreamProvider` and preserves text, thinking/signatures, images, tool calls, rich text/image tool results, and supported raw blocks.

Notes:

- The provider default maximum is 16,000 tokens; `AgentConfig.CallOptions.MaxTokens` overrides it.
- `ThinkingBudget`, temperature, stop sequences, and tool choice are supported.
- System-prompt caching covers the tools + system prefix.
- Conversation caching marks the growing message prefix. It can be combined with system caching.
- Anthropic cache minimums vary by model, so small prompts may produce no cache hit.
- Compaction rewrites the prefix and invalidates the conversation cache for that turn; later turns re-warm it.
- Native structured output maps onto `output_config.format`.
- Extended thinking cannot be combined with a forced structured-output tool turn. The runtime disables thinking for that forced final turn only.

## OpenAI-Compatible Chat Completions

```bash
go get github.com/emotional-data8482/automata@latest \
  github.com/emotional-data8482/automata/extensions/openai@latest
```

```go
provider := openai.
    New(model, "https://api.openai.com/v1").
    WithAPIKey(os.Getenv("OPENAI_API_KEY")).
    WithStreamUsage()
agent, err := core.New(provider, core.AgentConfig{SystemPrompt: prompt})
```

The stdlib-only provider works with OpenAI-compatible Chat Completions endpoints such as Ollama, vLLM, or a compatible gateway by changing the base URL. For OpenRouter, prefer the dedicated `extensions/openrouter` module (reasoning output and cache-write usage).

Notes:

- `WithStreamUsage` sends `stream_options.include_usage=true`. It is opt-in because some compatible backends reject that field.
- `ThinkingBudget` is ignored.
- Thinking and provider-raw blocks are dropped when sent because Chat Completions has no equivalent input.
- Images become `image_url` parts; inline data uses a data URL.
- Tool result errors are represented with an `error:` content prefix because Chat Completions lacks a native error flag.
- Rich tool result content is flattened for Chat Completions; non-text blocks degrade to placeholders such as `[non-text tool result block: image/png]` instead of being dropped.
- Temperature, max tokens, stop sequences, and tool choice are supported.
- Native structured output sends a `response_format` JSON schema, strict when the schema allows it and best-effort otherwise. Backends that reject `response_format` fail the call; keep typed runs on those backends on the default hidden-tool path (`Native: false`).
- `WithHTTPClient` replaces the HTTP client (tests, proxies, custom transports).

## OpenRouter

```bash
go get github.com/emotional-data8482/automata@latest \
  github.com/emotional-data8482/automata/extensions/openrouter@latest
```

```go
provider := openrouter.New("anthropic/claude-sonnet-4.5",
    openrouter.WithAPIKey(os.Getenv("OPENROUTER_API_KEY")),
    openrouter.WithAppInfo("https://example.com", "My App"), // optional attribution headers
)
agent, err := core.New(provider, core.AgentConfig{SystemPrompt: prompt})
```

The model string is an OpenRouter model ID. The provider wraps the official OpenRouter Go SDK and implements `core.StreamProvider` and native structured output. Other options: `WithServerURL` (gateways and test servers) and `WithHTTPClient`.

Notes:

- SDK-internal retries are disabled; failures surface as `*openrouter.APIError` (`StatusCode`, `Body`), and 408, 429, and 5xx are retryable through the definition's `Retry` policy.
- Model reasoning becomes a `core.ThinkingBlock` without a signature. Thinking and provider-raw blocks are dropped when sent.
- `ThinkingBudget` is ignored (OpenRouter uses reasoning effort levels). Temperature, max tokens, stop sequences, and tool choice are supported.
- Usage includes cached prompt tokens (`CacheReadTokens`) and cache writes (`CacheCreationTokens`).
- Images become `image_url` parts; tool-result errors use an `error:` content prefix; non-text tool-result blocks degrade to placeholder text.
- Native structured output sends a `json_schema` response format without the strict flag, because strict subsets differ across routed models. Core still validates the payload; a model that rejects schema enforcement fails the call, so use `Native: false` for it.
- Unknown or error finish reasons never map to a successful stop reason.

Do not switch an active provider-native transcript between providers casually: a conversation pins one definition revision. Prefer child agents per provider with typed handoffs.

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

The runtime streams from providers that implement `core.StreamProvider`, even for `Runtime.Run`; with only `core.Provider`, live views receive whole-turn events. Committed history is the same either way.

## SQLite Store

```bash
go get github.com/emotional-data8482/automata/extensions/sqlite@latest
```

```go
store, err := sqlite.Open(ctx, "automata.db")
rt, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
```

WAL with `synchronous=FULL`, one connection, and an exclusive OS lock (`ErrOwned` for a second owner; Unix only). Process-crash recovery is tested; power loss and network filesystems are not supported. Back up with the runtime closed, or with `VACUUM INTO` from another connection.

## Retry Policy

Provider invocations within one turn use the definition's retry policy:

```go
agent, err := core.New(provider, core.AgentConfig{Retry: &retry.Config{
    MaxAttempts:  4,
    InitialDelay: 500 * time.Millisecond,
    MaxDelay:     8 * time.Second,
    Multiplier:   2,
    RetryUnknown: false,
}})
```

Retries within a turn share one durable attempt record. After a crash during a provider call, the attempt is never resent silently: the run needs attention unless `RuntimeConfig.ProviderRecovery.MaxFreshAttempts` authorizes a counted fresh attempt.

The default is four attempts with exponential backoff and jitter. By default, only errors implementing:

```go
type Retryable interface { Retryable() bool }
```

are retried. Context cancellation/deadlines never retry. Leave `RetryUnknown` false unless replaying every unknown provider failure is known to be safe.

Tools own a separate retry decision:

```go
tools := []core.Tool{core.WithToolRetry(idempotentTool, retry.DefaultConfig())}
```

Do not retry non-idempotent operations this way; the runtime rejects a retried child tool.

## First-Party Tools Module

```bash
go get github.com/emotional-data8482/automata/tools@latest
```

### Sandboxed File Access

```go
core.AgentConfig{Tools: []core.Tool{tools.ReadFile(workspaceRoot), tools.WriteFile(workspaceRoot)}}
```

- `ReadFile` exposes `read_file`, uses `os.Root` to reject traversal/symlink escapes, and truncates after 256 KiB.
- `WriteFile` exposes `write_file`, uses `os.Root`, creates parent directories, and replaces complete file content. It is a mutating binding: it reports a content-digest receipt, and its semantic guard (scoped to the sandbox root) rejects a later write of a path whose earlier write is applied or unresolved, across every run on the store.
- Give read and write tools only to roles that need them. An isolated root limits filesystem scope but does not provide per-file authorization, version preconditions, conflict detection, audit persistence, or rollback.

### AGENTS.md Instructions

```go
instructions, err := tools.LoadAgentsMD(workspaceRoot, tools.AgentsMDOptions{Dir: "pkg/sub"})
core.AgentConfig{SystemPrompt: basePrompt + "\n\n" + instructions}
```

- `LoadAgentsMD` is a construction-time loader, not a tool. It reads each `AGENTS.md` from the root down to `Dir` through `os.Root`, shallowest first (closest instructions last), and heads each with its root-relative path.
- Missing, blank, or non-regular files are skipped, and no files yields `""`. A `Dir` outside the root, a missing `Dir`, a symlink escape, or combined content over `MaxBytes` (default 32 KiB) is an error.
- The result is frozen with the agent. Reload and register a new revision to pick up edits; existing conversations keep their original system message.

### Allow-Listed Shell

```go
shell := tools.Shell(tools.ShellConfig{
    Allow:   []string{"go", "git"},
    Dir:     workspaceRoot,
    Timeout: 30 * time.Second,
})
```

- Empty `Allow` denies every command.
- Program names must match exactly.
- Commands execute as argv via `exec.CommandContext`; pipes, redirections, globs, variable expansion, and command substitution do not run.
- Combined output is capped at 64 KiB.
- A tool-local timeout is a recoverable error; logical cancellation of the run aborts it.

An allow-listed binary may still expose dangerous flags (`git`, interpreters, package managers, and build tools can mutate broadly or execute subprocesses). Validate argument policy in an application-owned tool when exact program allow-listing is insufficient.

### HTTP Fetch

```go
fetch := tools.HTTPFetch()
```

`http_fetch` accepts HTTP(S), extracts readable HTML or returns text/JSON/XML, times out after 30 seconds, and caps responses at 512 KiB. It does **not** prevent SSRF; production apps must enforce destination/redirect/DNS/egress policy with an egress proxy, a durable approval (`core.WithDurableWait`), or a custom tool.

### Vendor-Neutral Web Search with Tavily

```bash
go get github.com/emotional-data8482/automata/tools@latest \
  github.com/emotional-data8482/automata/extensions/tavily@latest
```

```go
search := tavily.New(os.Getenv("TAVILY_API_KEY"))
search.Depth = "advanced" // optional; default is "basic"
webSearch := tools.WebSearch(search)
```

`tools.WebSearch` accepts any implementation of:

```go
type Searcher interface {
    Search(ctx context.Context, query string, max int) ([]tools.Result, error)
}
```

Tavily defaults to a 30-second client and asks for raw results rather than a synthesized answer. The model remains responsible for synthesis and citation.
