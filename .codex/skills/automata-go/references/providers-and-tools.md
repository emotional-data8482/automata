# Providers, First-Party Tools, and MCP

Automata keeps provider SDKs, the SQLite store, the MCP SDK, and tool integrations in separate Go modules. Add only what the application uses and keep module releases compatible. These modules are tagged independently: internal requirements are minimum compatible versions, not coordinated release numbers. The v0.6 server baseline is core v0.6.0, SQLite v0.5.3, HTTP v0.6.0, and host v0.6.0; MCP remains independently tagged at v0.5.3. Check a module's published version rather than assuming checkout-only changes are available via `@latest`.

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
go get github.com/emotional-data8482/automata@v0.6.0 \
  github.com/emotional-data8482/automata/extensions/sqlite@v0.5.3
```

```go
store, err := sqlite.Open(ctx, "automata.db")
rt, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
```

WAL with `synchronous=FULL`, one connection, and an exclusive OS lock (`ErrOwned` for a second owner; Unix only). Process-crash recovery is tested; power loss and network filesystems are not supported. Back up with the runtime closed, or with `VACUUM INTO` from another connection.

Use SQLite v0.5.3 or later for the transaction cancellation/reopen fix. Before opening an existing version-9 Runtime store with core v0.6, stop its owner and back up the database. `NewRuntime` upgrades it transactionally to version 10; older core releases refuse that store, so rollback requires restoring the backup.

## HTTP API and Host

```bash
go get github.com/emotional-data8482/automata/host@v0.6.0
```

Host v0.6.0 selects core v0.6.0, `extensions/httpserver` v0.6.0, and SQLite v0.5.3 without replacements. Use `host.Main(host.Options{Register: registerDefinitions})` for a standalone service, or `host.Run(ctx, options)` for an embedded lifecycle. It opens the store, registers definitions, recovers, runs retention, and drains HTTP and closes the Runtime on shutdown. Register every revision persisted runs may pin.

For an existing Runtime, use `httpserver.New(httpserver.Config{Runtime: rt, Authorize: authorize})` and serve the returned handler. Authorization is required; `Scope` enables tenant isolation and `Actor` supplies audit identity. Content is redacted unless `ExposeContent` is enabled. JSON/SSE routes expose admission, listing, status, committed events, waits, cancellation, reconciliation, hook acknowledgement, definitions, and a global change feed for single-tenant servers. The unauthenticated `/v1/openapi.json` route exposes the API schema, not run data.

Host defaults come from `AUTOMATA_ADDR`, `AUTOMATA_DB`, and `AUTOMATA_TOKEN`; it refuses startup without an authorization callback or bearer token. Middleware around streaming response writers must preserve write-deadline support (for example via `Unwrap() http.ResponseWriter`). HTTP streams carry committed events, not provisional tool-progress reports. See `host/README.md` and `extensions/httpserver/README.md` for the lifecycle and route contracts.

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
- `WriteFile` exposes `write_file`, uses `os.Root`, creates parent directories, and replaces complete file content. It is a mutating binding and reports a content-digest receipt. Published tools v0.5.2 has a permanent path semantic guard that rejects later writes after an applied or unresolved write. The current repository source removes that guard so new model calls can revise or restore content, and reports pre-write failures as `EffectNotApplied`; that change needs a separate tools release and is not delivered by upgrading core alone.
- Give read and write tools only to roles that need them. An isolated root limits filesystem scope but does not provide per-file authorization, version preconditions, conflict detection, audit persistence, or rollback.

### AGENTS.md Instructions

```go
instructions, err := tools.LoadAgentsMD(workspaceRoot, tools.AgentsMDOptions{Dir: "pkg/sub"})
core.AgentConfig{SystemPrompt: basePrompt + "\n\n" + instructions}
```

- `LoadAgentsMD` is a construction-time loader, not a tool. It reads each `AGENTS.md` from the root down to `Dir` through `os.Root`, shallowest first (closest instructions last), and heads each with its root-relative path.
- Missing, blank, or non-regular files are skipped, and no files yields `""`. A `Dir` outside the root, a missing `Dir`, a symlink escape, or combined content over `MaxBytes` (default 32 KiB) is an error.
- The result is frozen with the agent. Reload and register a new revision to pick up edits; existing conversations keep their original system message.

### Agent Skills

```go
skills, err := tools.LoadSkills(".agents/skills")
core.AgentConfig{SystemPrompt: basePrompt + "\n\n" + skills.Catalog(), Tools: []core.Tool{skills.Tool()}}
```

- `LoadSkills` treats each subdirectory with a `SKILL.md` as one skill. Every listed directory must exist. `name` (spec format, matching the directory) and `description` (at most 1024 characters) are required, and a name defined twice across the directories is an error.
- Frontmatter is a YAML subset: top-level plain, quoted, and `|`/`>` block scalars. Nested maps such as `metadata:` are skipped. `license`, `compatibility`, and `allowed-tools` are exposed on `Skill`; `allowed-tools` is not enforced.
- `Catalog()` returns names and descriptions for the system prompt (`""` when empty). `Tool()` is the read-only `load_skill` tool: `{name}` returns the `SKILL.md` body frozen at load, and `{name, path}` reads a file inside the skill directory through `os.Root` (256 KiB cap). Unknown names and bad paths are model-visible errors.
- Skill scripts are never executed; pair with an allow-listed `tools.Shell` if a skill needs them. Referenced files are read at call time, so their content is pinned by the committed tool result, not by the revision.

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

## MCP Servers

```bash
go get github.com/emotional-data8482/automata/extensions/mcp@latest
```

`extensions/mcp` wraps the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk/mcp`), which owns transports, framing, sessions, authentication, and capability negotiation. It requires core v0.5.3 or later (it uses tool progress).

