# Automata v0.4.0 Core API

## Agent Construction

```go
agent := core.New(provider).
    WithSystemPrompt(systemPrompt).
    WithMaxSteps(10).
    WithDefaultCallOptions(core.CallOptions{MaxTokens: 4096}).
    WithLogger(logger).
    WithTracer(tracer).
    WithApprover(approver).
    WithRetry(retryCfg).
    WithPreSendHook(hook)

agent.RegisterTool(tool)         // add, or replace a same-named tool
agent.WithTools(toolA, toolB)    // replace the complete tool set
```

`core.New` defaults to:

- `core.DefaultMaxSteps` (10)
- `slog.Default()`
- `tracing.Noop`
- `core.AllowAll`
- `retry.DefaultConfig()` for provider invocations

Configure the agent completely before running it. `Run` and `RunStream` are safe to call concurrently after configuration because each creates its own loop. Builder methods and tool registration mutate the agent and must not race with runs.

## Execution APIs

```go
result, err := agent.Run(ctx, task, runOptions...)
result, err := agent.RunStream(ctx, task, onEvent, runOptions...)
ch := agent.RunBackground(ctx, task, runOptions...)
```

A background channel receives one `core.BackgroundResult` and then closes.

`core.RunResult` contains:

| Field | Meaning |
| --- | --- |
| `Output` | Final assistant text, or partial final text for some completion failures |
| `FinalMessage` | Last assistant message with all blocks |
| `Messages` | Complete transcript produced so far |
| `Usage` | Provider usage summed across this run's turns; excludes sub-agent usage |
| `Steps` | Provider turns taken |
| `StopReason` | Provider-neutral terminal reason |
| `RawStopReason` | Verbatim provider terminal reason |

Never discard `result` because `err` is non-nil.

## Per-Call Options

```go
temperature := 0.2
agent.WithDefaultCallOptions(core.CallOptions{
    Temperature:   &temperature,
    MaxTokens:     4096,
    StopSequences: []string{"<END>"},
    ThinkingBudget: 8_000,
})

result, err := agent.Run(ctx, task, core.WithCallOptions(core.CallOptions{
    MaxTokens: 2_000, // merged over agent defaults
    ToolChoice: &core.ToolChoice{
        Mode: core.ToolChoiceTool,
        Name: "lookup_record",
    },
}))
```

Tool-choice modes are `ToolChoiceAuto`, `ToolChoiceNone`, `ToolChoiceAny`, and `ToolChoiceTool`. Providers silently ignore options they cannot honor. A zero field in a per-run override preserves the agent default; for temperature, use a pointer so zero is expressible.

## Messages and Blocks

A `core.Message` has `Role`, `Blocks`, and optional `Usage`. Blocks are the source of truth; there is no separate tool-call field.

Concrete block types:

- `core.TextBlock`
- `core.ThinkingBlock` (including provider signature)
- `core.ToolUseBlock` (raw JSON input)
- `core.ToolResultBlock` (nested blocks and `IsError`)
- `core.ImageBlock` (inline bytes or URL)
- `core.RawBlock` (provider-specific escape hatch)

Helpers:

```go
user := core.UserMessage("hello")
system := core.SystemMessage("be concise")
assistant := core.AssistantMessage(core.TextBlock{Text: "hello"})
toolResult := core.ToolResultMessage(callID, "done", false)
richToolResult := core.ToolResultBlockMessage(callID, core.Blocks{
    core.TextBlock{Text: "screenshot:"},
    core.ImageBlock{MediaType: "image/png", Data: pngBytes},
}, false)

text := message.Text()
thinking := message.Thinking()
calls := message.ToolUses()
```

`[]core.Message` can be marshaled to JSON. Typed blocks retain their type discriminants, thinking signatures, tool error flags, images, and raw provider blocks.

## Typed Tool Schemas

```go
type address struct {
    City    string `json:"city" desc:"city name"`
    Country string `json:"country,omitempty" desc:"optional ISO country code"`
}

type searchArgs struct {
    Query     string            `json:"query" desc:"search terms"`
    Addresses []address         `json:"addresses,omitempty" desc:"optional geographic filters"`
    Labels    map[string]string `json:"labels,omitempty" desc:"metadata filters"`
}

tool := core.Func("search", "Search records.",
    func(ctx context.Context, args searchArgs) (string, error) {
        return search(ctx, args)
    })
```

