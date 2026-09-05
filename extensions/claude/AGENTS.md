# Anthropic provider guidance

- Preserve thinking signatures and `redacted_thinking` across tool loops.
- Coalesce tool results into valid Anthropic user turns without losing block
  order or native error flags.
- Keep prompt-cache breakpoints valid for tools, system prompts, conversations,
  and thinking blocks.
- Map image data and URLs explicitly and reject block shapes the API cannot
  represent.
- Keep SDK retries and Automata retries distinguishable in code and docs.
- Native structured output capability is provider-level; unsupported models
  must surface invocation errors clearly.

Validate with conversion, caching, streaming, and provider tests in this module.