### Using a Server's Tools

```go
client, err := mcpclient.Connect(ctx,
    &mcp.CommandTransport{Command: exec.Command("mcp-server-fetch")},
    mcpclient.Options{})
defer client.Close()
remote, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "fetch_"})
agent, err := core.New(provider, core.AgentConfig{Tools: remote})
```

- Transports come from the SDK: `mcp.CommandTransport` for a local subprocess over stdio, `mcp.StreamableClientTransport` for a remote server (its `HTTPClient` carries authentication), and `mcp.NewInMemoryTransports` for tests.
- `Tools` lists the server's tools in server order. `Prefix` is prepended to each advertised name so several servers cannot collide (calls use the server's own name); `Include` exposes only the named server tools, and naming one the server lacks is an error.
- The list is a snapshot frozen with the agent. A server that changes its tools needs a new agent and a new registered revision.
- Errors: an `isError` result and a server JSON-RPC error (unknown tool, invalid parameters, internal error) become recoverable error results. Cancellation is returned unchanged. A closed connection, expired session, network error, or transient HTTP status (429, 5xx) is a Go error and fatal to the run. Nothing is marked retryable, because a remote tool may have side effects.
- Schemas: each input schema is rewritten into core's supported subset (local `$ref`s inlined, nullable unions collapsed, `const` to a one-value enum, `allOf` merged). Assertions core cannot express are dropped from the local pre-check; the server stays the authoritative validator.
- Progress: every call requests progress notifications and republishes them with `core.ReportToolProgress`, so they reach live views as `StreamToolProgress` events. A notification after the call returns is dropped.
- Content: text and images map to blocks, embedded image resources become images, and structured content is rendered as JSON text when the server sends no content blocks. Audio, resource links, and other binary resources degrade to a bracketed placeholder line.
- Policy: remote tools are unclassified (`core.ToolEffectLegacy`) and need no approval. Wrap them with `core.WithToolEffectPolicy` or `core.WithDurableWait`; server annotations are hints, not a trust decision.
- `Options.ClientOptions` configures the SDK client (sampling and elicitation handlers, keepalive, logging). `client.Session()` returns the SDK session for resources, prompts, and completion. The client and its tools are safe for concurrent use.

### Serving Core Tools

```go
server := mcp.NewServer(&mcp.Implementation{Name: "files", Version: "v1"}, nil)
if err := mcpserver.AddTools(server, tools.ReadFile(root)); err != nil {
    return err
}
err := server.Run(ctx, &mcp.StdioTransport{})
```

- `AddTools` validates every tool before registering any: names must be non-empty and distinct, and each input schema must be an object schema. A same-named tool already on the server is replaced.
- Each call validates its arguments against the schema, as a Runtime does, and returns a violation as an `isError` result. The tool then executes directly, with **no** Runtime policy: no `ToolPolicy` timeout, call budget, or rate limit, no durable wait, and no effect guard or recovery. Export only tools that are safe to call that way, and authenticate the transport (for example, in the HTTP handler in front of the streamable handler).
- Results: `core.ErrorResult` becomes `isError` content, a Go error becomes a JSON-RPC internal error, and cancellation is returned unchanged. Text and inline images map to MCP content, URL images become resource links, other blocks degrade to a placeholder line, and effect reports are not sent.
- When the client supplies a progress token, the tool's `core.ReportToolProgress` reports are sent as progress notifications for that request, stopping when the tool returns.
