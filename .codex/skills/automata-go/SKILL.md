---
name: automata-go
description: Build, extend, review, and troubleshoot agentic Go applications with github.com/emotional-data8482/automata. Use when creating tool-using agents, durable runs that survive restarts, typed outputs, child agents, conversations, approvals, live streams and tool progress, provider integrations, MCP tool servers, or production-safe agent services in Go.
license: Apache-2.0
compatibility: "Automata core v0.6.0+; SQLite v0.5.3+; HTTP and host v0.6.0+ when used; other modules are independently versioned; Go 1.26.2+; provider credentials are needed only for live runs"
metadata:
  package: github.com/emotional-data8482/automata
---

# Build Agentic Go Apps with Automata

Automata runs agents through one durable lifecycle. A `core.Agent` is an immutable definition (provider, prompt, tools, limits, output contract). A `core.Runtime` admits each task before running it, commits every step, and recovers after a restart without repeating completed work. Keep business state, authorization, and deterministic verification in application code.

This skill targets core v0.6.0 and the Runtime API, first released in v0.5.0. Direct `Agent.Run`, `Session`, `RunTyped`, `AsTool`, and `Approver` no longer exist; if an application uses them, migrate it with the table in the repository README ("Upgrading from v0.4"). Inspect `go.mod` first and keep the separately versioned Automata modules on compatible releases. Tool progress (`core.ReportToolProgress`) and `extensions/mcp` require core v0.5.3 or later. HTTP and host are published at v0.6.0; use SQLite v0.5.3 or later for the cancellation/reopen fix. Module versions need not match. Before upgrading a persistent v0.5 store, stop the owner and back it up: core v0.6 upgrades Runtime storage from version 9 to 10, and rollback requires restoring the backup.

## Workflow

1. **Inspect the application first**
   - Read `go.mod`, provider wiring, tools, prompts, run entry points, and tests.
   - Decide whether runs must survive a restart. If so, use a persistent store (`extensions/sqlite`); otherwise `core.NewEphemeralRuntime()`.
   - Do not introduce child agents unless distinct roles, tools, or permissions justify them.

2. **Add only the modules in use**
   - Core: `github.com/emotional-data8482/automata/core` (no dependencies)
   - Persistent store: `github.com/emotional-data8482/automata/extensions/sqlite`
   - Anthropic: `.../extensions/claude`; OpenAI-compatible: `.../extensions/openai`; OpenRouter: `.../extensions/openrouter`
   - First-party tools: `.../tools`; Tavily search backend: `.../extensions/tavily`
   - MCP servers: `.../extensions/mcp` (`mcpclient` exposes a server's tools as `core.Tool`s; `mcpserver` serves `core.Tool`s to MCP clients)
   - HTTP/SSE API: `.../extensions/httpserver`; durable server entry point: `.../host` (published v0.6.0 dependencies, no local replacements)

3. **Design boundaries before prompts**
   - Give each agent one role and only the tools it needs.
   - Put deterministic work in Go tools, not in prompt instructions.
   - Use typed tool parameters (`core.Func`) and typed final output (`core.OutputSchema` + `core.Decode`) at model/application boundaries.
   - Treat model-provided arguments, URLs, paths, and commands as untrusted input.
   - Declare side effects (`core.WithToolEffectPolicy`) and gate consequential calls with durable approvals (`core.WithDurableWait` + `RuntimeConfig.Authorizer`); still authorize inside tools.

4. **Construct once, register revisions**
   - Build each `core.Agent` with `core.New(provider, core.AgentConfig{...})`; configuration is frozen at construction.
   - Open one `Runtime` per process and store, `Register(id, revision, agent)` every definition, then `Recover(ctx)`.
   - Change behavior by registering a new revision; stored runs keep the revision they were admitted with.

5. **Run through the runtime**
   - `rt.Run(ctx, ref, task, opts...)`: admit and await.
   - `rt.RunStream(ctx, ref, task, onEvent, opts...)`: the same with a live view.
   - `rt.Submit(...)` + `handle.Await/Snapshot/Events/Cancel`: long-running or host-driven work.
   - Options: `core.WithIdempotencyKey(scope, key)` for external task IDs, `core.WithDeadline(t)`, `core.WithConversation(ref, head)` for multi-turn chats, `core.WithLabels(labels)` for host annotations.
   - Use `Runtime.Runs` for listing, `Changes`/`WaitChanges` for a committed global change feed, and `Agent.Describe`/`Runtime.Describe` for definition manifests.

6. **Handle partial and uncertain outcomes**
   - Always inspect `core.RunResult` even when `err != nil`; transcript, usage, and turns are populated as far as the run got.
   - A run that needs attention (`core.ErrRunNeedsAttention`) is waiting for the host: reconcile an uncertain tool call, authorize a fresh provider attempt, acknowledge hooks, or cancel. See the recovery runbook in `docs/durable-runtime.md`.

7. **Verify without spending model tokens**
   - Test tools directly, and use `core/coretest` for scripted providers, recorded requests, and Runtime fixtures. For restart-sensitive responses, use a transcript-driven provider rather than a call counter.
   - Run `gofmt`, `go test ./...`, and `go test -race ./...` for concurrent tool state; tool calls in one model turn run concurrently.

## Minimal Claude Agent

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "time"

    "github.com/emotional-data8482/automata/core"
    "github.com/emotional-data8482/automata/extensions/claude"
)

