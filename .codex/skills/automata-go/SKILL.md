---
name: automata-go
description: Build, extend, review, and troubleshoot agentic Go applications with github.com/emotional-data8482/automata. Use when creating tool-using agents, typed outputs, multi-agent orchestrators, persistent sessions, live streams, provider integrations, permissions, retries, or production-safe agent services in Go.
license: Apache-2.0
compatibility: "Automata v0.4.x; Go 1.26.2+; provider credentials are needed only for live runs"
metadata:
  package: github.com/emotional-data8482/automata
  version: "0.3.1"
---

# Build Agentic Go Apps with Automata

Use Automata as a set of composable primitives, not as a workflow framework. Keep business state, policy, persistence, and deterministic verification in application code; use `core.Agent`, `core.Tool`, `core.Provider`, sessions, and streams for model execution.

The API examples in this skill target Automata v0.4.x. Inspect the application's `go.mod` before changing code and keep the separately versioned Automata modules on compatible releases.

## Workflow

1. **Inspect the application first**
   - Read `go.mod`, existing provider wiring, tools, prompts, run entry points, and tests.
   - Preserve the application's package layout and dependency-injection style.
   - Determine whether the task needs a one-shot run, session, typed result, stream, or orchestrator. Do not introduce multi-agent complexity unless distinct roles or permissions justify it.

2. **Add only the modules in use**
   - Core: `github.com/emotional-data8482/automata/core`
   - Anthropic: `github.com/emotional-data8482/automata/extensions/claude`
   - OpenAI-compatible Chat Completions: `github.com/emotional-data8482/automata/extensions/openai`
   - First-party tools: `github.com/emotional-data8482/automata/tools`
   - Tavily search backend: `github.com/emotional-data8482/automata/extensions/tavily`
   - Pin separate modules to the same known-compatible release. For this skill's API, use `@v0.4.0`; otherwise honor the versions already selected by the application.

3. **Design boundaries before prompts**
   - Give each agent one explicit role and only the tools it needs.
   - Put deterministic work in Go tools, not in prompt instructions.
   - Use typed tool parameters and typed final results at model/application boundaries.
   - Use rich tool results only when block content (especially images) materially improves the model's next step; otherwise prefer plain text tool results.
   - Treat model-provided tool arguments, URLs, paths, commands, and generated content as untrusted input.
   - Gate consequential calls with `core.Approver`; also enforce validation and authorization inside tools.

4. **Build and configure once**
   - Construct the provider, then `core.New(provider)`.
   - Apply system prompt, step budget, call options, logger/tracer, approver, and hooks.
   - Register tools before any runs start. Agent configuration methods mutate in place and are not safe concurrently with runs.

5. **Choose the narrowest execution API**
   - `agent.Run`: independent one-shot task.
   - `agent.RunStream`: one-shot task with live events.
   - `agent.NewSession` / `ResumeSession`: multi-turn transcript.
   - `core.RunTyped[T]`: one-shot structured final answer.
   - `core.RunSessionTyped[T]`: structured answer in a persistent conversation.
   - `agent.RunBackground`: goroutine-backed one-shot run; cancellation still comes from `ctx`.

6. **Handle partial outcomes**
   - Always inspect the returned `core.RunResult` even when `err != nil`; output, transcript, usage, and stop reason are populated as far as execution got.
   - Bound runs with a context timeout and a deliberate `WithMaxSteps` value.
   - Persist session transcripts at completed-run boundaries with `core.WithPostRunHook` when durability matters.

7. **Verify without spending model tokens**
   - Unit-test tools directly and test orchestration with a fake `core.Provider` or `core.StreamProvider`.
   - Run `gofmt` on changed Go files, then `go test ./...`; run `go vet ./...` when the project uses it.
   - Use `go test -race ./...` for tools or services with concurrent state. Automata executes multiple tool calls from one model turn concurrently.

## Minimal Claude Agent

For a new v0.4.x module:

```bash
go get github.com/emotional-data8482/automata@v0.4.0 \
  github.com/emotional-data8482/automata/extensions/claude@v0.4.0
```

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

    provider := claude.New("claude-sonnet-4-6", os.Getenv("ANTHROPIC_API_KEY"))
    agent := core.New(provider).
        WithSystemPrompt("You are a concise assistant. Use tools when they improve accuracy.").
        WithMaxSteps(6)

    agent.RegisterTool(core.Func("current_time", "Get the current time in a timezone.",
        func(_ context.Context, args timeArgs) (string, error) {
            loc, err := time.LoadLocation(args.Timezone)
            if err != nil {
                return "", fmt.Errorf("invalid timezone %q: %w", args.Timezone, err)
            }
            return time.Now().In(loc).Format(time.RFC3339), nil
        }))

    result, err := agent.Run(ctx, "What time is it in Tokyo?")
    if err != nil {
        log.Printf("run stopped (%s): %v", result.StopReason, err)
    }
    fmt.Println(result.Output)
}
```

## Tool Rules

Define typed text tools with `core.Func`. Exported struct fields become JSON Schema properties; `json` controls names, `desc` supplies model-facing descriptions, and `omitempty` makes a field optional.

```go
type lookupArgs struct {
    ID      string   `json:"id" desc:"record identifier"`
    Fields []string `json:"fields,omitempty" desc:"optional fields to return"`
}

