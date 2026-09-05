# Typed agents example guidance

This example demonstrates typed sub-agent handoffs, typed session results, and
JSON transcript checkpoint/resume behavior.

- Keep typed schemas small, explicit, and useful at orchestration boundaries.
- Preserve the distinction between `AsToolFunc` natural-language handoffs and
  a `core.Func` wrapper that returns typed child output.
- Check every returned error while retaining partial `RunResult` data.
- Keep checkpoint files and generated binaries out of version control.
- Avoid adding application abstractions that obscure the public API being
  demonstrated.

Validate with `go build ./...`; live execution requires `ANTHROPIC_API_KEY`.
