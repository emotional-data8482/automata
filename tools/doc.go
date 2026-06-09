// Package tools provides reusable, zero-key [core.Tool] constructors built on
// the stdlib: web fetching ([HTTPFetch]), sandboxed file access ([ReadFile],
// [WriteFile]), an opt-in allow-listed shell ([Shell]), and a vendor-neutral
// web search adapter ([WebSearch]) whose backends (e.g. the tavily extension)
// plug in via the [Searcher] interface.
//
// Everything composes with the existing core.Func schema machinery; the module
// adds no dependencies beyond core itself.
//
// # Error and retry semantics
//
// Errors returned by these tools follow the [core.Func] contract: the run
// converts them to "error: <msg>" strings the model can recover from. The
// agent loop additionally wraps execution in its retry policy, which by
// default retries only errors implementing retry.Retryable — plain errors
// fail fast. If you build your own tools around non-idempotent operations,
// return plain (non-Retryable) errors so a transient-looking failure is never
// re-executed.
package tools
