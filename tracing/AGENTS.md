# Tracing package guidance

`tracing` defines the minimal observability boundary used by `core`.

- Keep it dependency-free and provider-neutral.
- Preserve the no-op implementation as a safe default.
- Do not expose a concrete tracing vendor's types in public interfaces.
- Add API surface only when the runtime can use it consistently across run,
  provider, hook, and tool spans.
- Attribute values should remain cheap to construct and safe to attach on hot
  execution paths.

Validate with `go test ./tracing` and the affected `core` tests.
