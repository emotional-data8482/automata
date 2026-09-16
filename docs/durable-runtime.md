# Durable runtime lifecycle

`core.Runtime` is the T02 execution lifecycle. It admits a task before work,
owns worker contexts independently from API/view contexts, and pins an
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
deadline conflicts. T03 extends this command-receipt model to every control.

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
registered. Work found in `running` or `cancel_requested` state becomes
`RuntimeNeedsAttention`. The existing loop commits an accepted provider turn
before it can dispatch requested tools and records the complete canonical batch
after execution. Those transitions retain partial evidence, but T02 does not
pretend they are sufficient to replay arbitrary effects. T03 and T04 add
receipts, resumable transition claims, per-invocation outcomes, and effect
reconciliation.

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