type timeArgs struct {
    Timezone string `json:"timezone" desc:"IANA timezone such as Asia/Tokyo"`
}

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer cancel()

    agent, err := core.New(claude.New("claude-sonnet-4-6", os.Getenv("ANTHROPIC_API_KEY")), core.AgentConfig{
        SystemPrompt: "You are a concise assistant. Use tools when they improve accuracy.",
        MaxTurns:     6,
        Tools: []core.Tool{core.Func("current_time", "Get the current time in a timezone.",
            func(_ context.Context, args timeArgs) (string, error) {
                loc, err := time.LoadLocation(args.Timezone)
                if err != nil {
                    return "", fmt.Errorf("invalid timezone %q: %w", args.Timezone, err)
                }
                return time.Now().In(loc).Format(time.RFC3339), nil
            })},
    })
    if err != nil {
        log.Fatal(err)
    }

    runtime, err := core.NewEphemeralRuntime() // or core.NewRuntime with sqlite.Open for durability
    if err != nil {
        log.Fatal(err)
    }
    defer runtime.Close()
    assistant, err := runtime.Register("assistant", "v1", agent)
    if err != nil {
        log.Fatal(err)
    }

    result, err := runtime.Run(ctx, assistant, "What time is it in Tokyo?")
    if err != nil {
        log.Printf("run stopped after %d turns (%s): %v", result.Turns, result.StopReason, err)
    }
    fmt.Println(result.Output)
}
```

Note: `core.Func` treats a returned Go error as fatal to the run. Return `core.ErrorResult(...)` from `core.FuncResult` (or a message string with a nil error) when the model should see the failure and adapt.

## Tool Rules

- `core.Func` / `core.FuncResult` derive the JSON schema from a struct: `json` names fields, `desc` describes them, `omitempty` makes them optional.
- Recoverable (model-visible): `core.ErrorResult`, unknown tool names, invalid arguments, budget denials, approval denials, policy timeouts.
- Fatal: a tool's Go error, logical cancellation (`RunHandle.Cancel`), the run deadline.
- Tool calls in one turn run concurrently; results commit in model order. Protect shared state.
- Tools are never retried implicitly. Wrap only idempotent operations with `core.WithToolRetry`.
- Mutating tools: wrap with `core.WithToolEffectPolicy(tool, core.ToolEffectPolicy{Kind: core.ToolEffectMutating})` and report `EffectApplied` / `EffectNotApplied` / `EffectUnknown` on every return; send the `IdempotencyKey` of `core.ToolOperationFromContext(ctx)` to the destination.
- A crash between dispatch and the committed outcome leaves the call uncertain; the run needs attention until the host calls `handle.Reconcile` with the destination's authoritative answer. The tool is never re-executed.
- Long-running tools report provisional progress with `core.ReportToolProgress(ctx, core.ToolProgress{Progress: done, Total: total, Message: status})`. Reports reach live views as `StreamToolProgress` events before the call's result, are never persisted, and are dropped once `Execute` returns. Report at a rate a person could follow.
- MCP server tools (`mcpclient`) are ordinary tools: server `isError` results and JSON-RPC errors are recoverable, a broken connection is fatal, and nothing is retried. They arrive unclassified with no approval requirement; wrap them like local tools.

## Architecture Defaults

- One agent plus deterministic tools first.
- `core.ChildTool[P](name, description, childRef)` for delegation; the child receives its validated arguments as JSON, so describe the fields in its system prompt.
- `core.OutputSchema[T]()` + `core.Decode[T](result)` for anything Go consumes; never parse prose.
- Conversations (`core.WithConversation`) only when later turns need earlier ones.
- `core.StreamAccumulator` for UI/SSE state; child events arrive tagged by `Agent` and `InvocationID`, and each tool call's view keeps its latest `Progress`.
- Committed events (`handle.Events` from a saved cursor) for reliable delivery to other systems; live streams are provisional and may drop events.

## Production Safety Checklist

- Persistent store for anything that must survive a restart; `Recover` after registering every revision on startup.
- External task IDs mapped with `WithIdempotencyKey`; a retry reuses the same deadline value.
- Context deadlines on API calls, `MaxTurns`, and `ToolPolicy` caps (`MaxCalls` is shared by the whole run tree; `PerTool` adds per-tool budgets, timeouts, and rate limiters).
- Every side-effecting tool declares its effect policy and uses the operation idempotency key.
- Approvals bind the exact action (`ActionDigest`) and are authorized by `RuntimeConfig.Authorizer` against current policy.
- Filesystem tools use an isolated root, shell tools exact argv allow-lists, network tools egress policy.
- MCP clients expose only the server tools a role needs (`ToolOptions.Include`, with a `Prefix` per server) and declare effects or approvals on mutating ones. Tools served through `mcpserver` bypass Runtime policy (timeouts, budgets, approvals, effect guards), so export only tools safe to call directly and authenticate the transport.
- API keys come from secret configuration, never prompts, transcripts, or logs.
- An operator path exists for runs that need attention (reconcile, provider fresh attempts, hook acknowledgement, cancel) and for retention (`Runtime.Prune`).
- Tests use scripted providers and cover tool failure, cancellation, max turns, and partial results.

## References

Load only the reference needed for the task:

- [Core API and execution semantics](references/core-api.md)
- [Providers, first-party tools, and MCP](references/providers-and-tools.md)
- [Durability, streaming, typed output, children, approvals, and testing patterns](references/patterns.md)
