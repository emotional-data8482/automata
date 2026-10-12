# host

`host.Main` is the entry point of a durable agent server. It opens the store
(SQLite by default), registers the definitions stored runs may pin, recovers
work a previous process left, applies retention on an interval, and serves
the [`httpserver`](../extensions/httpserver) API until SIGINT or SIGTERM.
Stopping drains HTTP requests, ends event streams (clients resume from their
cursor), and closes the Runtime at a safe boundary; the next start resumes.

```go
func main() {
	host.Main(host.Options{
		Register: func(rt *core.Runtime) error {
			_, err := rt.Register("support", "2026-10-01", supportAgent())
			return err
		},
		Retention: &core.RetentionPolicy{Events: 30 * 24 * time.Hour},
	})
}
```

Work the host owns beside the API, such as a scheduler that admits runs,
goes in `Options.Background`. It starts after recovery with the Runtime,
its context ends when the host stops, and the host waits for it before
closing the Runtime. An error it returns stops the host.

Unset options come from the environment: `AUTOMATA_ADDR` (default `:8080`),
`AUTOMATA_DB` (default `automata.db`), and `AUTOMATA_TOKEN`, a bearer token
every request must present when `Options.Server.Authorize` is not set. The
host refuses to start with neither. `GET /healthz` answers 204 once the host
serves. Use `host.Run` to embed the same lifecycle under your own context.
Failed startup closes the opened store exactly once, including invalid Runtime
configuration, so an embedded host can retry without leaking SQLite ownership.
