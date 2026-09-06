# Deep research example guidance

This example demonstrates a streamed multi-agent TUI.

- Keep researcher, writer, and coordinator roles distinct with bounded tools.
- Route UI state through `StreamAccumulator`; do not reconstruct nested-agent
  state from text output.
- Keep Bubble Tea updates non-blocking and stream callbacks lightweight.
- Preserve invocation IDs so repeated or concurrent child runs render in
  separate lanes.
- Search and provider calls require credentials; builds and unit-level checks
  must not.

Validate with `go build ./...` and exercise pure formatting/state helpers when
their behavior changes.