Schema derivation supports primitives, pointers, nested structs, slices/arrays, string-keyed maps, `time.Time`, `[]byte`, interfaces, and `json.RawMessage`. Exported fields without `omitempty` are required. Use a struct (or `struct{}`) as the parameter type.

A custom text tool implements:

```go
type Tool interface {
    Name() string
    Schema() json.RawMessage
    Execute(ctx context.Context, args string) (string, error)
}
```

Prefer `core.Func` unless hand-authored schema or custom decoding is necessary.

For block-based rich outputs, use `core.FuncResult` or implement `core.ResultTool`:

```go
tool := core.FuncResult("screenshot", "Capture a screenshot.",
    func(ctx context.Context, args shotArgs) (core.ToolResult, error) {
        png, err := capture(ctx, args.Target)
        if err != nil {
            return core.ToolResult{}, err
        }
        return core.BlockResult(
            core.TextBlock{Text: "captured " + args.Target},
            core.ImageBlock{MediaType: "image/png", Data: png},
        ), nil
    })
```

`core.ToolResult` constructors are `TextResult`, `BlockResult`, `ErrorResult`, `ImageResult`, and `URLImageResult`. `ToolResult.Text()` concatenates text blocks as the compatibility view for string-only consumers and providers. A zero `ToolResult` normalizes to one empty text block before recording.

## Tool Execution Semantics

- The model may request several calls in one turn; Automata announces them in model order and executes them concurrently.
- Transcript tool-result messages stay in model call order even if execution finishes in another order.
- Unknown tools, denials, and ordinary execution errors become recoverable error results for the model.
- String tools become single-text-block tool results. Rich-result tools preserve block order in `ToolResultBlock.Content`.
- Context cancellation/deadline from a tool aborts the batch and run. Sibling calls are canceled cooperatively and still receive transcript results.
- An external side effect can complete while cancellation races its return. A canceled transcript result is not proof of rollback.
- Provider retries use `Agent.WithRetry`; normal tool execution does not. Use `core.WithToolRetry` only for idempotent tools with retry-classifiable failures. It preserves `ResultTool.ExecuteResult` for rich tools.

## Error Classification

Common run errors:

- `core.ErrInvalidMaxSteps`
- `core.ErrMaxStepsExceeded`
- `core.ErrEmptyResponse`
- `core.CompletionError`

Completion sentinels:

- `core.ErrTokenLimit`
- `core.ErrContentFiltered`
- `core.ErrIncompleteResponse`
- `core.ErrUnknownStopReason`
- `context.Canceled`

```go
result, err := agent.Run(ctx, task)
if err != nil {
    var completion *core.CompletionError
    switch {
    case errors.As(err, &completion):
        log.Printf("partial completion: reason=%s raw=%q", completion.Reason, completion.RawReason)
    case errors.Is(err, core.ErrMaxStepsExceeded):
        log.Printf("step budget exhausted after %d turns", result.Steps)
    case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
        log.Printf("run canceled")
    default:
        log.Printf("run failed: %v", err)
    }
}
// result.Messages and result.Usage remain available.
```

## Approval

```go
approver := core.ApproverFunc(func(
    ctx context.Context,
    call core.ToolUseBlock,
    history []core.Message,
) (core.Decision, error) {
    switch call.Name {
    case "delete_record":
        return core.Decision{Outcome: core.Deny, Reason: "deletion requires operator approval"}, nil
    default:
        return core.Decision{Outcome: core.Allow}, nil
    }
})
agent.WithApprover(approver)
```

Outcomes:

- `core.Allow`: execute unchanged.
- `core.Modify`: replace raw arguments with `Decision.Args` and execute.
- `core.Deny`: return `denied: <reason>` to the model; run continues.

An approver error aborts the run. Approval is a gate, not a replacement for tool-side authorization, validation, egress control, or idempotency.

## Hooks

A pre-send hook transforms the provider-facing snapshot once per turn without changing canonical history:

```go
type PreSendHook func(
    ctx context.Context,
    messages []core.Message,
    tools []core.Tool,
) ([]core.Message, []core.Tool, error)
```

Hooks run in registration order. An error aborts the run.

A post-run hook observes the fully populated result:

```go
checkpoint := core.WithPostRunHook(func(
    ctx context.Context,
    result core.RunResult,
    runErr error,
) error {
    return persist(result.Messages)
})
```

Post-run hooks run after a session commits its transcript, including on failed and canceled runs. Their context preserves values but has cancellation/deadline removed; impose a storage timeout inside the hook. All hooks run even if an earlier hook fails, and errors are joined with the original run error.
