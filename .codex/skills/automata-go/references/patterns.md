# Application Patterns

## Typed Final Results

Use typed final output whenever Go code consumes the answer.

```go
type Decision struct {
    Action     string   `json:"action" desc:"one of approve, revise, reject"`
    Reasons    []string `json:"reasons" desc:"evidence-based reasons"`
    Confidence float64  `json:"confidence" desc:"value from 0 to 1"`
}

decision, result, err := core.RunTyped[Decision](ctx, agent, task)
```

Automata injects a hidden `automata_structured_output` tool whose schema derives from the type. Payloads are validated against the schema before being returned; invalid output gets one bounded correction turn, prose answers with embedded JSON are parsed before paying for a forced turn, and the forced-tool run remains the final backstop. Regular agent tools remain available.

For an ongoing conversation:

```go
session := agent.NewSession()
first, _, err := core.RunSessionTyped[Decision](ctx, session, "Review proposal A")
if err != nil {
    return err
}
next, result, err := core.RunSessionTyped[Decision](ctx, session, "Now compare proposal B")
if err != nil {
    return err
}
use(first, next, result)
```

Post-run hooks fire for each underlying run, including both the initial prose run and forced fallback when fallback is needed.

## Persistent Sessions

`Agent.Run` is one-shot. A `Session` carries the full canonical transcript across calls and commits partial progress even when a run fails.

```go
session := agent.NewSession()

checkpoint := core.WithPostRunHook(func(ctx context.Context, result core.RunResult, runErr error) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    data, err := json.Marshal(result.Messages)
    if err != nil {
        return err
    }
    return storeTranscript(ctx, data)
})

first, err := session.Run(ctx, "Draft a plan", checkpoint)
if err != nil {
    return err
}
second, err := session.Run(ctx, "Revise it using the new constraint", checkpoint)
if err != nil {
    return err
}
use(first, second)
```

Restore:

```go
var transcript []core.Message
if err := json.Unmarshal(data, &transcript); err != nil {
    return err
}
session = agent.ResumeSession(transcript)
```

Important semantics:

- `Session.Run`, `RunStream`, and `RunSessionTyped` serialize against one another.
- `Session.Messages()` returns a snapshot as of the last completed run.
- A resumed non-empty transcript is used verbatim; the current agent system prompt is not added again.
- Persistence is a completed-run checkpoint, not resumable execution inside a provider turn or tool call.
- Protect external actions with application-level idempotency and record uncertain outcomes explicitly.

## Streaming and UI/SSE State

Use `RunStream` for live output:

```go
var acc core.StreamAccumulator
result, err := agent.RunStream(ctx, task, func(event core.StreamEvent) {
    acc.Add(event)
    notifyRenderer() // keep callback work small
})

if err != nil {
    log.Printf("stream ended with partial result: %v", err)
}
renderFinal(result, acc.Views(), acc.Totals())
```

Event kinds:

| Kind | Important fields |
| --- | --- |
| `core.StreamText` | `Text` delta |
| `core.StreamThinking` | `Text` reasoning delta |
| `core.StreamUsage` | per-turn `Usage` |
| `core.StreamToolCall` | assembled `ToolCall` before execution |
| `core.StreamToolResult` | `ToolCall`, `Result`, `ResultBlocks`, `IsError`, `Err` |

Within one turn, text/thinking arrive first, then usage, all tool-call announcements in model order, then tool results in completion order. `Result` is the text compatibility view; rich tools also populate `ResultBlocks` with ordered `TextBlock`/`ImageBlock` content. Tool result handlers run concurrently, but callback delivery is serialized. The callback is on the critical path, so enqueue lightweight notifications rather than blocking on UI or network clients.

`StreamAccumulator` groups by `(Agent, InvocationID)`:

- `Agent == ""`, `InvocationID == ""`: top-level lane.
- Sub-agent events use the sub-agent tool name and starting tool-call ID.
- Repeated/concurrent calls to one sub-agent share `Agent` but have distinct `InvocationID` values.
- `Views()` returns top-level first and sub-agent invocations in first-seen order.
- `View(agent, invocationID)` selects one lane.
- `ViewsFor(agent)` selects every invocation of one named agent.
- `Totals()` includes all observed lanes and turns.

The accumulator is concurrency-safe and its snapshots are copies. The top-level `RunResult.Usage` does not include nested sub-agent usage; use streamed tagged usage/accumulator totals for the full hierarchy.

## Multi-Agent Orchestration

Use sub-agents to isolate role, provider, tools, context, or permissions—not merely to split a prompt.

