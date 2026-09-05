# OpenAI-compatible provider guidance

- This adapter targets OpenAI-compatible Chat Completions behavior; avoid
  assuming every compatible endpoint supports every OpenAI option.
- Preserve tool-call IDs, fragmented streaming arguments, usage, finish
  reasons, images, and supported raw fields across conversions.
- Keep capability fallbacks explicit when an endpoint ignores or rejects an
  option.
- Validate malformed SSE/data frames and partial streams without turning
  incomplete output into a normal completion.
- Keep the implementation stdlib-only unless a dependency has a clear,
  repository-wide benefit.

Validate with conversion, streaming, error, and request-shape tests here.
