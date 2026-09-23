# Application Patterns

The repository's `core/example_test.go` holds compiled, output-checked versions of these patterns, and `examples/durable_host` shows a multi-process host.

## Durable Service Integration

A web or queue service normally:

1. At startup: opens the SQLite store, builds immutable agents, registers every revision stored runs may pin, and calls `Recover`.
2. Per request: `Submit` with `WithIdempotencyKey(tenant, externalID)` and a request-independent `WithDeadline`, stores the run ID next to its own record, and returns immediately or awaits with the request context (cancellation only detaches).
3. Reports progress from `Snapshot` or from committed events read at a saved cursor.
4. Exposes operator actions for waits (`ResolveWait`), uncertain effects (`Reconcile`), provider attention, hook attention (`AcknowledgeHooks`), and `Cancel`.
5. Runs `Prune` on a schedule with explicit retention ages.

One process owns one SQLite store (enforced by an OS lock); scale out with separately owned stores.

## Typed Final Results

```go
type Decision struct {
    Action     string   `json:"action" desc:"one of approve, revise, reject"`
    Reasons    []string `json:"reasons" desc:"evidence-based reasons"`
    Confidence float64  `json:"confidence" desc:"value from 0 to 1"`
}

agent, err := core.New(provider, core.AgentConfig{
    StructuredOutput: &core.StructuredOutputConfig{
        Schema:         core.OutputSchema[Decision](),
        MaxCorrections: 1,
        Native:         true, // provider-native schema enforcement when supported
    },
})
// ...
result, err := rt.Run(ctx, reviewerRef, task)
decision, err := core.Decode[Decision](result)
```

Invalid payloads are corrected inside the same run within `MaxCorrections` and `MaxTurns`; accepted tool effects are never repeated to fix formatting. Without native support the model answers through a hidden `automata_structured_output` tool, or with JSON in prose, which is extracted and validated. Extended thinking is disabled only on a forced final structured-output turn.

## Conversations

```go
thread := core.ConversationRef{Scope: tenantID, ID: conversationID}
first, err := rt.Run(ctx, chatRef, "Draft a plan", core.WithConversation(thread, ""))
next, err := rt.Run(ctx, chatRef, "Revise it", core.WithConversation(thread, first.RunID))
```

Turns are serialized: one active turn per conversation (`ErrConversationBusy`), each naming the committed head it continues (`ErrConversationConflict` when stale). The first turn pins the definition revision. `rt.Conversation(ctx, thread)` returns the head, the active run, and the turn count, which also recovers a turn whose admission response was lost.

## Child Agents

```go
type ResearchRequest struct {
    Topic     string   `json:"topic" desc:"bounded subtopic"`
    Questions []string `json:"questions,omitempty" desc:"specific questions"`
}

researcher, err := core.New(researchProvider, core.AgentConfig{
    SystemPrompt: `Assignments arrive as JSON {"topic", "questions"}. Research only that and cite sources.`,
    Tools:        []core.Tool{searchTool},
    MaxTurns:     8,
})
researcherRef, err := rt.Register("researcher", "v1", researcher)

lead, err := core.New(leadProvider, core.AgentConfig{
    Tools: []core.Tool{core.ChildTool[ResearchRequest]("researcher",
        "Delegate one focused research assignment.", researcherRef)},
    ToolPolicy: core.ToolPolicy{MaxCalls: 40}, // shared by the whole tree
})
leadRef, err := rt.Register("lead", "v1", lead)
```

Each call is a durable child run linked to the parent's call; the parent's worker is released while children run, and completed children are never recreated after a restart. The parent receives the child's structured output (JSON) or final message. Children share the parent's caps, deadline, and cancellation and are never retried. A tool that runs another agent itself is an opaque host tool outside these guarantees; use `ChildTool`.

## Streaming and UI/SSE State

```go
var acc core.StreamAccumulator
result, err := rt.RunStream(ctx, leadRef, task, func(event core.StreamEvent) {
    acc.Add(event)
    notifyRenderer() // keep callback work small
})
renderFinal(result, acc.Views(), acc.Totals())
```

