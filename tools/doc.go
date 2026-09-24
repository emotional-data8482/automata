// Package tools provides reusable, zero-key [core.Tool] constructors built on
// the stdlib: web fetching ([HTTPFetch]), sandboxed file access ([ReadFile],
// [WriteFile]), an opt-in allow-listed shell ([Shell]), and a vendor-neutral
// web search adapter ([WebSearch]) whose backends (e.g. the tavily extension)
// plug in via the [Searcher] interface. [LoadAgentsMD] collects a workspace's
// AGENTS.md instruction files for an agent's system prompt.
//
// Everything composes with the existing core.Func schema machinery; the module
// adds no dependencies beyond core itself.
//
// # Error, effect, and recovery semantics
//
// Domain validation and ordinary I/O failures are returned as
// [core.ErrorResult] values so the model can adapt. Parent cancellation remains
// fatal. Core never retries tools implicitly; [core.WithToolRetry] is an
// explicit opt-in and must not wrap non-idempotent work without a destination
// idempotency strategy.
//
// ReadFile declares [core.ToolEffectReadOnly]. WriteFile declares
// [core.ToolEffectMutating], reports an [core.EffectApplied] content-digest
// receipt after a successful write, and installs a sandbox-root/path semantic
// guard for durable Runtime execution. A crash after dispatch is still
// uncertain until reconciled; local storage cannot prove what the filesystem
// did.
package tools
