# First-party tools module guidance

This standalone module supplies reusable `core.Tool` implementations.

- Keep tools narrowly scoped, bounded, and safe for model-controlled input.
- Validate paths, URLs, commands, and numeric limits inside the tool; prompts
  and approvers are additional policy layers, not validation substitutes.
- File access must stay rooted with `os.Root`. Shell execution must stay
  argv-based with an exact allow-list and no shell interpretation.
- Bound network response size, file reads, command output, and execution time.
- Parent cancellation is fatal. A tool's own timeout should normally return a
  plain recoverable error so the model can adapt.
- Do not mark non-idempotent failures retryable.
- Keep the module dependency-light; search vendors belong in `extensions/*`.

Validate here with `go test ./...`; use `GOWORK=off go test ./...` when module
dependencies or public interfaces change.
