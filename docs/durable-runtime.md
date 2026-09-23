# Durable runtime lifecycle

`core.Runtime` is the only way to run an `Agent`. It admits a task before any
work, owns worker contexts independently of API and view contexts, pins an
immutable `Agent` registration, commits every transition, and recovers after
a restart. Sync calls, live streams, typed output, child agents,
conversations, and approvals are all views of this one lifecycle.
`loopMachine` is its internal turn driver, not a public lifecycle.

Persistent construction is explicit:

```go
store, err := sqlite.Open(ctx, "automata.db")
if err != nil { return err }

runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
if err != nil { return err }
defer runtime.Close()

agent, err := core.New(provider, core.AgentConfig{SystemPrompt: "Be concise."})
if err != nil { return err }
assistant, err := runtime.Register("assistant", "2026-09-15", agent)
if err != nil { return err }
if err := runtime.Recover(ctx); err != nil { return err }

handle, err := runtime.Submit(ctx, assistant, task,
    core.WithIdempotencyKey(tenantID, externalTaskID))
if err != nil { return err }

result, err := handle.Await(ctx)
```

Use `core.NewEphemeralRuntime()` only when losing runs on process exit is
intended (tests, scripts), or `core.NewRuntime` with `core.NewMemoryStore()`
to configure hooks or an authorizer on an in-memory store. An unavailable or
invalid persistent store fails construction or admission; the runtime never
silently substitutes memory. The compiled, output-checked examples in
`core/example_test.go` show each feature end to end.

## Identities and registration

`Agent` is the immutable executable definition. `Register` binds it to an
application-controlled definition ID and revision and returns the
`DefinitionRef` that runs, child tools, and conversations pin. Registering a
different Agent under the same pair is rejected. The revision is a host
attestation, not proof of Go closure or binary identity: register a new
revision whenever behavior changes, and register every revision that stored
runs pin before `Recover`. There are no per-run configuration overrides;
a variation is a separate revision, which keeps every admission reproducible.

`WithIdempotencyKey(scope, key)` is the external idempotency identity. An
exact retry returns the original `RunHandle`; a changed task, definition,
revision, deadline, or conversation returns `ErrAdmissionConflict`. Compute
a deadline once and reuse it on retries: a deadline recomputed from
`time.Now()` is a different admission. Cancel, reconcile, and
wait-resolution commands retain historical durable outcomes, including after
later run transitions.

## Three lifetimes

- A context passed to `Submit`, `Snapshot`, `Await`, `Observe`, `Run`, or
  `RunStream` bounds only that API operation or view. Once admission commits,
  disconnecting it does not cancel the logical run.
- `Runtime.Close` cancels its worker context and waits for execution to yield
  before closing storage. An interrupted whole-loop segment is conservatively
  marked `RuntimeNeedsAttention` (`AttentionProvider` when a provider call was
  in flight); it is never automatically replayed.
