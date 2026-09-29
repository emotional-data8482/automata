# Core package guidance

`core` is the provider-neutral runtime. Changes here affect every provider,
tool, example, and downstream application.

## Preserve these contracts

- `Runtime` is the only execution lifecycle. `Agent` is an immutable
  definition; runs are admitted before work, every transition commits, and
  recovery never repeats a committed turn, tool outcome, or child link.
  `docs/durable-runtime.md` is the authoritative contract.
- Conversations serialize turns through their committed head; a turn's
  partial history commits on every terminal path.
- `Message.Blocks` is the source of truth. New block behavior must round-trip
  through JSON and degrade explicitly at provider boundaries.
- Every committed assistant tool call receives exactly one tool result.
- Batch results commit in model order; live result events may arrive in
  completion order.
- Unknown tools, invalid arguments, budget and approval denials, and policy
  timeouts are recoverable. A tool's Go error, logical cancellation, and
  deadlines are fatal.
- Returned `RunResult` values retain all output, transcript, usage, turn, and
  stop-reason data available before an error. Awaited errors are rebuilt from
  the persisted failure, identically before and after a restart.
- Streaming and non-streaming providers must produce the same committed
  history.
- Child runs share persisted subtree caps, deadlines, and cancellation, and
  their live events reach ancestor views tagged with the child tool name and
  call ID.

## Change guidance

- Keep public concepts orthogonal. Prefer an internal collaborator or a
  definition field over a new top-level abstraction. Per-run behavior that
  affects execution must be persisted with the admission, so prefer a new
  definition revision over a runtime override.
- Do not import vendor SDKs into this package.
- Add table-driven tests around observable lifecycle behavior. Use fake
  providers; live model calls do not belong in unit tests.
- For concurrency changes, assert transcript completeness and ordering, then run
  `go test -race ./core` from the repository root.

## Layout

`Runtime` is split by role; put new code in the file that owns its role.

- `runtime.go`: construction, configuration, definition registration, `Close`.
- `runtime_submit.go`: admission, submit options, `Run`, and `RunStream`.
- `runtime_execute.go`: claiming and executing runs, finalization, and
  failure and attention transitions.
- `runtime_recover.go`: every `Recover` decision.
- `runtime_hooks.go`: committed-run hook delivery and acknowledgement.
- `runtime_handle.go`: `RunHandle` host controls.
- `runtime_live.go`: provisional live-stream fan-out.
- `runtime_observe.go`: commit capture, committed events, and cursors.
- `runtime_record.go`: the stored run record, buckets, transition markers, and
  transcript chunks.
- `runtime_snapshot.go`: `RunSnapshot` and its value types.
- `runtime_{tools,children,waits,conversations,retention,driver}.go`: durable
  tool batches, child runs, waits, conversations, retention, and the background
  driver.
- `loop.go` and `loopmachine.go`: the per-run turn engine the runtime drives.

Use `core_explorer` to trace a lifecycle path and `core_expert` to review any
change involving `runtime*.go`, `loop.go`, `loopmachine.go`, `stream.go`, or
`typed.go`.