agent.RegisterTool(core.Func("lookup_record", "Look up one record by ID.",
    func(ctx context.Context, args lookupArgs) (string, error) {
        // Validate authorization and arguments here, then perform bounded I/O.
        return lookup(ctx, args.ID, args.Fields)
    }))
```

For rich tool outputs (mixed text/images), use `core.FuncResult`; it derives the same input schema as `core.Func`, but the handler returns `core.ToolResult`. Build results with `core.TextResult`, `core.BlockResult`, `core.ImageResult`, `core.URLImageResult`, or `core.ErrorResult`.

```go
type screenshotArgs struct {
    Target string `json:"target" desc:"what to capture"`
}

agent.RegisterTool(core.FuncResult("screenshot", "Capture a screenshot for visual inspection.",
    func(ctx context.Context, args screenshotArgs) (core.ToolResult, error) {
        png, err := capture(ctx, args.Target)
        if err != nil {
            return core.ToolResult{}, err // recoverable unless it is context cancellation/deadline
        }
        return core.BlockResult(
            core.TextBlock{Text: "captured " + args.Target},
            core.ImageBlock{MediaType: "image/png", Data: png},
        ), nil
    }))
```

Follow these semantics:

- Ordinary tool errors are recoverable: Automata records an error tool result and lets the model adapt.
- Returning `context.Canceled` or `context.DeadlineExceeded` aborts the run.
- Tool calls in one assistant turn run concurrently. Protect shared state and do not depend on completion order.
- Plain tools are not retried automatically. Wrap only idempotent/transient operations with `core.WithToolRetry`; the wrapper preserves `core.ResultTool` rich blocks.
- Do not wrap `core.AsTool` sub-agents in `WithToolRetry`; it can replay an entire sub-run and duplicate effects/events.
- Tool descriptions should state when to call the tool, what its arguments mean, and what the result represents. Keep outputs bounded.
- Rich result block order is preserved in the transcript. `ToolResult.Text()` concatenates text blocks and ignores images; text-only providers/consumers see that compatibility view.
- Keep inline image results small because image bytes are base64-encoded into the transcript and may be sent to supporting providers verbatim.

## Architecture Defaults

- Prefer one agent plus deterministic tools first.
- Use a `Session` only when later tasks require prior turns.
- Use `RunTyped` for decisions or artifacts consumed by Go; do not parse prose with regexes.
- Use `AsToolFunc` rather than `AsTool` when a sub-agent should receive a natural-language assignment instead of raw JSON.
- Use separate specialist sessions and typed handoffs across provider boundaries; do not assume provider-native thinking/raw blocks are portable.
- Use `StreamAccumulator` for UI/SSE state instead of manually joining deltas; inspect `ToolCallView.ResultBlocks` when rendering rich tool results.
- Keep stream callbacks fast; event delivery is serialized and lies on the run's critical path.
- Use `WithPostRunHook` for checkpointing after the session transcript is committed, not as an in-flight resume mechanism.

## Production Safety Checklist

Before finishing a production-facing app, verify:

- Context deadline and step/token budgets are explicit.
- Provider, tool, and post-run errors are surfaced while retaining partial `RunResult` data.
- API keys come from secret configuration and are never placed in prompts, tool schemas, transcripts, or logs.
- Every side-effecting tool validates scope and arguments; approval is used where human/policy review is required.
- Filesystem tools have an isolated root; shell commands use exact argv allow-lists; network tools enforce egress policy.
- Tool side effects are idempotent or carry application-level operation keys where replay is possible.
- Rich tool outputs are bounded, sanitized, and appropriate for the active provider; do not place secrets or excessive binary payloads in `ToolResult` blocks.
- Session transcripts are persisted as `[]core.Message` JSON and restored with the same provider semantics.
- Shared mutable tool state is concurrency-safe.
- Tests use fake providers and include cancellation, tool failure, max-step, and partial-result paths.

## References

Load only the reference needed for the task:

- [Core API and execution semantics](references/core-api.md)
- [Providers and first-party tools](references/providers-and-tools.md)
- [Sessions, streaming, typed output, multi-agent, and reliability patterns](references/patterns.md)
