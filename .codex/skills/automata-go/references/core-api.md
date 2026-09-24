# Automata Core API

## Definitions

```go
agent, err := core.New(provider, core.AgentConfig{
    SystemPrompt: systemPrompt,
    Tools:        []core.Tool{toolA, toolB},
    MaxTurns:     10,                                   // zero selects core.DefaultMaxTurns
    CallOptions:  core.CallOptions{MaxTokens: 4096},    // sent on every provider turn
    ToolPolicy: core.ToolPolicy{
        Timeout: 30 * time.Second, MaxCalls: 50, MaxParallel: 4,
        PerTool: map[string]core.ToolLimits{"web_search": {MaxCalls: 10, RateLimiter: limiter}},
    },
    Retry:        &retryCfg,                            // provider retries within one turn
    Logger:       logger,                               // nil selects slog.Default
    Tracer:       tracer,                               // nil selects tracing.Noop
    PreSendHooks: []core.PreSendHook{compactor},
    StructuredOutput: &core.StructuredOutputConfig{Schema: core.OutputSchema[Answer](), MaxCorrections: 1},
})
```

`New` validates and freezes the configuration: later changes to the config value, tools, or schemas do not affect the agent. Tool names must be unique; `automata_structured_output` is reserved.

## Runtime

```go
store, err := sqlite.Open(ctx, "automata.db")         // or core.NewMemoryStore()
rt, err := core.NewRuntime(ctx, core.RuntimeConfig{
    Store:      store,
    Authorizer: core.ApprovalAuthorizerFunc(authorize), // required for approval waits
    Hooks:      []core.CommittedRunHook{auditHook},     // after the result commits
    ProviderRecovery: core.ProviderRecoveryPolicy{MaxFreshAttempts: 0},
    MaxPayloadBytes:  0,                                // zero selects core.DefaultMaxPayloadBytes
})
defer rt.Close()

ref, err := rt.Register("support", "2026-09-23", agent) // returns core.DefinitionRef
err = rt.Recover(ctx)                                   // after registering every stored revision

result, err := rt.Run(ctx, ref, task, opts...)
result, err := rt.RunStream(ctx, ref, task, onEvent, opts...)
handle, err := rt.Submit(ctx, ref, task, opts...)
handle := rt.Handle(runID)
snapshot, err := rt.Conversation(ctx, core.ConversationRef{Scope: tenant, ID: thread})
report, err := rt.Prune(ctx, core.RetentionPolicy{Events: 24 * time.Hour, History: 30 * 24 * time.Hour, Runs: 90 * 24 * time.Hour})
```

`core.NewEphemeralRuntime()` is `NewRuntime` on a fresh memory store. Storage failure never falls back to memory. An oversized durable payload (a turn, a tool batch, or a tool result over `MaxPayloadBytes`) is never truncated: the run needs attention instead.

`ToolPolicy` belongs to the definition: `Timeout` bounds each execution (including the limiter wait, not durable waits), `MaxCalls` and per-tool `MaxCalls` reserve budget in model order and are shared with child runs, and `RateLimiter` accepts anything with `Wait(ctx) error` (such as `*rate.Limiter`). An invalid policy fails `core.New` with `ErrInvalidToolPolicy`.

Submit options: `core.WithIdempotencyKey(scope, key)`, `core.WithDeadline(t)`, `core.WithConversation(ref, expectedHead)`. An exact retry of a keyed submission (same task, definition, deadline, conversation) returns the original run; a changed one returns `core.ErrAdmissionConflict`.

`RunHandle`: `ID`, `Await`, `Snapshot`, `Observe` (live view), `Events` / `WaitEvents` (committed cursor pages), `Cancel`, `ResolveWait`, `Reconcile`, `AcknowledgeHooks`. Canceling the context of any call only stops that call; `Cancel` and the deadline are the only logical cancellations.

## RunResult

