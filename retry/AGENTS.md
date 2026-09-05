# Retry package guidance

`retry` is a generic, provider-neutral backoff primitive.

- Keep the package independent of `core` and provider SDKs.
- A non-positive `MaxAttempts` still invokes the operation exactly once.
- Context cancellation and deadlines stop immediately.
- Unknown errors retry only when `RetryUnknown` is enabled; errors implementing
  `Retryable` control their own classification.
- Preserve bounded exponential backoff and jitter without sleeping in tests.
- Prefer deterministic tests with tiny durations or injected behavior over
  timing-sensitive assertions.

Validate with `go test ./retry` from the repository root.
