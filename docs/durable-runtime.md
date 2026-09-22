# Durable runtime lifecycle

`core.Runtime` is the durable execution lifecycle. It admits a task before
work, owns worker contexts independently from API/view contexts, and pins an
immutable `Agent` registration. `loopMachine` is its current internal turn
driver, not a second public lifecycle; it can be replaced in place as the
durable transition model grows.

Persistent construction is explicit:

```go
store, err := sqlite.Open(ctx, "automata.db")
if err != nil { return err }

runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
if err != nil { return err }
defer runtime.Close()

agent, err := core.New(provider, core.AgentConfig{
    SystemPrompt: "Be concise.",
})
if err != nil { return err }
if err := runtime.Register("assistant", "2026-09-15", agent); err != nil {
    return err
}

handle, err := runtime.Submit(ctx, "assistant", "2026-09-15", task,
    core.SubmitOptions{Scope: tenantID, Key: externalTaskID})
if err != nil { return err }

result, err := handle.Await(ctx)
```

Use `core.NewEphemeralRuntime` only when loss on process exit is intentional.
An unavailable or invalid persistent store fails construction or admission; the
runtime never silently substitutes memory.

## Identities and registration

`Agent` remains the immutable executable definition. `Register` binds it to an
application-controlled definition ID and revision. A persisted run pins both;
registering a different Agent under the same pair is rejected. The revision is
a host attestation, not proof of Go closure or binary identity. Hosts must
register the matching executable binding before `Recover`.

`SubmitOptions.Scope` plus `Key` is the external idempotency identity. An exact
retry returns the original `RunHandle`; changed task, definition, revision, or
deadline conflicts. Cancel, reconcile, and wait-resolution commands retain
historical durable outcomes, including after later run transitions.

## Three lifetimes

- A context passed to `Submit`, `Snapshot`, `Await`, `Observe`, `Run`, or
  `RunStream` bounds only that API operation or view. Once admission commits,
  disconnecting it does not cancel the logical run.
- `Runtime.Close` cancels its worker context and waits for execution to yield
  before closing storage. An interrupted whole-loop segment is conservatively
  marked `RuntimeNeedsAttention`; it is never automatically replayed.
