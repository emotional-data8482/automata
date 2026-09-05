# Provider and backend extension guidance

Each child directory is an independently versioned Go module.

- Translate vendor requests, responses, streaming deltas, usage, errors, tool
  schemas, and stop reasons at this boundary.
- Preserve every provider-neutral block the vendor supports. Use `RawBlock` for
  provider-specific content that must round-trip.
- Never add vendor dependencies to the root module.
- Wrap transient vendor failures with a type implementing `retry.Retryable`;
  preserve context cancellation unchanged.
- Add conversion tests for each supported block and malformed-input path.
- Keep streaming and non-streaming response semantics aligned.
- When `core` changes, review every extension even if only one adapter needs a
  code edit.

Use `module_integrator` for cross-provider impact analysis. Test an affected
module both through `go.work` and with `GOWORK=off go test ./...`.