| Field | Meaning |
| --- | --- |
| `RunID`, `Status` | Stable identity; `completed`, `failed`, `cancelled`, `limit_reached` |
| `Output`, `FinalMessage` | Final assistant text and message (blocks included) |
| `Messages` | Complete committed transcript (a conversation's full history) |
| `Usage` | This run's provider usage; child runs are in `RunSnapshot.Accounting.Tree` |
| `Turns`, `ProviderAttempts` | Provider turns (including corrections) and calls (including retries) |
| `StopReason`, `RawStopReason`, `ProviderStopReason` | Why the run ended; the provider's verbatim and neutral last reasons |
| `StructuredOutput` | The validated payload when the definition declares structured output |
| `Diagnostics` | Rejected provider content and tool execution errors |

Never discard `result` because `err` is non-nil.

## RunSnapshot

`State` (`ready`, `running`, `waiting`, `cancel_requested`, `finalizing`, `needs_attention`, `terminal`), `Definition`, `Parent`, `Conversation`, `Result`, `Failure` (`Message`, `Kind`, `StopReason`, `Violations`), `Attention` (`Kind`: `execution`, `provider`, `hooks`, `child`; `Reason`, such as `core.ErrToolEffectUncertain`'s message for an unreconciled call; `BlockingRunID`), `Accounting` (`UnknownAttempts`, `FreshAttempts`, `Tree`), `Hooks`, `ToolBatches` (per-invocation state, effect, `ChildRunID`), `Waits`, `EventSequence`, `HistoryPruned`.

## Messages and Blocks

A `core.Message` has `Role`, `Blocks`, and optional `Usage`. Block types: `TextBlock`, `ThinkingBlock` (with signature), `ToolUseBlock` (raw JSON input), `ToolResultBlock` (nested blocks and `IsError`), `ImageBlock` (inline bytes or URL), and `RawBlock` (provider-native escape hatch). Helpers: `UserMessage`, `SystemMessage`, `AssistantMessage`, `ToolResultMessage`, `ToolResultBlockMessage`, and `Message.Text` / `Thinking` / `ToolUses`. `[]core.Message` round-trips through JSON.

## Tools

```go
type Tool interface {
    Definition() core.ToolDefinition                  // Name, Description, InputSchema
    Execute(ctx context.Context, args json.RawMessage) (core.ToolResult, error)
}
```

Prefer `core.Func` (text) or `core.FuncResult` (rich blocks). Schema derivation supports primitives, pointers, nested structs, slices, string-keyed maps, `time.Time`, `[]byte`, interfaces, and `json.RawMessage`; exported fields without `omitempty` are required.

Wrappers: `core.WithToolRetry(tool, cfg)`, `core.WithToolEffectPolicy(tool, policy)`, `core.WithDurableWait(tool, policy)`, `core.WithLegacyToolErrors(tool)` (turns a custom tool's Go errors into model-visible error results). Child tools: `core.ChildTool[P](name, description, ref)` or `core.NewChildTool(definition, ref)`; they cannot be retried, wrapped in a wait, or given an effect policy.

Results: `TextResult`, `BlockResult`, `ErrorResult`, `ImageResult`, `URLImageResult`; set `ToolResult.Effect` (`EffectApplied` with a `Receipt`, `EffectNotApplied`, `EffectUnknown`, `EffectNone`) for mutating tools.

## Errors

Run errors (rebuilt from the persisted failure after `Await`, identically before and after a restart):

- `core.ErrMaxTurnsExceeded`, `core.ErrInvalidMaxTurns`, `core.ErrEmptyResponse`
- `core.ErrInvalidStructuredOutput` / `*core.InvalidStructuredOutputError` (with `Violations`)
- `*core.CompletionError` with `core.ErrTokenLimit`, `ErrContentFiltered`, `ErrIncompleteResponse`, `ErrUnknownStopReason`
- `context.Canceled` (logical cancellation), `context.DeadlineExceeded` (run deadline)
- `core.ErrRunNeedsAttention` from `Await` when the host must act

A tool's or provider's own error types keep only their message after persistence.

Runtime command errors: `ErrDefinitionNotRegistered`, `ErrDefinitionConflict`, `ErrAdmissionConflict`, `ErrRunNotFound`, `ErrRuntimeClosed`, `ErrRunPruned`, `ErrEventGap`, `ErrConversationNotFound`, `ErrConversationBusy`, `ErrConversationConflict`, `ErrConversationBlocked`, `ErrWaitNotFound`, `ErrWaitConflict`, `ErrWaitExpired`, `ErrWaitStale`, `ErrApprovalUnauthorized`, `ErrApprovalActionMismatch`, `ErrOperationNotFound`, `ErrReconciliationConflict`, `ErrPayloadTooLarge`, `ErrPayloadUnavailable`.

```go
result, err := rt.Run(ctx, ref, task)
switch {
case err == nil:
case errors.Is(err, core.ErrRunNeedsAttention):
    snapshot, _ := rt.Handle(result.RunID).Snapshot(ctx) // inspect Attention, ToolBatches, Waits
case errors.Is(err, core.ErrMaxTurnsExceeded):
    log.Printf("turn budget exhausted after %d turns", result.Turns)
case errors.Is(err, core.ErrIncompleteResponse):
    log.Printf("partial answer: %q", result.Output)
default:
    log.Printf("run failed: %v", err)
}
```

## Hooks

`core.PreSendHook func(ctx, core.Request) (core.Request, error)` transforms the provider-facing request each turn without changing committed history (`core.Compactor` is one). `core.CommittedRunHook{Name, Timeout, Handle}` runs after a run's result commits; its outcome is recorded in `RunSnapshot.Hooks` and never changes the result. An interrupted delivery needs `AcknowledgeHooks`.
