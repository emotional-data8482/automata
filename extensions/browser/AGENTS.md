# Browser extension guidance

This module drives one Chromium page through go-rod and exposes it as
`core.Tool` values. Keep rod and CDP types inside this module.

- The model-facing contract is the snapshot text format and the ref rules.
  Refs name backend DOM node ids, never restart numbering within a session,
  and are retired when the main frame commits a new document. Golden files in
  `testdata/golden` pin the format; update them with `go test -update` only
  for an intended format change.
- Snapshot pruning (`snapshot.go`) is pure. Cover pruning changes with
  synthetic-tree tests that run without a browser.
- Effect policies are a contract: observation tools are read-only;
  `browser_act` is mutating and must report `EffectNotApplied` only when no
  input was dispatched, `EffectApplied` once input reached the page, and
  `EffectUnknown` only when a dispatch itself failed (Runtime treats Unknown as
  uncertain and stops for reconciliation).
- Navigation fails closed: an empty `AllowedHosts` rejects every URL. Validate
  URLs (http/https only, `AllowedHosts`) before navigation, and keep
  the in-browser document filter in place for redirects and link clicks.
- Never download a browser or use rod's default launcher (it may download an
  unsigned Chromium that macOS Gatekeeper blocks). Launch once per test run
  through the shared process in `browser_test.go`; tests skip with a clear
  message, without retrying, when no Chrome or
  Chromium is installed; `AUTOMATA_BROWSER_PATH` selects one.
- Parent cancellation is fatal; page, action, and capture failures are
  recoverable `ErrorResult`s.

Validate with `go test ./...` and `go test -race ./...`, and with
`GOWORK=off go test ./...` for dependency changes.
