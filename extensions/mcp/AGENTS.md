# MCP extension guidance

This module wraps the official MCP Go SDK
(`github.com/modelcontextprotocol/go-sdk`). `mcpclient` exposes a server's
tools as `core.Tool` values; `mcpserver` registers `core.Tool` values on an SDK
server.

- Let the SDK own transports, JSON-RPC framing, sessions, OAuth, and
  capability negotiation. Do not reimplement protocol behavior here, and do
  not wrap SDK transports that users can construct directly.
- Keep SDK types out of `core`. They may appear in this module's API only
  where the SDK is the configuration surface (transports, server,
  implementation, client options).
- Client error mapping is a contract: `isError` results and server JSON-RPC
  errors are recoverable `ErrorResult`s, context cancellation is returned
  unchanged, and transport failures are fatal Go errors. The SDK reports some
  transport failures as wire errors (`connectionCodes`), so classify by the
  server's answer, not by error type. Never mark a remote call retryable.
- Remote input schemas must lower into core's supported subset. Exact
  rewrites come first; any relaxation must be a strict weakening, because the
  server stays the authoritative validator. When core's schema contract
  (`core/schemacontract.go`) changes, update `droppedKeywords` and add a
  lowering case. Tests register every lowered schema with `core.New`.
- Server calls validate arguments against the tool's schema before
  `Execute`, mirroring Runtime. They bypass Runtime policy (timeouts,
  budgets, approvals, effect guards); keep that documented in the package
  docs.
- Progress bridges both ways through core: the client reports server
  notifications with `core.ReportToolProgress` under the call's context, and
  the server installs `core.WithToolProgress` only when the request carries a
  progress token, stopping it before the handler returns so no notification
  follows the response.
- Unsupported content degrades to a bracketed placeholder line, never
  silently.

Tests use `mcp.NewInMemoryTransports` or a loopback `httptest` server for the
streamable transport; never start subprocesses or contact live services.
Validate with `go test -race ./...` and `GOWORK=off go test ./...` from this
module.
