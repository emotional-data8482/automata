# Tavily search backend guidance

- Implement the vendor-neutral `tools.Searcher` contract; do not leak Tavily
  response types into `tools` or `core`.
- Validate empty queries and result limits before network requests.
- Preserve source title, URL, and content needed for model citation.
- Bound response handling and classify transient HTTP failures consistently.
- Never require a live API key for unit tests; use an HTTP test server.

Validate with `go test ./...` and, for dependency changes,
`GOWORK=off go test ./...` from this module.
