# Example module guidance

Examples are executable documentation for released public APIs.

- Favor clarity over abstraction and keep the main execution path easy to
  follow.
- Use environment variables for credentials and never commit `.env` files,
  transcripts, generated reports, or compiled binaries.
- Give every run a context deadline and deliberate step/tool budgets.
- Show partial-result error handling instead of discarding `RunResult`.
- Keep local `replace` directives for repository development; examples are not
  published modules.
- Build examples without making live provider calls during verification.

Validate an affected example with `go build ./...` from its module directory.
