---
name: automata-maintainer
description: Implement, review, or plan changes inside the Automata Go repository, including core runtime behavior, provider adapters, first-party tools, examples, and multi-module compatibility. Use for work on this repository itself; use automata-go for applications that consume the library.
---

# Maintain Automata

Work from the closest `AGENTS.md` and preserve the repository's provider-neutral
boundaries and tested execution semantics.

## Workflow

1. Classify the change as core runtime, support package, tool, provider adapter,
   example, documentation, or release work.
2. Read the public entry point, implementation path, nearby tests, and relevant
   planning or docs before editing.
3. For changes crossing packages or involving concurrency/provider translation,
   delegate independent read-only mapping to the matching custom agent when
   subagents are available. Keep one writer for the shared worktree.
4. State the observable contract and failure behavior the change must preserve.
5. Implement the smallest coherent patch and add tests at the layer where the
   behavior is observable.
6. Format changed Go files and run targeted tests. Broaden validation to every
   affected Go module before finishing.
7. Review the final diff for public API growth, dependency leakage, stale docs,
   generated artifacts, and missing provider coverage.

## Routing

- Use `core_explorer` to trace a runtime lifecycle or locate behavioral tests.
- Use `core_expert` to review core execution, transcript, cancellation,
  streaming, typed-output, or concurrency changes.
- Use `module_integrator` for provider, tool, example, dependency, or release
  impact across modules.
- Use `simplify` after a substantial implementation when a smaller public or
  internal design may exist.

Read [references/architecture.md](references/architecture.md) when a change
crosses package boundaries or modifies runtime behavior. Do not load it for a
small isolated documentation or test correction.

## Completion contract

Report the resulting behavior, changed packages, validation performed, and any
remaining compatibility or integration limit. Never invoke the release script,
tag, push, or publish unless the user explicitly requests a release.
