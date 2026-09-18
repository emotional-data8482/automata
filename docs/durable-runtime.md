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
deadline conflicts. Cancel and reconcile commands also retain exact durable
receipts; waits and signals join this model in T05.

## Three lifetimes

- A context passed to `Submit`, `Snapshot`, `Await`, `Observe`, `Run`, or
  `RunStream` bounds only that API operation or view. Once admission commits,
  disconnecting it does not cancel the logical run.
- `Runtime.Close` cancels its worker context and waits for execution to yield
  before closing storage. An interrupted whole-loop segment is conservatively
  marked `RuntimeNeedsAttention`; it is never automatically replayed.
- `RunHandle.Cancel` is the logical cancellation command. A persisted deadline
  in `SubmitOptions` has the same logical ownership.

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

Each attempt is recorded in `RunSnapshot.HookResults`. Hook errors and panics
remain visible there but never change `RunResult` or its execution error. After
the hook outcomes commit, the run becomes `RuntimeTerminal` and `Await`
returns. Runtime never retries a hook automatically: if the process stops in
`RuntimeFinalizing`, recovery reports `RuntimeNeedsAttention` because an
external effect may have happened without its outcome being stored.

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
fabricated as canonical history. Durable conversation-head serialization and
cross-process continuation build on this format in T07; the current `Session`
mutex is not a durable concurrency primitive.

## Direct entry points

`Agent.Run`, `Agent.RunStream`, `Session`, `RunTyped`, and `RunSessionTyped` are
currently direct, process-local entry points. `Session` is still the useful
conversation concept; T07 moves that concept onto committed Runtime history
rather than discarding it. T06 moves typed correction into the same lifecycle.
New durable code should start with `Runtime`.