- `RunHandle.Cancel` is the logical cancellation command. A persisted deadline
  in `SubmitOptions` has the same logical ownership. Both reach every required
  durable child (see [Durable children](#durable-children)).

`Runtime.Run` is `Submit` plus `Await`. `Runtime.RunStream` admits the same kind
of run and attaches a bounded live view; detaching the view does not own worker
or logical cancellation. `RunHandle.Observe` exposes that provisional view
separately. Durable cursors and replay are T08 work, so `Snapshot` is the
authoritative T02 observation.

## Committed hooks

`RuntimeConfig.Hooks` accepts named `CommittedRunHook`s for audit export,
notifications, metrics, and other application integrations. Runtime first
commits the execution result in `RuntimeFinalizing`, then invokes hooks in
registration order with detached snapshots and a per-hook timeout. Zero timeout
selects 30 seconds. As with tools, Go cannot forcibly stop a callback; hook
implementations must honor context cancellation.

Each attempt is recorded in `RunSnapshot.Hooks`. Hook errors and panics
remain visible there but never change `RunResult` or its execution error. After
the hook outcomes commit, the run becomes `RuntimeTerminal` and `Await`
returns. Runtime commits a delivery marker before invoking the first hook and
never retries a hook automatically. If the process stops in
`RuntimeFinalizing` after that marker, recovery reports `RuntimeNeedsAttention`
because an external effect may have happened without its outcome being stored.
If it stops before the marker, no hook ran, so recovery invokes the currently
configured hooks once and commits the terminal state.

A run in hook attention (`RunSnapshot.Attention.Kind == AttentionHooks`) also keeps a waiting
durable parent blocked or its conversation busy. After checking whatever the
interrupted hooks might have done, the host calls `handle.AcknowledgeHooks(ctx)`.
Runtime never invokes those hooks again. The run becomes terminal with its
committed result unchanged, and each hook whose delivery had started is recorded
in `Hooks` with `Unknown` set and a non-empty `Error`. The terminal commit
wakes the parent or advances the conversation as usual. An exact retry returns
the original outcome. `Cancel` remains available but records the run as
canceled.

## Storage and recovery

The provider-neutral `core.Store` contract is a small atomic transaction port.
Core owns bucket names and versioned encodings; adapters own database details.
`extensions/sqlite` is the supported T02 local candidate and keeps its driver out
of the root module. It enforces one local owner with an OS advisory lock.

On recovery, admitted-but-unstarted work can run after its exact binding is
registered. Accepted provider turns and durable tool batches resume from their
committed transcript. Each invocation has a stable operation ID, reservation,
dispatch state, outcome, and effect report. Completed siblings are not rerun;
reserved siblings may continue. A call found dispatched without a committed
outcome becomes `ToolInvocationUncertain`, keeps the run in
`RuntimeNeedsAttention`, and is never automatically replayed. The canonical
transcript receives the complete batch in model request order only after every
invocation has an authoritative outcome. A fatal batch outcome commits with
that history; recovery finalizes the failure rather than continuing the model.

Provider attempts have no per-attempt record. When recovery finds a run whose
stopped owner's next step was a provider call, it counts one
`RunSnapshot.Accounting.UnknownAttempts`: that attempt may have been sent, and its usage,
if any, is not in `RunResult.Usage`. Runtime never invents usage for it.

## Tool effects and reconciliation

`ToolResult.IsError`, a returned Go error, and external effect certainty are
separate signals. A normal Go error does not by itself mean that a mutation is
uncertain. Tools declare recovery behavior with `WithToolEffectPolicy`:

- `ToolEffectReadOnly` defaults a completed call to `EffectNone`.
- `ToolEffectMutating` requires every return path to provide an explicit
  `EffectApplied`, `EffectNotApplied`, or `EffectUnknown` report. An applied
  report may carry an opaque destination receipt, including when returned with
  a Go error or a recoverable policy timeout.
- Unwrapped legacy tools remain supported. Their completed calls are
  `EffectUnreported`; a crash after dispatch is still uncertain and cannot be
  retried automatically.

Runtime puts a stable `ToolOperation` in the execution context. Use
`ToolOperationFromContext` and pass its `IdempotencyKey` to destinations that
support idempotent requests. This reduces ambiguity but does not make a local
storage transaction atomic with an external service.

For application-level duplicate protection, a mutating policy may supply a
stable `Scope` and `SemanticKey`. Runtime derives the key from the validated,
model-requested arguments, reserves it in model order, and rejects a later
mutation while the first is applied or unresolved. T05 extends this binding to
approval-modified actions. The guard is a backstop, not proof that two arbitrary
payloads are semantically equivalent.

Inspect `RunSnapshot.ToolBatches` to find an uncertain operation. After checking
the destination authoritatively, call:

```go
err := handle.Reconcile(ctx, operationID, core.EffectResolution{
    Result: core.TextResult("verified existing write"),
    Effect: core.EffectReport{
        Status:  core.EffectApplied,
        Receipt: destinationReceipt,
    },
})
```

Reconciliation never executes the tool. It atomically records the model-facing
result, effect evidence, and command receipt, then resumes remaining reserved
siblings and the run. An exact retry returns the original outcome; a changed
resolution returns `ErrReconciliationConflict`. Cancellation remains terminal
when a tool reports uncertainty: late reconciliation retains the evidence but
never resumes the canceled run.

`tools.ReadFile` is a representative read-only binding. `tools.WriteFile` is a
mutating binding with a content-digest receipt and sandbox-root/path semantic
guard. Custom legacy bindings should be deliberately wrapped as read-only or
mutating before relying on restart recovery.

## Durable questions and approvals

`WithDurableWait` suspends a Runtime tool invocation without retaining its
worker. A question answer becomes the tool result; the wrapped question tool is
never executed. An approval instead gates dispatch of the wrapped tool. Direct
`Agent.Run` calls remain process-local and execute the wrapped tool normally.

Approvals bind the operation ID, tool, canonical arguments, resource target,
definition revision, policy context, and expiry into `WaitSnapshot.ActionDigest`.
The host must configure `RuntimeConfig.Authorizer`; an actor string in a
resolution is audit context, never proof of authority. Runtime calls the
authorizer when accepting an allow decision and again immediately before the
atomic dispatch transition. Modification is deliberately unsupported: a changed
action needs a new model invocation and approval.

A minimal host flow is:

```go
write := core.WithDurableWait(writeTool, core.DurableWaitPolicy{
    Kind:          core.WaitApproval,
    PolicyContext: "files-v3",
    ExpiresAfter:  15 * time.Minute,
    Target: func(raw json.RawMessage) (string, error) {
        var in struct{ Path string `json:"path"` }
        if err := json.Unmarshal(raw, &in); err != nil { return "", err }
        return in.Path, nil
    },
})

agent, err := core.New(provider, core.AgentConfig{Tools: []core.Tool{write}})
if err != nil { return err }

runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
    Store: store,
    Authorizer: core.ApprovalAuthorizerFunc(func(ctx context.Context,
        check core.ApprovalAuthorization) error {
        // Authenticate outside core and consult current policy here.
        return currentPolicy.Authorize(ctx, check.Actor, check.Target)
    }),
})
// Register the same definition revision, then recover after every restart.
_ = runtime.Register("writer", "v3", agent)
_ = runtime.Recover(ctx)

snapshot, _ := handle.Snapshot(ctx)
wait := snapshot.Waits[0] // render wait.Prompt/Target in the host UI
err = handle.ResolveWait(ctx, wait.ID, core.WaitResolution{
    Decision:     core.Allow,
    Actor:        authenticatedSubject,
    ActionDigest: wait.ActionDigest,
})
```

For a question use `WaitQuestion` and resolve with a valid JSON `Answer` instead
of a decision. After `Runtime.Close`, reopen the same store, register the same
binding, call `Recover`, inspect the same wait, and resolve it. If the HTTP/CLI
response is lost, submit the identical `ResolveWait` again: it returns the
stored outcome even if the run has since completed. A different answer or
approval returns `ErrWaitConflict`; stale action digests, expired waits, revoked
authority, and cancellation never dispatch the mutation. `RunSnapshot.Waits`
keeps those races inspectable.

## Declared structured output

A definition can require its final answer as validated structured data:

```go
agent, err := core.New(provider, core.AgentConfig{
    Tools: []core.Tool{write},
    StructuredOutput: &core.StructuredOutputConfig{
        Schema: json.RawMessage(`{
            "type":"object",
            "properties":{"summary":{"type":"string"}},
            "required":["summary"]}`),
        Native: true,        // request provider-native enforcement when supported
        MaxCorrections: 1,   // bounded in-run correction turns
    },
})
```

The declaration is frozen with the agent, so a Runtime definition revision pins
both the executable binding and its output contract. `New` validates `Schema`
against the one supported schema contract shared by tool inputs,
`CallOptions.OutputSchema`, typed helper values, and declared final outputs.
Core enforces the documented subset (`type`, `enum`, object properties,
required fields, arrays, selected string and numeric bounds) and rejects
unsupported assertion keywords (`oneOf`, `$ref`, `const`, `uniqueItems`, and
similar) explicitly instead of silently weakening validation. Annotation and
provider-native metadata (for example `title`, `description`, `format`,
OpenAI's `strict`, and `$defs`) pass through unchanged for providers that use
them, while core still enforces only the subset documented in
`core/schemacontract.go`.

At run time the hidden `automata_structured_output` tool collects the payload
(or, with `Native` and a provider that supports native structured output, the
schema travels via `CallOptions.OutputSchema` and the final response text is
validated). Runtime can also recover a committed provider-accepted native/prose
assistant turn by reclassifying its already accepted text; it does not replay a
provider turn or dispatch tools merely to reinterpret that structured payload.
Every payload is checked before it becomes the run's accepted output; the
validated value is reported as `RunResult.StructuredOutput` (and in
`RunSnapshot.Result.StructuredOutput`), deliberately separate from the
model-facing `Output` text, `FinalMessage` blocks, provider-native `RawBlock`
history, and durable effect receipts exposed in `ToolBatchSnapshot`.

An invalid payload does not fail the run while correction budget remains:
Runtime commits the violations as model-visible evidence, increments a
persisted correction-turn count, and asks the model again inside the same run
and the same budgets. Tool turns the model already completed are never replayed
to fix formatting, and a newly proposed duplicate mutation is rejected by the
configured semantic guard. The correction count persists with the transition
that re-dispatches the correction, so a restarted run continues with its
original correction, turn, tool, and provider budgets; with the budget
exhausted the run fails with `ErrInvalidStructuredOutput` (error kind
`invalid_structured_output`) while retaining accepted receipts and partial
evidence. When the run's turn limit ends correction with an invalid payload
still pending, the error kind is `max_steps_invalid_structured_output` and
`errors.Is` matches both `ErrMaxStepsExceeded` and `ErrInvalidStructuredOutput`.
A known recovery limitation is that restarting after the correction transition
but before turn-limit classification can report plain `max_steps`: the in-memory
invalid-cause marker is not restored, although correction counts, violation
history, budgets, and effect receipts remain persisted.

The direct typed facade shares this engine: [RunTyped]/[RunSessionTyped]
decode the accepted `RunResult.StructuredOutput` payload after the loop
finishes. If an Agent already declares [AgentConfig.StructuredOutput], typed
helpers reuse that pinned declaration instead of installing a second hidden
tool; choose a Go result type compatible with the declared schema or decoding
will fail after the run. Direct `Agent.Run` runs of a declared definition also
enforce the same contract through the same loop, but direct Agent, Session, and
typed helper entry points remain process-local. Only Runtime promises durable
admission, persisted correction counts, recovery, and effect preservation.

## Durable children

A parent delegates to another registered definition through a
`DurableChildTool`. The declaration pins the child's definition ID and
revision and freezes the model-facing input schema:

```go
if err := runtime.Register("researcher", "v2", researcher); err != nil {
    return err
}
research := core.DurableChildTool(core.ToolDefinition{
    Name:        "research",
    Description: "Research one topic.",
    InputSchema: json.RawMessage(`{"type":"object",
        "properties":{"topic":{"type":"string"}},"required":["topic"]}`),
}, core.DurableChildPolicy{DefinitionID: "researcher", Revision: "v2"})

lead, err := core.New(provider, core.AgentConfig{Tools: []core.Tool{research}})
if err != nil { return err }
if err := runtime.Register("lead", "v1", lead); err != nil { return err }
```

Each call is admitted as an ordinary durable child run. The child run, its link
to the parent run and operation ID, and an internal child wait commit in the
same transaction that reserves the parent invocation. The child's task is the
model's arguments, validated against the frozen schema and passed as raw JSON.
Invalid arguments are a model-visible error and admit no child, although, as
for any known tool, the call has already consumed its cap reservation. Admission fails
closed when the pinned child binding is not registered. An admission replay
after a crash resolves the same child, and completed siblings are never rerun.
Children are never retried implicitly.

Runtime never infers durable children from Agent values. `Register` rejects a
process-local `AsTool` or `AsToolFunc` adapter, directly or behind first-party
wrappers. It also rejects a child tool wrapped for retry, durable waits, or an
effect policy, and a parent that would intercept child calls with a
process-local `Approver`, per-call timeout, or rate limiter. The declaration's
`Execute` always fails, so direct `Agent.Run` cannot invoke it by accident.

The parent's worker returns while children run. When a child terminalizes,
Runtime consumes the child wait once and makes the parent runnable; recovery
repeats the check if that wake was lost. `ResolveWait` cannot resolve child
waits. The parent receives the child's accepted structured output as JSON text,
otherwise its final message blocks (including `RawBlock`), or a model-visible
error when the child failed or was canceled. The child run keeps its own
transcript, effects, and receipts: `ToolInvocationSnapshot.ChildRunID` and
`WaitSnapshot.ChildRunID` link down, and the child's
`RunSnapshot.Parent.RunID`/`OperationID` link up (`Parent` is nil for a root run).

A child that needs attention, or a terminal child whose subtree still has an
uncertain effect or an unsettled run, blocks the parent in
`RuntimeNeedsAttention` with `Attention.Kind == AttentionChild`. The parent's
`Attention.BlockingRunID` identifies the immediate pending child responsible
for the current attention, not necessarily the descendant that needs action.
Inspect that child's snapshot: child attention may only mean a canceled subtree
is settling without host intervention. When an operation needs reconciliation,
reconciling it (or the child otherwise settling cleanly) lets the parent consume
the outcome and continue without replaying child work.

### Caps and accounting

`ToolPolicy.MaxCalls` is pinned on each run at admission and acts as a subtree
cap. A child invocation consumes one reservation from its parent. Every known
tool call in a descendant reserves against its own caps and every capped
ancestor, atomically with the descendant's batch creation. `PerTool` caps stay
local to their run, zero stays unlimited, and turn and provider limits are
per run. A process-local agent that a Runtime tool runs internally (for example
`inner.Run(ctx, ...)` inside a `Func`) charges the same persisted caps for each
of its known calls. Counters live on the persisted records, survive restarts,
and are never charged again when a batch is replayed.

`RunResult.Usage` stays local to one run. `RunSnapshot.Accounting.Tree` totals `Usage`,
`ProviderAttempts`, and `UnknownAttempts` across the run and its linked
descendants, counting each run's persisted local values once. Repeated
completion notices or recovery passes never add usage again. `Accounting.Tree.Unsettled`
counts descendants that are not yet terminal. This is inspection, not a budget
or billing ledger.

### Cancellation and deadlines

Children are required: there is no detached mode. Canceling a run cancels every
non-terminal descendant in the same commit, before any local worker is
signaled. Suspended descendants become terminal at once. Running descendants
move to `RuntimeCancelRequested` and cannot admit children or dispatch reserved
tools; after a crash, recovery finishes their cancellation without dispatching
anything. A child inherits its parent's deadline at admission. When a waiting
parent's deadline passes, its suspended descendants are finalized with it.

A canceled parent may become terminal before a non-cooperative descendant
stops. It keeps its links and all descendant effect evidence,
`Accounting.Tree.Unsettled` shows the work still settling, and it never reports clean completion.
`RunSnapshot.Definition` holds the pinned ID/revision, `Conversation` identifies
a turn (nil otherwise), and `Failure` is nil on success. A non-nil `Failure`
contains the persisted message, typed `FailureKind`, and any completion stop
reason. `Attention` is nil outside needs-attention; `Accounting.UnknownAttempts`
is local to the run, while `Accounting.Tree.UnknownAttempts` includes descendants.
These are public projections; the storage encoding is unchanged.

Canceling a child directly delivers the canceled outcome to its waiting parent
as a model-visible error once the child and its subtree have settled. A running
child first finishes its cancellation; while any descendant is still settling,
the parent reports child attention. A parent blocked on child attention is
still suspended, so its own deadline is enforced as for a waiting run.

## Conversations

A Runtime conversation serializes runs of one definition into turns:

```go
first, err := runtime.Run(ctx, "assistant", "v1", "Summarize the report.",
    core.SubmitOptions{Conversation: core.ConversationOptions{
        Scope: tenantID, ID: threadID}})
if err != nil { return err }

next, err := runtime.Run(ctx, "assistant", "v1", "Now list the risks.",
    core.SubmitOptions{Conversation: core.ConversationOptions{
        Scope: tenantID, ID: threadID, ExpectedHead: first.RunID}})
```

The first turn pins the definition and revision. Each later turn names the
committed head it continues. Admission reserves the conversation's single
active slot: a competing turn returns `ErrConversationBusy`, and a stale
`ExpectedHead` or a different definition returns `ErrConversationConflict`.
With `Scope` and `Key`, an exact retry resolves its original run before these
checks, so a lost acknowledgement never turns into a conflict.

A turn starts from the head's committed transcript plus the new task, appended
exactly once. Provider-native blocks are preserved, `RunResult.Messages` holds
the whole conversation, and `RunResult.Usage` covers only that turn. The head
advances, and the active slot is released, in the same commit that makes the
turn terminal, whether it completed, failed, or was canceled. A failed or
canceled turn with structurally complete history can be continued. A head with
unanswered tool calls, uncertain effects, or unsettled children returns
`ErrConversationBlocked`; Runtime never fabricates results to make history
valid. A turn that needs attention keeps the conversation busy until it is
reconciled, acknowledged (for interrupted hook delivery), or canceled.
`Runtime.Conversation` returns the committed head, active run, and turn count.
Each turn copies the committed history into its own transcript; bounding
long-history cost is T08 work.

## Transformation, observation, and history

- `PreSendHook` remains an immutable request transformation supplied by the
  registered Agent.
- Agent `RunObserver`s remain direct-run compatibility callbacks and are not
  invoked by Runtime. `RunHandle.Observe` and stream callbacks are bounded,
  provisional local views; callback success is never storage acknowledgement.

The old checkpoint callback was removed. Durable commits belong to Runtime;
committed hooks consume those commits rather than acting as storage authority.
T08 adds replayable persisted events and cursors for integrations that need
reconnectable delivery rather than an in-process hook attempt.

Canonical conversation data remains JSON-encoded `[]core.Message`, with
`Message.Blocks` as the source of truth. Pending provider/tool work is not
fabricated as canonical history.

## Direct entry points

`Agent.Run`, `Agent.RunStream`, `Session`, `RunTyped`, `RunSessionTyped`,
`AsTool`, and `AsToolFunc` remain direct, process-local transitional entry
points; T09 migrates or removes them. None of them persists anything or
survives a restart, and the `Session` mutex is not a durable concurrency
primitive. The durable equivalents are Runtime conversations for `Session`,
`DurableChildTool` for `AsTool`/`AsToolFunc`, and a declared
`StructuredOutput` for typed results. New durable code should start with
`Runtime`.