```go
type researchParams struct {
    Topic     string   `json:"topic" desc:"bounded subtopic"`
    Questions []string `json:"questions,omitempty" desc:"specific questions"`
}

researcher := core.New(researchProvider).
    WithSystemPrompt("Research only the assigned topic and return evidence with source URLs.").
    WithMaxSteps(8)
researcher.RegisterTool(searchTool)

orchestrator := core.New(orchestratorProvider).
    WithSystemPrompt("Delegate focused research, reconcile evidence, and answer the user.").
    WithMaxSteps(16)

orchestrator.RegisterTool(core.AsToolFunc(
    researcher,
    "researcher",
    "Delegate one focused research assignment and receive evidence-backed notes.",
    func(p researchParams) string {
        return fmt.Sprintf("Research: %s\nQuestions:\n- %s", p.Topic, strings.Join(p.Questions, "\n- "))
    },
))
```

Generic type inference usually infers `researchParams` from the renderer. An explicit form is `core.AsToolFunc[researchParams](...)`.

Use `core.AsTool[P]` only when forwarding raw JSON as the sub-agent task is intentional. Each sub-agent invocation is a fresh independent run; it does not share a session with prior calls or the parent. Under `RunStream`, nested agents auto-stream into the parent.

Good specialist design:

- Give each role a narrow system prompt and least-privilege tool set.
- Put shared facts in typed assignments/results, not a shared mutable transcript.
- Use a single writer for a shared mutable resource; parallel readers are safer.
- Keep deterministic verification outside the model and feed evidence back as data.
- Budget nested runs explicitly; parent step/usage totals do not automatically impose a shared hierarchical budget.

## Approval and Side Effects

Use an approver to allow, deny, or rewrite a requested tool call before execution:

```go
agent.WithApprover(core.ApproverFunc(func(
    ctx context.Context,
    call core.ToolUseBlock,
    messages []core.Message,
) (core.Decision, error) {
    if call.Name != "send_email" {
        return core.Decision{Outcome: core.Allow}, nil
    }

    approvedArgs, ok := approvalStore.Lookup(call.ID, call.Input)
    if !ok {
        return core.Decision{Outcome: core.Deny, Reason: "operator approval required"}, nil
    }
    return core.Decision{Outcome: core.Modify, Args: approvedArgs}, nil
}))
```

For consequential actions, approval should bind to effective arguments, authenticated actor, policy version, target resource, and expiration. A blocking in-memory approver is not durable suspension; if a process can restart or approval can take a long time, model that lifecycle in application storage.

## Context Management

Add a compactor for long sessions or tool loops:

```go
summarizer := claude.New(summaryModel, apiKey)
agent.WithPreSendHook(core.Compactor(summarizer, core.CompactorConfig{
    TriggerTokens: 100_000,
    KeepRecent:    8,
    MinRecompute:  8,
}))
```

Compaction:

- Changes only the provider-facing view; canonical session history remains complete.
- Keeps leading system messages and recent messages.
- Avoids splitting a tool call from its result.
- Memoizes summaries and recomputes after `MinRecompute` additional messages.
- Uses approximate token estimation from recent usage or serialized character count.
- Aborts the run if summarization fails.

Use a cheap, fast provider for summaries when appropriate. Compaction is not a replacement for domain-specific retrieval, bounded tool output, or application artifact storage.

## Service Integration

A safe HTTP/service handler normally:

1. Builds immutable agents during application startup.
2. Creates a request context with deadline/cancellation.
3. Loads or creates a session owned by an authenticated conversation ID.
4. Serializes operations for that conversation in application storage as well as in process.
5. Streams lightweight events into a bounded channel for SSE/WebSocket delivery.
6. Persists the completed transcript and result using a post-run hook.
7. Returns partial status and a stable operation ID on failure.

Do not hold API keys or authorization objects in prompts/transcripts. Do not let a slow/disconnected stream consumer block the run indefinitely; use bounded buffering and a defined backpressure/drop/cancel policy in application code.

## Testing Without Live Models

Test deterministic tools directly:

```go
func TestLookupToolRejectsMissingID(t *testing.T) {
    tool := newLookupTool(fakeStore{})
    _, err := tool.Execute(context.Background(), `{}`)
    if err == nil {
        t.Fatal("expected validation error")
    }
}
```

For agent-loop tests, implement a scripted `core.Provider` that records `core.Request` values and returns predetermined `core.Response` values. Construct assistant messages with `core.AssistantMessage`, `core.TextBlock`, and `core.ToolUseBlock`; attach a `core.Usage` to the message when testing accounting. Set `StopReason` explicitly to `core.StopToolUse` or `core.StopEndTurn`.

Cover:

- normal completion and typed decoding;
- tool request → result → final turn;
- concurrent tool calls and shared-state safety;
- recoverable tool error and unknown tool;
- context cancellation/deadline;
- provider retry classification;
- `ErrMaxStepsExceeded` with a populated transcript;
- token-limit/incomplete `CompletionError` with partial output;
- post-run checkpoint failure joined with run failure;
- session JSON round-trip;
- stream event order and nested lane attribution.
