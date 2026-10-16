# OpenRouter provider guidance

- This adapter wraps the official OpenRouter Go SDK. The SDK is beta and
  regenerates frequently: pin the exact version, and re-run every test here
  before bumping it.
- SDK-internal retries are disabled at construction; automata's retry layer
  owns retry classification through the wrapped `APIError`. Keep the two retry
  mechanisms distinguishable, as for the Claude adapter.
- Map Chat Completions content faithfully: text, reasoning (thinking), tool
  calls with stable IDs and fragmented arguments, and usage including cache
  tokens. Tool-result images travel as `image_url` content parts on the tool
  message. Degrade unsupported input (thinking blocks, raw blocks) explicitly
  by placeholder or documented drop, never silently.
- Prompt caching is opt-in through `WithAutomaticCaching`, which sets the
  top-level `cache_control` directive. Do not turn it on by default: Anthropic
  bills cache writes above the base input price.
- Native structured output travels as a `json_schema` response format without
  the strict flag (OpenRouter routes to hundreds of models; strict subsets are
  provider-specific). Core still validates the payload.
- Unknown or error finish reasons must never map to a success stop reason.
- Never add the SDK to the root module, and never let vendor types cross into
  `core`.

Validate with conversion, request-shape, error, streaming, and runtime tests
in this module. Unit tests use `WithServerURL` against a local test server —
never a live API key or a live model call.