- `RunHandle.Cancel` is the logical cancellation command. A persisted deadline
  (`WithDeadline`) has the same logical ownership. Both reach every required
  durable child (see [Durable children](#durable-children)).

`Runtime.Run` is `Submit` plus `Await`. `Runtime.RunStream` admits the same kind
of run and attaches a bounded live view; detaching the view does not own worker
or logical cancellation. `RunHandle.Observe` exposes that provisional view
separately. Committed facts are replayable through `RunHandle.Events` (see
[Observation](#observation)).

## Observation

A run has two kinds of observation:

- **Provisional deltas.** `RunHandle.Observe` and the `RunStream` callback
  deliver live `StreamEvent`s from the worker (text and thinking deltas, tool
  calls, tool results, usage). A child run's events also reach every
  ancestor's views, tagged with `StreamEvent.Agent` (the child tool name) and
  `InvocationID` (the parent's tool call ID); a nested child keeps the
  innermost tags. Each view has a bounded queue; a slow view misses deltas and
  never delays the run.
- **Committed events.** Every commit that changes a run's state, transcript,
  tool invocations, or waits appends `CommittedEvent`s in the same commit:
  state changes (with attention kind and reason), transcript messages
  appended, tool invocation progress and effect status, and wait creation and
  resolution. Internal markers (attempt records, budget counters) emit none.
  Sequence numbers are per run, start at one, and have no holes. Runtime
  derives the events from the committed writes themselves, so no transition
  can commit without its event.

Read committed events in bounded pages from a cursor:

```go
cursor := snapshot.EventSequence // or 0 to replay from the start
for {
    page, err := handle.WaitEvents(ctx, cursor, 256)
    if errors.Is(err, core.ErrEventGap) {
        snapshot, err = handle.Snapshot(ctx) // resynchronize
        if err != nil { return err }
        cursor = snapshot.EventSequence
        continue
    }
    if err != nil { return err }
    for _, event := range page.Events {
        apply(event)
        if event.Kind == core.CommittedRunState && event.State == core.RuntimeTerminal {
            return nil // no later commits follow a terminal state
        }
    }
    cursor = page.Next
}
```

`Events` returns immediately; `WaitEvents` first waits until the run commits
past the cursor. Consumers pull, so there is no delivery buffer to overflow:
a stalled or disconnected consumer costs nothing until it reads again, and it
can resume from its cursor after a restart. A page holds at most 1024 events
and attaches at most 4 MiB of transcript (always at least one event). Message
events carry their messages, read from the transcript rather than stored
twice; `MessagesPruned` marks messages removed by retention. A cursor behind
the retained range or ahead of the committed head returns `ErrEventGap`:
resynchronize with `Snapshot`, whose `EventSequence` is read in the same
transaction as the rest of the snapshot.

Waiting is driven by commits, not polling. `Await`, `Observe`, `RunStream`,
and `WaitEvents` subscribe to the run's committed transitions in process. An
idle waiter reads nothing; a wake that cannot end the wait (for example a
transition between running states) reads nothing either, and `Await` reads the
full result once, when the run is terminal or needs attention. After
`Runtime.Close`, waiters stop waiting and return `ErrRuntimeClosed`; a run
pruned by retention returns `ErrRunPruned`. `Snapshot`
remains the authoritative full view, and its cost grows with the run's
history.

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

The provider-neutral `core.Store` contract is a small atomic transaction port:
`Get`, `Put`, `Delete`, and ordered `Scan`/`ScanPage` inside `Transaction`.
Core owns bucket names and versioned encodings; adapters own database details
and must pass `core/storetest`. `extensions/sqlite` is the supported local
adapter and keeps its driver out of the root module. It enforces one local
owner with an OS advisory lock. Storage encoding version 9 is current; stores
written by an earlier version are rejected without being rewritten.

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

`Recover` pages through an index of non-terminal runs only, so its cost
follows the active work, not the number of runs ever stored.

### Provider attempts

Runtime commits an attempt record immediately before each provider call. If a
worker stops while that record is open, the provider may already have
received (and billed) the request, and its response is lost. Recovery counts
the attempt in `RunResult.ProviderAttempts` and in
`RunSnapshot.Accounting.UnknownAttempts` (its usage, if any, is not in
`RunResult.Usage`; Runtime never invents it; retries within one turn share one
record) and never silently sends
the request again: the run needs attention with `AttentionProvider`. A run
stopped before its attempt record committed never reached the provider and
resumes normally.

A host that accepts the cost may authorize fresh attempts explicitly:

```go
runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
    Store:            store,
    ProviderRecovery: core.ProviderRecoveryPolicy{MaxFreshAttempts: 1},
})
```

Within the bound, recovery starts a new provider turn from the committed
transcript and counts it in `Accounting.FreshAttempts`; the fresh attempt
consumes the turn budget like any turn. Beyond the bound the run needs
attention; raising the bound and calling `Recover` again continues it, and
`Cancel` ends it.

### Background driver

`NewRuntime` starts a driver that keeps suspended work moving without a host
calling `Recover`: it finalizes a waiting run (or a parent blocked on child
attention) at its logical deadline, expires waits at `ExpiresAfter`, and
repairs a child completion or attention notice whose parent wake was lost.
Commits schedule it, so it costs nothing while idle. `Close` stops it.
`Recover` remains the startup step that adopts a previous owner's runs and
arms their timers; returning from `Recover` does not guarantee that those runs
are terminal. Background work, including committed-run hook delivery, may
still be finishing. Use `Await` or observe the run snapshot when terminal
completion is required. A run stranded by a storage failure in this process
stays visible to `Await` as that failure until an explicit `Recover`.

### Payload limits and integrity

Transcript chunks (one per provider turn or committed tool batch) and tool
invocation records are the payload facts; the run record stays compact.
`RuntimeConfig.MaxPayloadBytes` bounds each encoded payload (default
`DefaultMaxPayloadBytes`, 16 MiB). Runtime never truncates: a provider turn
or committed tool batch too large to store leaves the run in
`RuntimeNeedsAttention` with its committed transcript unchanged, and recovery
resumes it only after `MaxPayloadBytes` is raised to cover it. A tool result
or durable child answer too large to store leaves its invocation
`ToolInvocationUncertain` with its effect report intact, so `Reconcile` can
supply an authoritative smaller result without running anything again.

Every transcript chunk and tool result carries a SHA-256 digest checked on
every read. A missing, truncated, or altered payload returns
`ErrPayloadUnavailable` from `Snapshot` and `Events`; recovery marks that run
as needing attention with the reason and continues with the others, and a
worker that cannot load its history does the same instead of guessing. An
acknowledged cancellation still finishes; `Cancel` is the exit for other runs
with unavailable payloads. `Prune` skips such runs (`PruneReport.Skipped`) and
continues.

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
never executed. An approval instead gates dispatch of the wrapped tool.

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
writer, _ := runtime.Register("writer", "v3", agent)
_ = runtime.Recover(ctx)
handle, _ := runtime.Submit(ctx, writer, task)

snapshot, _ := handle.Snapshot(ctx)
wait := snapshot.Waits[0] // render wait.Prompt/Target in the host UI
err = handle.ResolveWait(ctx, wait.ID, core.WaitResolution{
    Decision:     core.Allow,
    Actor:        authenticatedSubject,
    ActionDigest: wait.ActionDigest,
})
```

For a question use `WaitQuestion` and resolve with a valid JSON `Answer` and no
decision. An approval's `Decision` must be `core.Allow` or `core.Deny`; the
zero value is not a decision, so an approval is never granted by omission. After `Runtime.Close`, reopen the same store, register the same
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
        Schema:         core.OutputSchema[Summary](), // or a raw JSON schema
        Native:         true, // request provider-native enforcement when supported
        MaxCorrections: 1,    // bounded in-run correction turns
    },
})
// ... after the run:
summary, err := core.Decode[Summary](result)
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
`core/schemacontract.go`. `core.Decode[T]` validates the accepted payload
against the schema implied by `T` before decoding it, so a mismatched Go type
fails loudly instead of zero-filling.

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
exhausted the run fails with `ErrInvalidStructuredOutput` (failure kind
`invalid_structured_output`) while retaining accepted receipts and partial
evidence; `RunFailure.Violations` persists the last violations, so
`errors.As` with a `*core.InvalidStructuredOutputError` target works after
`Await` and after a restart. When the run's turn limit ends correction with an invalid
payload still pending, the kind is `max_turns_invalid_structured_output` and
`errors.Is` matches both `ErrMaxTurnsExceeded` and `ErrInvalidStructuredOutput`.
A known recovery limitation is that restarting after the correction transition
but before turn-limit classification can report plain `max_turns`: the
in-memory invalid-cause marker is not restored, although correction counts,
violation history, budgets, and effect receipts remain persisted.

## Durable children

A parent delegates to another registered definition through a child tool.
The declaration pins the child's `DefinitionRef` and freezes the model-facing
input schema, derived from a Go type by `core.ChildTool[P]` or given
explicitly to `core.NewChildTool`:

```go
researcher, err := runtime.Register("researcher", "v2", researcherAgent)
if err != nil { return err }

type ResearchInput struct {
    Topic string `json:"topic" desc:"the subtopic to research"`
}
research := core.ChildTool[ResearchInput]("research", "Research one topic.", researcher)

lead, err := core.New(provider, core.AgentConfig{Tools: []core.Tool{research}})
if err != nil { return err }
leadRef, err := runtime.Register("lead", "v1", lead)
if err != nil { return err }
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

Runtime never infers children from Agent values; composition is always an
explicit, pinned child tool. `Register` rejects a child tool wrapped for
retry, durable waits, or an effect policy, and a parent that would bound
child calls with a per-call timeout or rate limiter (the child inherits the
parent's deadline and enforces its own policy instead). A child tool's
`Execute` always fails: only Runtime admission consumes it.

A tool that runs another agent itself, for example on a separate ephemeral
runtime inside `Execute`, is an opaque host tool: its work is not linked,
budgeted, cancelled, or recovered with the parent. Use a child tool whenever
the nested work should be part of the parent's durable run.

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
per run. Counters live on the persisted records, survive restarts, and are
never charged again when a batch is replayed.

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
thread := core.ConversationRef{Scope: tenantID, ID: threadID}
first, err := runtime.Run(ctx, assistant, "Summarize the report.",
    core.WithConversation(thread, ""))
if err != nil { return err }

next, err := runtime.Run(ctx, assistant, "Now list the risks.",
    core.WithConversation(thread, first.RunID))
```

The first turn pins the definition and revision. Each later turn names the
committed head it continues. Admission reserves the conversation's single
active slot: a competing turn returns `ErrConversationBusy`, and a stale
`ExpectedHead` or a different definition returns `ErrConversationConflict`.
With `WithIdempotencyKey`, an exact retry resolves its original run before
these checks, so a lost acknowledgement never turns into a conflict. Without
one, recover a turn whose admission acknowledgement was lost from
`Runtime.Conversation`: its `ActiveRunID` (or, once it finished, `Head`)
identifies the admitted turn.

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
A turn references the head's committed transcript instead of copying it and
stores only what it adds, so storage grows linearly with the conversation.
Each turn still reads the whole history, because the provider receives it.

## Retention

Nothing is deleted unless the host asks. `Runtime.Prune` applies a
`RetentionPolicy` with an independent age per class, measured from each run's
terminal commit:

```go
report, err := runtime.Prune(ctx, core.RetentionPolicy{
    Events:  24 * time.Hour,      // committed event logs
    History: 30 * 24 * time.Hour, // transcripts and tool result payloads
    Runs:    90 * 24 * time.Hour, // whole run trees, leaving tombstones
    Limit:   500,                 // runs per class per call; report.More says more are due
})
```

Only settled runs are eligible: terminal, every descendant terminal, and no
dispatched or uncertain tool invocation anywhere in the subtree. Retention
therefore never removes evidence that a pending decision needs.

- `Events` deletes the event log; older cursors return `ErrEventGap`.
- `History` deletes transcript chunks and tool result payloads. The run keeps
  its state, accepted output, final message, usage, and every invocation's
  effect report; `RunSnapshot.HistoryPruned` and
  `ToolInvocationSnapshot.ResultPruned` report the removal. Conversation turns
  are skipped, because later turns reference their transcripts.
- `Runs` deletes a root run and all its durable descendants, including their
  receipts, and leaves a tombstone per run. Every operation on a pruned run,
  and an exact retry of its admission or of any command against it, returns
  `ErrRunPruned`; a changed submission under the same `Scope`/`Key` still
  returns `ErrAdmissionConflict`. An expired identity never becomes new work.
  Children are deleted only with their root, and conversation turns are kept.

Effect guards are never pruned, so a semantic duplicate of a pruned run's
mutation is still rejected. Each class keeps its own index of runs not yet
processed, so repeated calls never rescan finished work.

## Operating bounds

The supported profile is one exclusive local owner of a SQLite store (WAL,
`synchronous=FULL`) with process-crash recovery. These figures were measured
on an Apple M5 Pro (darwin/arm64, Go 1.26.2, local SSD) with the benchmarks
in `core/runtime_bench_test.go` and `extensions/sqlite/bench_test.go`. They
describe how costs scale, not guaranteed latencies on other hardware.

| Path | Bound | Measured |
| --- | --- | --- |
| Tool turn (2 KiB result) | 7 commits and constant bytes per turn, independent of history | about 12 KB and 1.2–1.3 ms per turn on SQLite, same at 16 and 64 turns |
| Conversation turn (2 KiB answer) | constant bytes per turn, independent of prior turns | about 27 KB per turn after 1, 16, or 64 turns |
| Idle `Await`/`Observe`/`WaitEvents` | no storage reads while nothing commits | 0 reads |
| Commit to `WaitEvents` consumer | one page read per wake | about 20–30 µs (in-memory store) |
| `Recover` | proportional to non-terminal runs only | 10 µs empty, 15 µs with 1,000 terminal runs on SQLite |
| Event page | at most 1024 events and 4 MiB of transcript | 256 events of a 64-turn run: 4.7 ms on SQLite |
| `Snapshot` | proportional to the run's history | 64-turn run: 6.9 ms on SQLite |
| `Prune` | one transaction per run and class | about 0.6 ms per run (all classes) on SQLite |
| One payload | `MaxPayloadBytes` (16 MiB default) | enforced, never truncated |

The run record is rewritten on each transition and carries the final
assistant message and output text, so bytes per turn grow with the size of
the final answer (not with history). Run `go test ./core -run '^$' -bench
Runtime` and `go test ./extensions/sqlite -run '^$' -bench SQLite` to
reproduce the measurements.

## Transformation, observation, and history

- `PreSendHook` is an immutable request transformation supplied by the
  registered Agent.
- `RunHandle.Observe` and stream callbacks are bounded, provisional local
  views; callback success is never storage acknowledgement. Committed hooks
  consume commits rather than acting as storage authority. Integrations that
  need reconnectable delivery read committed events from a cursor (see
  [Observation](#observation)).

Canonical conversation data remains JSON-encoded `[]core.Message`, with
`Message.Blocks` as the source of truth. Pending provider/tool work is not
fabricated as canonical history.

## Errors after persistence

An awaited run's error is rebuilt from its persisted `RunFailure`, so it is
the same in the process that ran it and after a restart. `errors.Is` matches
the sentinel of its kind (`context.Canceled`, `context.DeadlineExceeded`,
`ErrMaxTurnsExceeded`, `ErrEmptyResponse`, `ErrInvalidStructuredOutput`, and
the completion sentinels through `*CompletionError`), and `errors.As` finds a
`*CompletionError` or a `*InvalidStructuredOutputError` with its violations. A
tool's or provider's own error types keep only their message; inspect
`RunSnapshot.Failure` and the tool batches for structured evidence.

## Operating envelope

The supported deployment is one process that exclusively owns one SQLite
store on a local filesystem. `extensions/sqlite` enforces this with an OS
advisory lock: a second owner fails with `ErrOwned`. What is tested is
process-crash recovery (subprocess kills at every durable boundary), not
power loss, network filesystems, or several processes sharing one store. A
platform can run many independently owned runtimes, each with its own store;
failing a run over to another machine needs an ownership protocol this
library does not provide.

Within that envelope Runtime guarantees: an acknowledged admission survives;
a committed provider turn, tool outcome, wait resolution, or child link is
never repeated; a dispatched tool without a committed outcome is reported
uncertain rather than replayed; an interrupted provider call is never
silently resent; and every command has a durable receipt that answers an
exact retry.

It does not make external systems transactional with the store. Mutating
tools should send `ToolOperationFromContext(ctx).IdempotencyKey` to their
destinations and report their effect truthfully; `Reconcile` is the repair
path when they cannot.

## Storage upgrades and backups

The store records its encoding version (currently 9). A build opens only
stores of its own version and rejects others at `NewRuntime`, without
rewriting them, so a failed upgrade never damages data. No released version
has shipped `Runtime`; version 9 is the first supported encoding, and
earlier pre-release stores must be drained (every run terminal) and
discarded. A future encoding change will ship with an explicit migration
tool and its own version; the runtime will never migrate implicitly.

Back up a SQLite store with the runtime closed (`Runtime.Close`, then copy
the database file together with its `-wal` file), or while it is open with
SQLite's online backup, for example `VACUUM INTO 'backup.db'` from another
connection, which reads a consistent snapshot through WAL. The
`.owner.lock` file only enforces single ownership and is not part of a
backup. Restoring a backup rewinds the
runtime: runs admitted after the backup are unknown to it, and work that
happened in between may happen again, so treat a restore like a crash whose
uncertain work needs reconciliation against your destinations.

## Recovery runbook

After any restart:

1. Open the store and register every definition revision the store's runs
   pin. A run whose binding is missing needs attention with
   `ErrDefinitionNotRegistered` in its reason; register the revision and
   call `Recover` again.
2. Call `Recover`. It starts admitted work, resumes interrupted runs from
   their last committed transition, and arms deadlines and wait expiries.
3. List runs that need attention from your own index of admitted work (for
   example your ticket table) or from committed `CommittedRunState` events,
   and inspect `RunSnapshot.Attention`:

| `Attention.Kind` | Meaning | Action |
| --- | --- | --- |
| `execution` with an uncertain invocation | A tool was dispatched and its outcome was lost. | Check the destination by the invocation's operation ID, then `Reconcile` with the authoritative outcome; the run continues without re-executing the tool. |
| `execution` with a payload reason | A payload is too large or unreadable. | Raise `RuntimeConfig.MaxPayloadBytes` and `Recover`, reconcile the oversized invocation, or `Cancel`. |
| `provider` | A provider call may have been sent and billed; its response is lost. | Accept the cost by raising `ProviderRecoveryPolicy.MaxFreshAttempts` and calling `Recover`, or `Cancel`. |
| `hooks` | Committed-run hook delivery was interrupted. | Check what the hooks may have done, then `AcknowledgeHooks`. |
| `child` | A child run is blocking the parent; `BlockingRunID` names it. | Act on that child (it may only be settling after a cancel); the parent continues when it settles. |

4. `Cancel` is always available and records the run as canceled while
   keeping all effect evidence.
5. Retry lost responses exactly: resubmitting with the same idempotency key,
   resolving a wait again, or repeating a reconciliation returns the stored
   outcome instead of doing the work twice.

`examples/durable_host` runs this procedure end to end across processes,
including a crash between an external write and its commit.
