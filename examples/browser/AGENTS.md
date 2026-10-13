# Browser example guidance

Keep this a small, readable demonstration of `extensions/browser` with a
terminal approval for `browser_request_access`. It must not become a
dependency of library modules.

Validate with `go build ./...`; a live run needs `ANTHROPIC_API_KEY` and an
installed Chrome or Chromium, and is optional unless explicitly requested.
