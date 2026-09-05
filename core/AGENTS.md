# Core package guidance

`core` is the provider-neutral runtime. Changes here affect every provider,
tool, example, and downstream application.

## Preserve these contracts

- `Agent.Run` is one-shot; `Session` serializes runs and commits partial history
  on every terminal path.
- `Message.Blocks` is the source of truth. New block behavior must round-trip
  through JSON and degrade explicitly at provider boundaries.
- Every committed assistant tool call receives exactly one tool result.
- Batch results commit in model order; live result events may arrive in
  completion order.
- Unknown tools, denials, and ordinary execution errors are recoverable.
  Context cancellation, deadlines, and approver failures are fatal.
- Returned `RunResult` values retain all output, transcript, usage, step, and
  stop-reason data available before an error.
- Streaming and non-streaming execution should remain behaviorally equivalent.
- Nested agents inherit shared tool budgets and tag stream events with agent and
  invocation identity.

## Change guidance

- Keep public concepts orthogonal. Prefer an internal collaborator or `RunOption`
  over a new top-level abstraction when the behavior is run-scoped.
- Do not import vendor SDKs into this package.
- Add table-driven tests around observable lifecycle behavior. Use fake
  providers; live model calls do not belong in unit tests.
- For concurrency changes, assert transcript completeness and ordering, then run
  `go test -race ./core` from the repository root.

Use `core_explorer` to trace a lifecycle path and `core_expert` to review any
change involving `loop.go`, `session.go`, `stream.go`, `toolbatch.go`, or
`typed.go`.