| Kind | Important fields |
| --- | --- |
| `core.StreamText` / `StreamThinking` | `Text` delta |
| `core.StreamUsage` | per-turn `Usage` |
| `core.StreamToolCall` | assembled `ToolCall` before execution |
| `core.StreamToolResult` | `ToolCall`, `Result`, `ResultBlocks`, `IsError`, `Err` |

Child-run events carry `Agent` (the child tool name) and `InvocationID` (the call that started the child); nested children keep the innermost tags. `StreamAccumulator.Views()` lists the observed run first, then child invocations in first-seen order. Live views are bounded and provisional: they drop events rather than slow the run. For reliable delivery (webhooks, queues, another process), read committed events:

```go
page, err := handle.WaitEvents(ctx, cursor, 256) // resume from a persisted cursor
if errors.Is(err, core.ErrEventGap) { /* resynchronize from handle.Snapshot(ctx).EventSequence */ }
```

## Approvals and Questions

```go
refund := core.WithDurableWait(refundTool, core.DurableWaitPolicy{
    Kind:          core.WaitApproval,
    PolicyContext: "refunds-v2",
    ExpiresAfter:  24 * time.Hour,
    Prompt:        func(raw json.RawMessage) (string, error) { return describe(raw) },
    Target:        func(raw json.RawMessage) (string, error) { return orderID(raw) },
})
// Host, possibly another process after a restart:
snapshot, _ := handle.Snapshot(ctx)
wait := snapshot.Waits[0]
err = handle.ResolveWait(ctx, wait.ID, core.WaitResolution{
    Decision: core.Allow, Actor: authenticatedUser, ActionDigest: wait.ActionDigest,
})
```

The run holds no worker while waiting. `RuntimeConfig.Authorizer` checks current authority when the approval is accepted and again immediately before dispatch; the actor string is audit context, not a credential. Denials are returned to the model; a changed action needs a new approval. `core.WaitQuestion` waits take a JSON `Answer`, which becomes the tool result.

## Side Effects and Reconciliation

```go
publish := core.WithToolEffectPolicy(core.FuncResult("publish", "Publish a report.",
    func(ctx context.Context, in PublishInput) (core.ToolResult, error) {
        op, _ := core.ToolOperationFromContext(ctx)
        receipt, err := cms.Publish(ctx, in, op.IdempotencyKey)
        if err != nil {
            r := core.ErrorResult("publish failed: " + err.Error())
            r.Effect = core.EffectReport{Status: core.EffectUnknown}
            return r, nil
        }
        r := core.TextResult("published " + receipt)
        r.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: receipt}
        return r, nil
    }),
    core.ToolEffectPolicy{Kind: core.ToolEffectMutating, Scope: "cms", SemanticKey: pathOf})
```

After a crash between dispatch and the committed outcome, the invocation is `ToolInvocationUncertain` and the run needs attention. Ask the destination by the operation's idempotency key, then `handle.Reconcile(ctx, operationID, core.EffectResolution{Result: ..., Effect: ...})`; the run continues without re-executing the tool.

## Context Management

```go
summarizer := claude.New(summaryModel, apiKey)
agent, err := core.New(provider, core.AgentConfig{PreSendHooks: []core.PreSendHook{
    core.Compactor(summarizer, core.CompactorConfig{TriggerTokens: 100_000, KeepRecent: 8, MinRecompute: 8}),
}})
```

Compaction changes only the provider-facing view, keeps system and recent messages, never splits a tool call from its result, and memoizes summaries. A summarization failure fails the turn.

## Testing Without Live Models

Implement a scripted `core.Provider` that returns `core.Response{Message: core.AssistantMessage(...), StopReason: core.StopToolUse or StopEndTurn}` and run agents through `core.NewEphemeralRuntime()`. Deterministic providers that decide from the request transcript (not a call counter) keep working across restarts in multi-process tests.

Cover normal completion and typed decoding; tool request, result, and final turn; concurrent tool calls; recoverable tool errors and unknown tools; cancellation with `RunHandle.Cancel` and deadlines with `WithDeadline`; `ErrMaxTurnsExceeded` with a populated transcript; token-limit or incomplete `CompletionError` with partial output; approvals and denials; and, with a SQLite store in a temp dir, restart and reconciliation paths.
